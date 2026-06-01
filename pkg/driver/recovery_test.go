package driver

import (
	"context"
	"testing"
	"time"
)

func TestFailureLimiter(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	l := newFailureLimiter(time.Hour, 3)

	if l.blocked("nvme0n1", now) {
		t.Fatal("a fresh device must not be blocked")
	}

	// Three failures within the window exhaust the budget.
	for i := range 3 {
		l.recordFailure("nvme0n1", now.Add(time.Duration(i+1)*time.Minute))
	}
	if !l.blocked("nvme0n1", now.Add(4*time.Minute)) {
		t.Errorf("device should be blocked after 3 failures in window")
	}

	// A different device is tracked independently.
	if l.blocked("nvme1n1", now.Add(4*time.Minute)) {
		t.Errorf("independent device should not be blocked")
	}

	// A success resets the count — the core guarantee: a flapping-but-recovering
	// link must never exhaust its budget.
	l.recordSuccess("nvme0n1")
	if l.blocked("nvme0n1", now.Add(5*time.Minute)) {
		t.Errorf("device must not be blocked after a successful recovery reset it")
	}

	// Failures age out of the window.
	for i := range 3 {
		l.recordFailure("nvme2n1", now.Add(time.Duration(i)*time.Minute))
	}
	if !l.blocked("nvme2n1", now.Add(3*time.Minute)) {
		t.Errorf("nvme2n1 should be blocked within window")
	}
	if l.blocked("nvme2n1", now.Add(2*time.Hour)) {
		t.Errorf("limiter should reset after the window elapses")
	}
}

// TestRecoverAndRetryMountSuccessDoesNotExhaustBudget locks in the fix for the
// limiter bug: repeated *successful* recoveries (the flapping-transport case the
// feature targets) must never lock a device out, even starting near the limit.
func TestRecoverAndRetryMountSuccessDoesNotExhaustBudget(t *testing.T) {
	s := &NodeService{
		recovery: RecoveryConfig{Mode: RecoveryOn, RepairTimeout: time.Minute},
		limiter:  newFailureLimiter(time.Hour, 3),
	}
	// Pre-load two failures so a buggy "count every attempt" limiter would trip on
	// the second call below.
	s.limiter.recordFailure("nvme0n1", time.Now())
	s.limiter.recordFailure("nvme0n1", time.Now())

	for i := range 5 {
		resp := s.recoverAndRetryMount(context.Background(), recoverParams{
			volumeID:    "pvc-1",
			devicePath:  "/dev/nvme0n1",
			fsType:      "xfs",
			mountOutput: "Structure needs cleaning",
			remount:     func(_ context.Context) (string, error) { return "", nil }, // succeeds
		})
		if resp == nil {
			t.Fatalf("iteration %d: expected successful recovery, got nil (breaker tripped on success?)", i)
		}
	}
}

// TestRecoverAndRetryMountShadowDoesNotRemount locks in the shadow fix: shadow
// mode must not change the stage outcome, even if a remount would have worked.
func TestRecoverAndRetryMountShadowDoesNotRemount(t *testing.T) {
	s := &NodeService{
		recovery: RecoveryConfig{Mode: RecoveryShadow, RepairTimeout: time.Minute},
	}
	remountCalled := false
	resp := s.recoverAndRetryMount(context.Background(), recoverParams{
		volumeID:    "pvc-1",
		devicePath:  "/dev/nvme0n1",
		fsType:      "xfs",
		mountOutput: "Structure needs cleaning",
		remount: func(_ context.Context) (string, error) {
			remountCalled = true
			return "", nil // would succeed
		},
	})
	if resp != nil {
		t.Errorf("shadow mode must not return a mounted response")
	}
	if remountCalled {
		t.Errorf("shadow mode must not perform the outcome-changing remount")
	}
}

func TestVolumeRecoveryEnabled(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"", true},       // unset -> follow node-global
		{"true", true},   //
		{"false", false}, // explicit opt-out
		{"FALSE", false}, // case-insensitive
		{" off ", false}, // trimmed + alias
		{"disabled", false},
		{"no", false},
		{"anything", true}, // unknown -> default on (global still gates)
	}
	for _, tt := range tests {
		if got := volumeRecoveryEnabled(tt.in); got != tt.want {
			t.Errorf("volumeRecoveryEnabled(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestRecoverAndRetryMountDormant(t *testing.T) {
	// With recovery disabled (zero-value config), recoverAndRetryMount must be a
	// pure no-op: it returns nil and never invokes remount.
	s := &NodeService{} // recovery.Mode == "" -> not Enabled
	called := false
	resp := s.recoverAndRetryMount(t.Context(), recoverParams{
		volumeID:    "pvc-1",
		devicePath:  "/dev/nvme0n1",
		fsType:      "xfs",
		mountOutput: "Structure needs cleaning",
		remount: func(_ context.Context) (string, error) {
			called = true
			return "", nil
		},
	})
	if resp != nil {
		t.Errorf("dormant recovery returned a response: %v", resp)
	}
	if called {
		t.Errorf("dormant recovery must not attempt remount")
	}
}
