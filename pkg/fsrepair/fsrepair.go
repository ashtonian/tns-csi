// Package fsrepair provides filesystem-aware, non-destructive-first repair of
// block-device filesystems that the Linux kernel has shut down (XFS) or
// remounted read-only (ext4) after a transport interruption.
//
// It is used by the TrueNAS CSI node plugin to recover NVMe-oF/iSCSI volumes
// whose initiator-side filesystem entered a shutdown state. On this class of
// hardware these events are almost always caused by a transport drop rather
// than genuine on-disk corruption: the on-disk metadata is intact, only the
// in-memory state diverged during the disrupted write window. Repair runs
// against the local initiator block device (e.g. /dev/nvme0n1), which holds
// the same on-disk metadata as the backing zvol, while the device is connected
// but unmounted — the window CSI NodeStageVolume hands us for free.
//
// The package is deliberately free of CSI/Kubernetes dependencies so it can be
// unit-tested without root: command construction and exit-code/output
// classification are the testable surface.
package fsrepair

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Outcome classifies the result of a Check or Repair operation.
type Outcome string

const (
	// OutcomeClean means a read-only check found no inconsistencies. The
	// filesystem is structurally sound, so a mount failure was NOT caused by
	// on-disk corruption and must not be "repaired". This is the central
	// false-positive guard: callers must stop here rather than mutate.
	OutcomeClean Outcome = "clean"
	// OutcomeDirty means a read-only check found inconsistencies that a repair
	// should be able to fix. The caller may proceed to a non-destructive Repair.
	OutcomeDirty Outcome = "dirty"
	// OutcomeRepaired means a non-destructive repair completed successfully.
	OutcomeRepaired Outcome = "repaired"
	// OutcomeRepairedDestructive means a repair completed but required a
	// data-losing operation (XFS log zeroing via -L, or ext fsck -y).
	OutcomeRepairedDestructive Outcome = "repaired_destructive"
	// OutcomeNeedsDestructive means the filesystem cannot be brought back
	// without a data-losing operation, and destructive repair was not allowed.
	// The caller must consult policy / a human before proceeding.
	OutcomeNeedsDestructive Outcome = "needs_destructive"
	// OutcomeFailed means the operation could not complete (tool missing,
	// timed out, or an operational error).
	OutcomeFailed Outcome = "failed"
)

// Result carries the outcome of an operation plus the full tool invocation and
// output, for audit (Kubernetes Events, structured logs).
type Result struct {
	Outcome  Outcome
	Tool     string
	Output   string
	Args     []string
	ExitCode int
	Duration time.Duration
}

// ErrUnsupportedFS is returned when no repairer is registered for a filesystem.
var ErrUnsupportedFS = errors.New("fsrepair: unsupported filesystem type")

// Repairer knows how to check and repair a single filesystem family.
//
// Check never modifies the device. Repair refuses data-losing operations
// unless allowDestructive is true, returning OutcomeNeedsDestructive instead.
type Repairer interface {
	// FSType returns the canonical filesystem family this repairer handles.
	FSType() string
	// Check runs a read-only consistency check.
	Check(ctx context.Context, devicePath string) (Result, error)
	// Repair attempts to repair the filesystem. When allowDestructive is false
	// it stops before any data-losing step and returns OutcomeNeedsDestructive.
	Repair(ctx context.Context, devicePath string, allowDestructive bool) (Result, error)
}

// cmdRunner runs a filesystem tool and returns its combined output and exit
// code. It is a seam: production code uses runFSCmd; tests inject a fake so the
// exit-code/output classification can be exercised without the real binaries.
type cmdRunner func(ctx context.Context, name string, args ...string) (output string, exitCode int, err error)

// For returns a Repairer for the given filesystem type (as reported by blkid /
// the CSI volume capability), or ErrUnsupportedFS.
func For(fsType string) (Repairer, error) {
	switch fsType {
	case "xfs":
		return xfsRepairer{run: runFSCmd}, nil
	case "ext2", "ext3", "ext4":
		return extRepairer{fsType: fsType, run: runFSCmd}, nil
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedFS, fsType)
	}
}

// runFSCmd runs a filesystem tool and separates "command ran and exited
// non-zero" (a normal, classifiable result) from "command could not run"
// (a Go error). A non-zero exit is NOT returned as an error.
func runFSCmd(ctx context.Context, name string, args ...string) (output string, exitCode int, err error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, runErr := cmd.CombinedOutput()
	output = string(out)

	if runErr == nil {
		return output, 0, nil
	}
	// Context deadline/cancel takes precedence over the process error.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return output, -1, ctxErr
	}
	var ee *exec.ExitError
	if errors.As(runErr, &ee) {
		// The command ran and exited non-zero — a classifiable result, not an error.
		return output, ee.ExitCode(), nil
	}
	// The command could not be started (e.g. binary not present).
	return output, -1, runErr
}

// ---- XFS ----

type xfsRepairer struct{ run cmdRunner }

func (r xfsRepairer) runner() cmdRunner {
	if r.run != nil {
		return r.run
	}
	return runFSCmd
}

func (xfsRepairer) FSType() string { return "xfs" }

// xfsDirtyLogMarkers are phrases xfs_repair prints when the on-disk log is dirty
// and cannot be replayed without either mounting the filesystem or zeroing the
// log with -L (the data-losing escalation). Matched against lower-cased output,
// so the markers themselves must be lower-case (note "-L" -> "-l").
var xfsDirtyLogMarkers = []string{
	"-l option", // "...use the -L option to destroy the log..."
	"destroy the log",
	"to be replayed",
	"zero the log",
}

func xfsNeedsLogZero(output string) bool {
	lower := strings.ToLower(output)
	for _, m := range xfsDirtyLogMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

func (r xfsRepairer) Check(ctx context.Context, devicePath string) (Result, error) {
	args := []string{"-n", devicePath}
	start := time.Now()
	out, code, err := r.runner()(ctx, "xfs_repair", args...)
	res := Result{Tool: "xfs_repair", Args: args, Output: out, ExitCode: code, Duration: time.Since(start)}
	if err != nil {
		res.Outcome = OutcomeFailed
		return res, fmt.Errorf("xfs_repair -n %s: %w", devicePath, err)
	}
	switch {
	case code == 0:
		res.Outcome = OutcomeClean
	case xfsNeedsLogZero(out):
		// A dirty log that no-modify mode cannot resolve; only -L (or a
		// successful mount/log-replay) can. Flag it for the destructive gate.
		res.Outcome = OutcomeNeedsDestructive
	default:
		res.Outcome = OutcomeDirty
	}
	return res, nil
}

func (r xfsRepairer) Repair(ctx context.Context, devicePath string, allowDestructive bool) (Result, error) {
	// First attempt: a normal repair, which replays a clean log and fixes
	// on-disk inconsistencies without losing data.
	args := []string{devicePath}
	start := time.Now()
	out, code, err := r.runner()(ctx, "xfs_repair", args...)
	res := Result{Tool: "xfs_repair", Args: args, Output: out, ExitCode: code, Duration: time.Since(start)}
	if err != nil {
		res.Outcome = OutcomeFailed
		return res, fmt.Errorf("xfs_repair %s: %w", devicePath, err)
	}
	if code == 0 {
		res.Outcome = OutcomeRepaired
		return res, nil
	}
	if !xfsNeedsLogZero(out) {
		// Failed for some reason other than a dirty log — not something log
		// zeroing would fix.
		res.Outcome = OutcomeFailed
		return res, nil
	}
	if !allowDestructive {
		res.Outcome = OutcomeNeedsDestructive
		return res, nil
	}

	// Destructive escalation: zero the log. This discards any unreplayed
	// transactions and is gated by the caller's explicit opt-in.
	dargs := []string{"-L", devicePath}
	dstart := time.Now()
	dout, dcode, derr := r.runner()(ctx, "xfs_repair", dargs...)
	dres := Result{Tool: "xfs_repair", Args: dargs, Output: out + "\n" + dout, ExitCode: dcode, Duration: time.Since(dstart)}
	if derr != nil {
		dres.Outcome = OutcomeFailed
		return dres, fmt.Errorf("xfs_repair -L %s: %w", devicePath, derr)
	}
	if dcode == 0 {
		dres.Outcome = OutcomeRepairedDestructive
	} else {
		dres.Outcome = OutcomeFailed
	}
	return dres, nil
}

// ---- ext2/3/4 ----

type extRepairer struct {
	run    cmdRunner
	fsType string
}

func (e extRepairer) runner() cmdRunner {
	if e.run != nil {
		return e.run
	}
	return runFSCmd
}

func (e extRepairer) FSType() string { return e.fsType }

// e2fsck exit codes are a bitmask (see e2fsck(8)):
//
//	1  errors corrected
//	2  errors corrected, system should be rebooted
//	4  errors left uncorrected
//	8  operational error
//	16 usage or syntax error
//	32 checking canceled by user request
//	128 shared library error
const (
	e2fsckErrCorrected   = 1
	e2fsckErrReboot      = 2
	e2fsckErrUncorrected = 4
	e2fsckErrOperational = 8 | 16 | 32 | 128
)

func (e extRepairer) Check(ctx context.Context, devicePath string) (Result, error) {
	args := []string{"-f", "-n", devicePath}
	start := time.Now()
	out, code, err := e.runner()(ctx, "e2fsck", args...)
	res := Result{Tool: "e2fsck", Args: args, Output: out, ExitCode: code, Duration: time.Since(start)}
	if err != nil {
		res.Outcome = OutcomeFailed
		return res, fmt.Errorf("e2fsck -fn %s: %w", devicePath, err)
	}
	switch {
	case code == 0:
		res.Outcome = OutcomeClean
	case code&e2fsckErrOperational != 0:
		res.Outcome = OutcomeFailed
	case code&e2fsckErrUncorrected != 0:
		res.Outcome = OutcomeDirty
	default:
		// In no-modify mode any nonzero-but-non-operational code reflects
		// findings; treat as dirty so the caller can attempt a real repair.
		res.Outcome = OutcomeDirty
	}
	return res, nil
}

func (e extRepairer) Repair(ctx context.Context, devicePath string, allowDestructive bool) (Result, error) {
	// Preen: automatically fix only the problems that are safe to fix
	// unattended. Anything requiring judgement leaves the "uncorrected" bit set.
	args := []string{"-p", devicePath}
	start := time.Now()
	out, code, err := e.runner()(ctx, "e2fsck", args...)
	res := Result{Tool: "e2fsck", Args: args, Output: out, ExitCode: code, Duration: time.Since(start)}
	if err != nil {
		res.Outcome = OutcomeFailed
		return res, fmt.Errorf("e2fsck -p %s: %w", devicePath, err)
	}
	switch {
	case code == 0 || code == e2fsckErrCorrected || code == e2fsckErrReboot:
		res.Outcome = OutcomeRepaired
		return res, nil
	case code&e2fsckErrOperational != 0:
		res.Outcome = OutcomeFailed
		return res, nil
	}
	// Preen left errors uncorrected — they need an unattended "yes to all" pass.
	if !allowDestructive {
		res.Outcome = OutcomeNeedsDestructive
		return res, nil
	}

	dargs := []string{"-f", "-y", devicePath}
	dstart := time.Now()
	dout, dcode, derr := e.runner()(ctx, "e2fsck", dargs...)
	dres := Result{Tool: "e2fsck", Args: dargs, Output: out + "\n" + dout, ExitCode: dcode, Duration: time.Since(dstart)}
	if derr != nil {
		dres.Outcome = OutcomeFailed
		return dres, fmt.Errorf("e2fsck -fy %s: %w", devicePath, derr)
	}
	if dcode == 0 || dcode == e2fsckErrCorrected || dcode == e2fsckErrReboot {
		dres.Outcome = OutcomeRepairedDestructive
	} else {
		dres.Outcome = OutcomeFailed
	}
	return dres, nil
}
