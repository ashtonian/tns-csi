package driver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/fenio/tns-csi/pkg/fsrepair"
	"github.com/fenio/tns-csi/pkg/tnsapi"
	"k8s.io/klog/v2"
)

// recoveryEventReason is the Reason on Kubernetes Events emitted by recovery.
const recoveryEventReason = "FilesystemAutoRecovery"

var (
	errNoAPIClient    = errors.New("recovery: no TrueNAS API client available")
	errUnknownDataset = errors.New("recovery: unknown backing dataset for volume")
)

// circuitBreaker bounds how many recovery attempts may occur for a key (device)
// within a rolling window, so a flapping device or a false-positive loop cannot
// drive repeated repairs. It mirrors the external daemon's per-device breaker.
type circuitBreaker struct {
	entries  map[string]*breakerEntry
	window   time.Duration
	maxFails int
	mu       sync.Mutex
}

type breakerEntry struct {
	first time.Time
	count int
}

func newCircuitBreaker(window time.Duration, maxFails int) *circuitBreaker {
	return &circuitBreaker{
		window:   window,
		maxFails: maxFails,
		entries:  make(map[string]*breakerEntry),
	}
}

// blocked reports whether the breaker is open for key — i.e. maxFails *failed*
// recovery attempts have occurred within the rolling window. It does not modify
// state, so checking the breaker never consumes a slot. now is passed for
// testability.
func (b *circuitBreaker) blocked(key string, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.entries[key]
	if e == nil || now.Sub(e.first) > b.window {
		return false
	}
	return e.count >= b.maxFails
}

// recordFailure counts one failed recovery attempt for key, starting a fresh
// window if none is open or the previous one has elapsed.
func (b *circuitBreaker) recordFailure(key string, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.entries[key]
	if e == nil || now.Sub(e.first) > b.window {
		b.entries[key] = &breakerEntry{count: 1, first: now}
		return
	}
	e.count++
}

// recordSuccess clears the failure count for key: a successful recovery means
// the device is healthy again, so repeated successful recoveries (a flapping but
// recoverable transport link) must never trip the breaker.
func (b *circuitBreaker) recordSuccess(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.entries, key)
}

// recoverParams carries everything recoverAndRetryMount needs to attempt
// recovery of a block-device mount failure.
type recoverParams struct {
	remount     func(ctx context.Context) (string, error)
	volumeID    string
	devicePath  string
	stagingPath string
	fsType      string
	datasetName string
	protocol    string
	autoRepair  string
	mountOutput string
}

// recoverAndRetryMount attempts to recover a block volume whose filesystem the
// kernel shut down after a transport interruption, then remount it. It returns a
// non-nil response only if the device is mounted afterward; a nil response means
// recovery did not apply or did not succeed, and the caller surfaces the
// original mount error unchanged.
//
// The path is defensive by construction: it engages only on a shutdown-like
// failure, mutates a filesystem only after an independent read-only check
// confirms inconsistencies, only when the relevant opt-in flags are set, and
// only within the per-device circuit breaker. In shadow mode it decides and
// reports but never acts.
func (s *NodeService) recoverAndRetryMount(ctx context.Context, p recoverParams) (resp *csi.NodeStageVolumeResponse) {
	if !s.recovery.Enabled() || !volumeRecoveryEnabled(p.autoRepair) {
		return nil
	}

	dev := baseDeviceName(p.devicePath)
	ref := s.resolvePVC(ctx, p.volumeID)
	logPrefix := fmt.Sprintf("recovery[dev=%s vol=%s fs=%s]", dev, p.volumeID, p.fsType)

	// Gate 1: only engage on a shutdown/structure-like mount failure — not a
	// missing device, wrong fstype, or the size-mismatch refusal upstream.
	if !fsrepair.MountErrorIndicatesShutdown(p.mountOutput) {
		klog.V(4).Infof("%s mount failure is not a recognized shutdown signature; not recovering (output=%q)",
			logPrefix, strings.TrimSpace(p.mountOutput))
		return nil
	}

	repairer, err := fsrepair.For(p.fsType)
	if err != nil {
		klog.Warningf("%s no repairer for filesystem: %v", logPrefix, err)
		return nil
	}

	// The circuit breaker and the actions below apply only in acting mode. Shadow
	// mode must observe (run the read-only check, report what it would do) without
	// ever changing the stage outcome or consuming breaker state.
	acting := s.recovery.Acting()
	if acting && s.breaker != nil {
		// Gate 2: per-device circuit breaker. Checked WITHOUT consuming a slot —
		// only *failed* recoveries count, recorded in the deferred accounting
		// below. A successful recovery resets the count, so a flapping-but-
		// recoverable link never trips the breaker.
		if s.breaker.blocked(dev, time.Now()) {
			msg := fmt.Sprintf("device %s exceeded %d failed recovery attempts within %s — manual intervention required",
				dev, s.recovery.MaxRetries, s.recovery.RetryWindow)
			klog.Errorf("%s %s", logPrefix, msg)
			s.emitRecoveryEvent(ctx, ref, "Warning", msg)
			return nil
		}
		defer func() {
			if resp != nil {
				s.breaker.recordSuccess(dev)
			} else {
				s.breaker.recordFailure(dev, time.Now())
			}
		}()
	}

	// Step 1: a plain remount (fresh kernel log replay) clears the overwhelmingly
	// common transport-drop case at zero risk. Acting-only: a successful remount
	// changes the stage outcome, which shadow mode must not do.
	if acting {
		out, remErr := p.remount(ctx)
		if remErr == nil {
			msg := fmt.Sprintf("Recovered %s by remount (kernel log replay) after filesystem shutdown", dev)
			klog.Infof("%s %s", logPrefix, msg)
			s.emitRecoveryEvent(ctx, ref, "Normal", msg)
			return &csi.NodeStageVolumeResponse{}
		}
		klog.V(4).Infof("%s remount still failing: %v (out=%q)", logPrefix, remErr, strings.TrimSpace(out))
	}

	// Step 2: read-only check — the authority on whether this is real on-disk
	// corruption. A clean result means "not corruption": never mutate.
	checkCtx, cancel := context.WithTimeout(ctx, s.recovery.RepairTimeout)
	check, checkErr := repairer.Check(checkCtx, p.devicePath)
	cancel()
	if checkErr != nil {
		klog.Warningf("%s read-only check failed: %v", logPrefix, checkErr)
		return nil
	}
	if check.Outcome == fsrepair.OutcomeClean {
		msg := fmt.Sprintf("mount of %s failed but %s -n reports the filesystem clean; not repairing (likely transient/transport). Manual investigation may be warranted.",
			dev, check.Tool)
		klog.Warningf("%s %s", logPrefix, msg)
		s.emitRecoveryEvent(ctx, ref, "Warning", msg)
		return nil
	}

	// Shadow mode: the decision logic has concluded it WOULD remount and, if that
	// failed, repair. Report and stop without changing the stage outcome.
	if !acting {
		msg := fmt.Sprintf("[shadow] would remount and, if needed, repair %s (%s check outcome=%s). Set --auto-recovery=on to act.",
			dev, p.fsType, check.Outcome)
		klog.Infof("%s %s", logPrefix, msg)
		s.emitRecoveryEvent(ctx, ref, "Normal", msg)
		return nil
	}

	// Acting. Non-destructive repair must be explicitly enabled.
	if !s.recovery.Repair {
		msg := fmt.Sprintf("%s needs repair (check=%s) but --auto-recovery-repair is disabled; not repairing", dev, check.Outcome)
		klog.Warningf("%s %s", logPrefix, msg)
		s.emitRecoveryEvent(ctx, ref, "Warning", msg)
		return nil
	}

	// Snapshot before mutating, if requested. If the operator asked for a
	// snapshot and we cannot take one, do NOT mutate — the snapshot is the
	// rollback they asked for.
	if s.recovery.Snapshot {
		if snapErr := s.snapshotBeforeRepair(ctx, p); snapErr != nil {
			msg := fmt.Sprintf("%s pre-repair snapshot failed: %v; aborting repair to preserve rollback safety", dev, snapErr)
			klog.Errorf("%s %s", logPrefix, msg)
			s.emitRecoveryEvent(ctx, ref, "Warning", msg)
			return nil
		}
	}

	repairCtx, rcancel := context.WithTimeout(ctx, s.recovery.RepairTimeout)
	repair, repErr := repairer.Repair(repairCtx, p.devicePath, s.recovery.RepairDestructive)
	rcancel()
	if repErr != nil {
		klog.Errorf("%s repair error: %v", logPrefix, repErr)
		s.emitRecoveryEvent(ctx, ref, "Warning", fmt.Sprintf("Repair of %s errored: %v", dev, repErr))
		return nil
	}

	switch repair.Outcome {
	case fsrepair.OutcomeRepaired, fsrepair.OutcomeRepairedDestructive:
		out, remErr := p.remount(ctx)
		if remErr == nil {
			destructive := repair.Outcome == fsrepair.OutcomeRepairedDestructive
			msg := fmt.Sprintf("Repaired %s with %s (destructive=%v) and remounted after filesystem shutdown",
				dev, repair.Tool, destructive)
			klog.Infof("%s %s", logPrefix, msg)
			s.emitRecoveryEvent(ctx, ref, "Normal", msg)
			return &csi.NodeStageVolumeResponse{}
		}
		msg := fmt.Sprintf("%s repaired (%s) but remount still failed: %v (out=%q)", dev, repair.Tool, remErr, strings.TrimSpace(out))
		klog.Errorf("%s %s", logPrefix, msg)
		s.emitRecoveryEvent(ctx, ref, "Warning", msg)
		return nil
	case fsrepair.OutcomeNeedsDestructive:
		msg := dev + " requires destructive repair (e.g. log zeroing) which is disabled (--auto-recovery-repair-destructive=false). Manual action required."
		klog.Warningf("%s %s", logPrefix, msg)
		s.emitRecoveryEvent(ctx, ref, "Warning", msg)
		return nil
	default:
		msg := fmt.Sprintf("%s repair did not succeed (outcome=%s). Manual action required.", dev, repair.Outcome)
		klog.Errorf("%s %s", logPrefix, msg)
		s.emitRecoveryEvent(ctx, ref, "Warning", msg)
		return nil
	}
}

// volumeRecoveryEnabled applies the per-volume override surfaced via volume
// context. Empty (unset) follows the node-global mode; an explicit "false"
// opts the volume out.
func volumeRecoveryEnabled(autoRepair string) bool {
	switch strings.ToLower(strings.TrimSpace(autoRepair)) {
	case "false", "off", "disabled", "no":
		return false
	default:
		return true
	}
}

// snapshotBeforeRepair takes a ZFS snapshot of the backing zvol via the TrueNAS
// WebSocket client the driver already holds — no new boundary. Returns an error
// if the snapshot could not be taken so the caller can decide whether to proceed.
func (s *NodeService) snapshotBeforeRepair(ctx context.Context, p recoverParams) error {
	if s.apiClient == nil {
		return errNoAPIClient
	}
	if p.datasetName == "" {
		return fmt.Errorf("%w %s", errUnknownDataset, p.volumeID)
	}
	name := fmt.Sprintf("tns-csi-autorepair-%d", time.Now().Unix())
	snapCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := s.apiClient.CreateSnapshot(snapCtx, tnsapi.SnapshotCreateParams{Dataset: p.datasetName, Name: name}); err != nil {
		return err
	}
	klog.Infof("recovery: snapshot %s@%s taken before repair of volume %s", p.datasetName, name, p.volumeID)
	return nil
}

// resolvePVC looks up the PVC for a volume, best-effort.
func (s *NodeService) resolvePVC(ctx context.Context, volumeID string) pvcRef {
	if s.kube == nil {
		return pvcRef{}
	}
	return s.kube.ResolvePVCForVolume(ctx, volumeID)
}

// emitRecoveryEvent emits a Kubernetes Event on the PVC, best-effort.
func (s *NodeService) emitRecoveryEvent(ctx context.Context, ref pvcRef, eventType, message string) {
	if s.kube == nil {
		return
	}
	s.kube.EmitEvent(ctx, ref, eventType, recoveryEventReason, message)
}

// trackStagedVolume records a successfully staged block volume for the
// reconciler. No-op when recovery (and thus the tracker) is disabled.
func (s *NodeService) trackStagedVolume(volumeID, stagingPath, devicePath, fsType, protocol string, volumeContext map[string]string) {
	if s.tracker == nil {
		return
	}
	s.tracker.Add(stagedVolume{
		VolumeID:    volumeID,
		StagingPath: stagingPath,
		DevicePath:  devicePath,
		FSType:      fsType,
		Protocol:    protocol,
		AutoRepair:  volumeContext[VolumeContextKeyAutoRepair],
	})
}
