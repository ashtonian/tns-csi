package driver

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fenio/tns-csi/pkg/fsrepair"
	"github.com/fenio/tns-csi/pkg/mount"
	"k8s.io/klog/v2"
)

// Default cadences for the periodic reconciler loops.
const (
	staleSweepInterval = 60 * time.Second
	bindHealInterval   = 60 * time.Second
)

var errNoMountTable = errors.New("recovery: no readable mount table")

// recoveryReconciler runs node-side recovery loops out-of-band. CSI is
// request-driven: the kubelet only calls the node plugin on stage/publish/
// unstage, never "your mounted filesystem just died". These loops fill that gap
// — reacting to kernel filesystem-shutdown events and to stale mounts — and so
// replace the external recovery DaemonSet. Everything is gated by the recovery
// mode (off/shadow/on) and the cluster kill switch.
type recoveryReconciler struct {
	node         *NodeService
	lastAction   map[string]time.Time
	bindFailures map[string]int
	nodeID       string
	cfg          RecoveryConfig
	mu           sync.Mutex
}

func newRecoveryReconciler(s *NodeService) *recoveryReconciler {
	return &recoveryReconciler{
		node:         s,
		cfg:          s.recovery,
		nodeID:       s.nodeID,
		lastAction:   make(map[string]time.Time),
		bindFailures: make(map[string]int),
	}
}

// run starts the loops and blocks until ctx is canceled.
func (r *recoveryReconciler) run(ctx context.Context) {
	klog.Infof("recovery reconciler starting (mode=%s evict=%s debounce=%d cooldown=%s)",
		r.cfg.Mode, r.cfg.EvictMode, r.cfg.Debounce, r.cfg.Cooldown)
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); r.watchKmsg(ctx) }()
	go func() {
		defer wg.Done()
		r.loop(ctx, "stale-globalmount-sweep", staleSweepInterval, r.sweepStaleGlobalmounts)
	}()
	go func() { defer wg.Done(); r.loop(ctx, "stale-bind-heal", bindHealInterval, r.healStaleBinds) }()
	wg.Wait()
	klog.Infof("recovery reconciler stopped")
}

// loop runs fn on a ticker until ctx is canceled, skipping ticks while the
// cluster kill switch is engaged.
func (r *recoveryReconciler) loop(ctx context.Context, name string, interval time.Duration, fn func(context.Context)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if r.paused(ctx) {
				klog.V(4).Infof("recovery: %s skipped (kill switch engaged)", name)
				continue
			}
			fn(ctx)
		}
	}
}

func (r *recoveryReconciler) paused(ctx context.Context) bool {
	return r.node.kube.RecoveryPaused(ctx)
}

// ---- shutdown watcher (replaces the daemonset's xfs-recovery container) ----

// watchKmsg tails /dev/kmsg for filesystem-shutdown signatures and reacts to
// those affecting volumes this node staged.
func (r *recoveryReconciler) watchKmsg(ctx context.Context) {
	f, err := os.OpenFile("/dev/kmsg", os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		klog.Warningf("recovery: cannot open /dev/kmsg, shutdown watcher disabled: %v", err)
		return
	}
	defer func() { _ = f.Close() }() //nolint:errcheck // best-effort close of /dev/kmsg
	// Skip the historical ring buffer; only react to events from now on.
	_, _ = f.Seek(0, io.SeekEnd) //nolint:errcheck // best-effort seek to tail of kmsg

	buf := make([]byte, 8192)
	for {
		if ctx.Err() != nil {
			return
		}
		n, readErr := f.Read(buf)
		if readErr != nil {
			// EAGAIN (no new record) on a non-blocking fd: wait briefly, honoring ctx.
			if errors.Is(readErr, syscall.EAGAIN) {
				select {
				case <-ctx.Done():
					return
				case <-time.After(500 * time.Millisecond):
				}
				continue
			}
			// EPIPE means records were overwritten before we read them; keep going.
			if errors.Is(readErr, syscall.EPIPE) {
				continue
			}
			klog.Warningf("recovery: /dev/kmsg read error, shutdown watcher stopping: %v", readErr)
			return
		}
		r.handleKmsgLine(ctx, string(buf[:n]))
	}
}

func (r *recoveryReconciler) handleKmsgLine(ctx context.Context, raw string) {
	// /dev/kmsg records are "priority,seq,timestamp,flags;message"; the human
	// text we match against follows the first ';'.
	msg := raw
	if _, after, found := strings.Cut(raw, ";"); found {
		msg = after
	}
	sig, ok := fsrepair.ParseKmsgLine(msg)
	if !ok {
		return
	}
	r.handleShutdown(ctx, sig)
}

func (r *recoveryReconciler) handleShutdown(ctx context.Context, sig fsrepair.ShutdownSignal) {
	// Correlate to a volume we staged. If it is not ours, ignore it entirely —
	// this is the reconciler's primary false-positive guard.
	vol, ok := r.node.tracker.ByDevice(sig.Device)
	if !ok {
		klog.V(4).Infof("recovery: shutdown on %s is not a tracked volume; ignoring", sig.Device)
		return
	}
	now := time.Now()
	if !r.coolDownOK(sig.Device, now) {
		klog.V(4).Infof("recovery: shutdown on %s within cooldown; skipping", sig.Device)
		return
	}
	// Claim the cooldown window immediately: a single filesystem shutdown emits
	// several kernel lines, and without this each one would repeat the PVC
	// resolve + pod-list API calls below.
	r.recordAction(sig.Device, now)

	logf := fmt.Sprintf("recovery[dev=%s vol=%s]", sig.Device, vol.VolumeID)
	klog.Warningf("%s filesystem shutdown detected (corrupt=%v): %s", logf, sig.Corrupt, sig.Raw)

	if r.paused(ctx) {
		klog.Infof("%s kill switch engaged; not acting", logf)
		return
	}
	if !volumeRecoveryEnabled(vol.AutoRepair) {
		klog.V(4).Infof("%s volume opted out of recovery; not acting", logf)
		return
	}

	ref := r.node.resolvePVC(ctx, vol.VolumeID)
	pods := r.node.kube.PodsUsingPVC(ctx, ref)
	if len(pods) == 0 {
		klog.Infof("%s no actionable pods consuming the volume on this node", logf)
		return
	}

	if !r.cfg.Acting() {
		msg := fmt.Sprintf("[shadow] would evict %d pod(s) to trigger repair-on-reschedule after filesystem shutdown on %s", len(pods), sig.Device)
		klog.Infof("%s %s", logf, msg)
		r.node.emitRecoveryEvent(ctx, ref, "Normal", msg)
		return
	}

	reason := "filesystem shutdown on " + sig.Device
	for _, p := range pods {
		r.evict(ctx, p, ref, reason)
	}
}

// ---- stale globalmount sweeper (replaces stale-mount-cleanup) ----

// sweepStaleGlobalmounts lazy-unmounts staging mounts whose backing device has
// disappeared (an ungraceful target loss that never triggers NodeUnstageVolume).
func (r *recoveryReconciler) sweepStaleGlobalmounts(ctx context.Context) {
	for _, vol := range r.node.tracker.List() {
		if vol.DevicePath == "" || vol.StagingPath == "" {
			continue
		}
		if _, err := os.Stat(vol.DevicePath); err == nil {
			continue // device still present
		}
		mounted, err := mount.IsMounted(ctx, vol.StagingPath)
		if err != nil || !mounted {
			continue
		}
		if !r.cfg.Acting() {
			klog.Infof("recovery: [shadow] would lazy-unmount stale globalmount %s (device %s gone)", vol.StagingPath, vol.DevicePath)
			continue
		}
		klog.Warningf("recovery: lazy-unmounting stale globalmount %s (device %s gone)", vol.StagingPath, vol.DevicePath)
		if err := lazyUnmount(ctx, vol.StagingPath); err != nil {
			klog.Warningf("recovery: failed to lazy-unmount %s: %v", vol.StagingPath, err)
		}
	}
}

// ---- stale bind healer (replaces stale-bind-heal) ----

// healStaleBinds evicts pods whose CSI bind mount returns I/O errors while the
// backing globalmount is healthy — the "pod looks Ready but its filesystem is
// dead" case after an NVMe-oF reconnect.
func (r *recoveryReconciler) healStaleBinds(ctx context.Context) {
	mounts, err := readProcMounts()
	if err != nil {
		klog.Warningf("recovery: cannot read mounts for bind healer: %v", err)
		return
	}

	// Healthy globalmount devices for our driver.
	gmHealthy := make(map[string]bool)
	for _, m := range mounts {
		if isOurGlobalmount(m.mountpoint) && mountAlive(m.mountpoint) {
			gmHealthy[m.device] = true
		}
	}

	evicted := 0
	for _, m := range mounts {
		if !isPodBindMount(m.mountpoint) || !isBlockDevice(m.device) {
			continue
		}
		uid := podUIDFromMountPath(m.mountpoint)
		if uid == "" {
			continue
		}
		if mountAlive(m.mountpoint) {
			r.clearBindFailure(uid) // healthy again — reset debounce
			continue
		}
		// Pod bind is broken. Only act if the globalmount itself is healthy
		// (otherwise this is a device-level problem the sweeper/kmsg path owns).
		if !gmHealthy[m.device] {
			continue
		}

		count := r.bumpBindFailure(uid)
		if count < r.cfg.Debounce {
			klog.V(4).Infof("recovery: stale bind for pod uid=%s (%d/%d confirmations)", uid, count, r.cfg.Debounce)
			continue
		}
		if evicted >= r.cfg.MaxEvictions {
			klog.Warningf("recovery: bind-heal eviction rate limit (%d) reached; deferring remaining", r.cfg.MaxEvictions)
			break
		}
		pod, ok := r.node.kube.PodByUID(ctx, uid)
		if !ok {
			r.clearBindFailure(uid)
			continue
		}
		if !r.cfg.Acting() {
			klog.Infof("recovery: [shadow] would evict %s/%s (stale bind mount, %d confirmations)", pod.Namespace, pod.Name, count)
			r.clearBindFailure(uid)
			continue
		}
		r.evict(ctx, pod, pvcRef{}, "stale bind mount (globalmount healthy)")
		r.clearBindFailure(uid)
		evicted++
	}
}

// ---- shared helpers ----

func (r *recoveryReconciler) evict(ctx context.Context, p podRef, ref pvcRef, reason string) {
	klog.Infof("recovery: evicting pod %s/%s via %s (%s)", p.Namespace, p.Name, r.cfg.EvictMode, reason)
	if err := r.node.kube.EvictPod(ctx, p.Namespace, p.Name, r.cfg.EvictMode); err != nil {
		klog.Warningf("recovery: failed to evict %s/%s: %v", p.Namespace, p.Name, err)
		r.node.emitRecoveryEvent(ctx, ref, "Warning", fmt.Sprintf("Failed to evict pod %s/%s after %s: %v", p.Namespace, p.Name, reason, err))
		return
	}
	r.node.emitRecoveryEvent(ctx, ref, "Normal", fmt.Sprintf("Evicted pod %s/%s to recover from %s", p.Namespace, p.Name, reason))
}

func (r *recoveryReconciler) coolDownOK(dev string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if last, ok := r.lastAction[dev]; ok && now.Sub(last) < r.cfg.Cooldown {
		return false
	}
	return true
}

func (r *recoveryReconciler) recordAction(dev string, now time.Time) {
	r.mu.Lock()
	r.lastAction[dev] = now
	r.mu.Unlock()
}

func (r *recoveryReconciler) bumpBindFailure(uid string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bindFailures[uid]++
	return r.bindFailures[uid]
}

func (r *recoveryReconciler) clearBindFailure(uid string) {
	r.mu.Lock()
	delete(r.bindFailures, uid)
	r.mu.Unlock()
}

// procMount is one parsed line of /proc/mounts.
type procMount struct {
	device     string
	mountpoint string
	fstype     string
}

// readProcMounts reads the host mount table. It prefers /proc/1/mounts (the host
// init's view, available via hostPID) and falls back to /proc/mounts.
func readProcMounts() ([]procMount, error) {
	for _, path := range []string{"/proc/1/mounts", "/proc/mounts"} {
		f, err := os.Open(path) //nolint:gosec // fixed allowlist of mount-table paths, not user input
		if err != nil {
			continue
		}
		mounts := parseProcMounts(f)
		_ = f.Close() //nolint:errcheck // best-effort close of read-only mount table
		return mounts, nil
	}
	return nil, errNoMountTable
}

func parseProcMounts(r io.Reader) []procMount {
	var out []procMount
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 {
			continue
		}
		out = append(out, procMount{device: fields[0], mountpoint: fields[1], fstype: fields[2]})
	}
	return out
}

// lazyUnmount detaches a mount even if it is busy (umount -l / MNT_DETACH), then
// the kernel cleans up references as they close.
func lazyUnmount(ctx context.Context, path string) error {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "umount", "-l", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("umount -l %s: %w (%s)", path, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// mountAlive reports whether a mount point is readable. It opens the directory
// and forces a real read of its contents, so a mount whose backing device has
// died returns EIO here — which a bare stat can miss when the root inode is
// still cached. An empty-but-healthy directory (io.EOF) counts as alive.
func mountAlive(path string) bool {
	f, err := os.Open(path) //nolint:gosec // path is a mountpoint from our own /proc parse, not user input
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }() //nolint:errcheck // best-effort close of probe handle
	_, err = f.ReadDir(1)
	return err == nil || errors.Is(err, io.EOF)
}

func isBlockDevice(device string) bool {
	return strings.HasPrefix(device, "/dev/nvme") || strings.HasPrefix(device, "/dev/sd") || strings.HasPrefix(device, "/dev/dm-")
}

// isOurGlobalmount matches a CSI globalmount path belonging to this driver. The
// driver name appears in the kubelet CSI path (.../csi/tns.csi.io/<hash>/globalmount).
func isOurGlobalmount(mountpoint string) bool {
	return strings.Contains(mountpoint, "tns.csi.io") && strings.HasSuffix(mountpoint, "/globalmount")
}

// isPodBindMount matches a per-pod CSI bind mount (not the shared globalmount).
func isPodBindMount(mountpoint string) bool {
	return strings.Contains(mountpoint, "kubernetes.io~csi/") && !strings.HasSuffix(mountpoint, "/globalmount")
}

var podUIDRe = regexp.MustCompile(`/pods/([^/]+)/`)

// podUIDFromMountPath extracts the pod UID from a kubelet pod mount path like
// /var/lib/kubelet/pods/<uid>/volumes/kubernetes.io~csi/<pv>/mount.
func podUIDFromMountPath(mountpoint string) string {
	if m := podUIDRe.FindStringSubmatch(mountpoint); m != nil {
		return m[1]
	}
	return ""
}
