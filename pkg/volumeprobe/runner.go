package volumeprobe

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/fenio/tns-csi/pkg/cmdexec"
	"golang.org/x/sys/unix"
	"k8s.io/klog/v2"
)

// Runner shares persistent flock leases with helper processes. Never unlink lease files.
type Runner struct {
	executable  func() (string, error)
	environment func() []string
	kill        func(*exec.Cmd) error
	Directory   string
	timeout     time.Duration
}

// Probe starts at most four concurrent helpers, with at most one per volume.
// A killed D-state child retains inherited locks until it actually exits; a new
// plugin instance observes those same locks on the persistent host directory.
func (r *Runner) Probe(ctx context.Context, volumeID string, request Request) (Result, error) {
	var result Result
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if volumeID == "" {
		return result, ErrProtocol
	}
	data, err := json.Marshal(request)
	if err != nil || len(data) > maxRequest {
		return result, ErrProtocol
	}
	global, volume, err := r.acquire(volumeID)
	if err != nil {
		return result, err
	}
	defer closeLease(global)
	defer closeLease(volume)
	executable := r.executable
	if executable == nil {
		executable = os.Executable
	}
	binary, err := executable()
	if err != nil {
		return result, err
	}
	timeout := r.timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := cmdexec.CommandContext(probeCtx, binary, HelperArgument)
	cmd.Env = probeEnvironment()
	if r.environment != nil {
		cmd.Env = r.environment()
	}
	cmd.ExtraFiles = []*os.File{global, volume}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if r.kill != nil {
			return r.kill(cmd)
		}
		return unix.Kill(-cmd.Process.Pid, unix.SIGKILL)
	}
	cmd.Stdin = bytes.NewReader(data)
	var output limitedOutput
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		return result, err
	}
	// Waiting may outlive an RPC if the kernel cannot reap a D-state task.
	// Inherited locks, not this goroutine or the RPC deadline, control admission.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-probeCtx.Done():
		return result, probeCtx.Err()
	case waitErr := <-done:
		if probeCtx.Err() != nil {
			return result, probeCtx.Err()
		}
		if waitErr != nil {
			return result, fmt.Errorf("filesystem probe helper failed: %w", waitErr)
		}
	}
	response, overflow := output.contents()
	if overflow || decodeResponse(response, &result) != nil || result.Version != 1 {
		return Result{}, ErrProtocol
	}
	switch result.Code {
	case OK, IOFailure, Unsupported, Unknown, IdentityChanged, Missing, TransportUnavailable:
		return result, nil
	default:
		return Result{}, ErrProtocol
	}
}

func decodeResponse(data []byte, result *Result) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(result); err != nil {
		return ErrProtocol
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return ErrProtocol
	}
	return nil
}

func (r *Runner) acquire(volumeID string) (globalLease, volumeLease *os.File, resultErr error) {
	if err := os.MkdirAll(r.Directory, 0o700); err != nil {
		return nil, nil, err
	}
	root, err := os.OpenRoot(r.Directory)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if closeErr := root.Close(); closeErr != nil {
			klog.Warningf("Closing filesystem probe lease directory: %v", closeErr)
		}
	}()
	sum := sha256.Sum256([]byte(volumeID))
	volume, err := lockLease(root, "volume-"+hex.EncodeToString(sum[:])+".lock")
	if err != nil {
		return nil, nil, err
	}
	for index := range 4 {
		global, lockErr := lockLease(root, fmt.Sprintf("global-%d.lock", index))
		if lockErr == nil {
			return global, volume, nil
		}
		if !errors.Is(lockErr, ErrBusy) {
			closeLease(volume)
			return nil, nil, lockErr
		}
	}
	closeLease(volume)
	return nil, nil, ErrBusy
}

func lockLease(root *os.Root, name string) (*os.File, error) {
	file, err := root.OpenFile(name, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		closeLease(file)
		return nil, ErrProtocol
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		closeLease(file)
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, ErrBusy
		}
		return nil, err
	}
	return file, nil
}

func closeLease(file *os.File) {
	// Close never unlocks a separate inherited copy while the helper still exists.
	if err := file.Close(); err != nil {
		klog.Warningf("Closing filesystem probe lease: %v", err)
	}
}

func probeEnvironment() []string {
	return []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C", "LANG=C"}
}
