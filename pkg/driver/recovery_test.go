package driver

import (
	"context"
	"testing"
	"time"
)

func TestCircuitBreaker(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	b := newCircuitBreaker(time.Hour, 3)

	if b.blocked("nvme0n1", now) {
		t.Fatal("a fresh device must not be blocked")
	}

	// Three FAILURES within the window open the breaker.
	for i := 1; i <= 3; i++ {
		b.recordFailure("nvme0n1", now.Add(time.Duration(i)*time.Minute))
	}
	if !b.blocked("nvme0n1", now.Add(4*time.Minute)) {
		t.Errorf("device should be blocked after %d failures in window", 3)
	}

	// A different device is tracked independently.
	if b.blocked("nvme1n1", now.Add(4*time.Minute)) {
		t.Errorf("independent device should not be blocked")
	}

	// A success resets the count — the core fix: a flapping-but-recovering link
	// must never trip the breaker.
	b.recordSuccess("nvme0n1")
	if b.blocked("nvme0n1", now.Add(5*time.Minute)) {
		t.Errorf("device must not be blocked after a successful recovery reset it")
	}

	// Failures age out of the window.
	for i := range 3 {
		b.recordFailure("nvme2n1", now.Add(time.Duration(i)*time.Minute))
	}
	if !b.blocked("nvme2n1", now.Add(3*time.Minute)) {
		t.Errorf("nvme2n1 should be blocked within window")
	}
	if b.blocked("nvme2n1", now.Add(2*time.Hour)) {
		t.Errorf("breaker should reset after the window elapses")
	}
}

// TestRecoverAndRetryMountSuccessDoesNotTripBreaker locks in the fix for the
// breaker bug: repeated *successful* recoveries (the flapping-transport case the
// feature targets) must never lock a device out, even starting near the limit.
func TestRecoverAndRetryMountSuccessDoesNotTripBreaker(t *testing.T) {
	s := &NodeService{
		recovery: RecoveryConfig{Mode: RecoveryOn, RepairTimeout: time.Minute},
		breaker:  newCircuitBreaker(time.Hour, 3),
	}
	// Pre-load two failures so a buggy "count every attempt" breaker would trip
	// on the second call below.
	s.breaker.recordFailure("nvme0n1", time.Now())
	s.breaker.recordFailure("nvme0n1", time.Now())

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
