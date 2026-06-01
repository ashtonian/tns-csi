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
// not change behavior. See docs/RFC-AUTO-RECOVERY.md.
type RecoveryConfig struct {
	Mode              RecoveryMode
	EvictMode         string
	Debounce          int
	Cooldown          time.Duration
	MaxEvictions      int
	RepairTimeout     time.Duration
	RetryWindow       time.Duration
	MaxRetries        int
	Repair            bool
	RepairDestructive bool
	Snapshot          bool
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
