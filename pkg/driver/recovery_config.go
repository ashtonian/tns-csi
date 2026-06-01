package driver

import (
	"errors"
	"fmt"
	"time"
)

var errInvalidRecoveryMode = errors.New("invalid recovery mode")

// RecoveryMode is the master switch for filesystem auto-recovery.
type RecoveryMode string

const (
	// RecoveryOff disables auto-recovery entirely. This is the default; the
	// driver behaves exactly as it did before the feature existed.
	RecoveryOff RecoveryMode = "off"
	// RecoveryShadow runs the full detection and decision logic and emits
	// logs/Events describing the action it *would* take, but takes no action.
	// Used to validate the decision logic against reality before enabling.
	RecoveryShadow RecoveryMode = "shadow"
	// RecoveryOn enables auto-recovery to take action.
	RecoveryOn RecoveryMode = "on"
)

// ParseRecoveryMode validates and parses a recovery mode string.
func ParseRecoveryMode(s string) (RecoveryMode, error) {
	switch RecoveryMode(s) {
	case RecoveryOff, RecoveryShadow, RecoveryOn:
		return RecoveryMode(s), nil
	default:
		return RecoveryOff, fmt.Errorf("%w %q (want off|shadow|on)", errInvalidRecoveryMode, s)
	}
}

// Evict modes for the mid-flight reconciler.
const (
	EvictModeEviction = "evict"  // use the Eviction API (respects PodDisruptionBudgets)
	EvictModeDelete   = "delete" // raw pod delete
)

// RecoveryConfig controls filesystem auto-recovery for block volumes (NVMe-oF,
// iSCSI). Every field defaults to off/safe: with zero overrides the driver does
// not change behavior.
type RecoveryConfig struct {
	// Mode is the master switch: off | shadow | on.
	Mode RecoveryMode
	// Repair allows non-destructive repair at stage time (xfs_repair clean-log,
	// e2fsck -p) after a read-only check confirms inconsistencies.
	Repair bool
	// RepairDestructive is a separate gate for the data-losing last resort
	// (xfs_repair -L, e2fsck -fy). Has no effect unless Repair is also set.
	RepairDestructive bool
	// Snapshot takes a ZFS snapshot of the backing zvol before any mutating
	// repair, via the TrueNAS WebSocket client the driver already holds.
	Snapshot bool

	// EvictMode selects how the reconciler removes a pod: "evict" | "delete".
	EvictMode string
	// Debounce is the number of consecutive confirmations the reconciler
	// requires before evicting/repairing.
	Debounce int
	// Cooldown is the minimum time between recovery actions for one device.
	Cooldown time.Duration
	// MaxEvictions caps evictions per node per RetryWindow.
	MaxEvictions int
	// RepairTimeout bounds a single repair invocation.
	RepairTimeout time.Duration

	// RetryWindow and MaxRetries bound the per-device failure limiter: after
	// MaxRetries failed recovery attempts within RetryWindow, recovery stops for
	// that device until the window elapses or a recovery succeeds.
	RetryWindow time.Duration
	MaxRetries  int
}

// DefaultRecoveryConfig returns the safe defaults (recovery off).
func DefaultRecoveryConfig() RecoveryConfig {
	return RecoveryConfig{
		Mode:              RecoveryOff,
		Repair:            false,
		RepairDestructive: false,
		Snapshot:          true,
		EvictMode:         EvictModeEviction,
		Debounce:          3,
		Cooldown:          300 * time.Second,
		MaxEvictions:      5,
		RepairTimeout:     10 * time.Minute,
		RetryWindow:       time.Hour,
		MaxRetries:        3,
	}
}

// Enabled reports whether recovery should run at all (shadow or on).
func (c RecoveryConfig) Enabled() bool {
	return c.Mode == RecoveryShadow || c.Mode == RecoveryOn
}

// Acting reports whether recovery should take real action (on, not shadow).
func (c RecoveryConfig) Acting() bool {
	return c.Mode == RecoveryOn
}
