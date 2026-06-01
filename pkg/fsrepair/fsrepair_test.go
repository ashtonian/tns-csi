package fsrepair

import (
	"context"
	"errors"
	"testing"
)

// fakeRunner returns a cmdRunner that replays the given results in order and
// records the commands it was asked to run.
type fakeRunner struct {
	results []fakeResult
	calls   [][]string // recorded "name + args" per invocation
	idx     int
}

type fakeResult struct {
	err    error
	output string
	code   int
}

func (f *fakeRunner) run(_ context.Context, name string, args ...string) (output string, exitCode int, err error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	if f.idx >= len(f.results) {
		return "", 0, errors.New("fakeRunner: unexpected extra call")
	}
	r := f.results[f.idx]
	f.idx++
	return r.output, r.code, r.err
}

func equalArgs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestFor(t *testing.T) {
	tests := []struct {
		fsType   string
		wantType string
		wantErr  bool
	}{
		{fsType: "xfs", wantType: "xfs"},
		{fsType: "ext4", wantType: "ext4"},
		{fsType: "ext3", wantType: "ext3"},
		{fsType: "ext2", wantType: "ext2"},
		{fsType: "btrfs", wantErr: true},
		{fsType: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.fsType, func(t *testing.T) {
			r, err := For(tt.fsType)
			if tt.wantErr {
				if !errors.Is(err, ErrUnsupportedFS) {
					t.Errorf("For(%q) error = %v, want ErrUnsupportedFS", tt.fsType, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("For(%q) unexpected error: %v", tt.fsType, err)
			}
			if r.FSType() != tt.wantType {
				t.Errorf("For(%q).FSType() = %q, want %q", tt.fsType, r.FSType(), tt.wantType)
			}
		})
	}
}

func TestXFSCheck(t *testing.T) {
	tests := []struct {
		name    string
		want    Outcome
		result  fakeResult
		wantErr bool
	}{
		{name: "clean", result: fakeResult{output: "No modify flag set", code: 0}, want: OutcomeClean},
		{name: "dirty log needs -L", result: fakeResult{output: "ERROR: ... please use the -L option to destroy the log", code: 1}, want: OutcomeNeedsDestructive},
		{name: "inconsistent", result: fakeResult{output: "bad magic number in superblock", code: 1}, want: OutcomeDirty},
		{name: "tool missing", result: fakeResult{err: errors.New(`exec: "xfs_repair": not found`)}, want: OutcomeFailed, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fr := &fakeRunner{results: []fakeResult{tt.result}}
			r := xfsRepairer{run: fr.run}
			res, err := r.Check(context.Background(), "/dev/nvme0n1")
			if tt.wantErr && err == nil {
				t.Errorf("Check() expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("Check() unexpected error: %v", err)
			}
			if res.Outcome != tt.want {
				t.Errorf("Check() outcome = %q, want %q (output=%q)", res.Outcome, tt.want, res.Output)
			}
			want := []string{"xfs_repair", "-n", "/dev/nvme0n1"}
			if !equalArgs(fr.calls[0], want) {
				t.Errorf("Check() ran %v, want %v", fr.calls[0], want)
			}
		})
	}
}

func TestXFSRepair(t *testing.T) {
	tests := []struct {
		name      string
		want      Outcome
		results   []fakeResult
		wantCalls int
		allow     bool
	}{
		{name: "clean repair", results: []fakeResult{{code: 0}}, want: OutcomeRepaired, wantCalls: 1},
		{name: "dirty log, destructive not allowed", results: []fakeResult{{output: "use the -L option", code: 1}}, want: OutcomeNeedsDestructive, wantCalls: 1},
		{name: "dirty log, destructive allowed, -L succeeds", results: []fakeResult{{output: "use the -L option", code: 1}, {code: 0}}, allow: true, want: OutcomeRepairedDestructive, wantCalls: 2},
		{name: "dirty log, destructive allowed, -L fails", results: []fakeResult{{output: "use the -L option", code: 1}, {code: 2}}, allow: true, want: OutcomeFailed, wantCalls: 2},
		{name: "non-log failure", results: []fakeResult{{output: "fatal error -- couldn't open device", code: 1}}, allow: true, want: OutcomeFailed, wantCalls: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fr := &fakeRunner{results: tt.results}
			r := xfsRepairer{run: fr.run}
			res, err := r.Repair(context.Background(), "/dev/nvme0n1", tt.allow)
			if err != nil {
				t.Fatalf("Repair() unexpected error: %v", err)
			}
			if res.Outcome != tt.want {
				t.Errorf("Repair() outcome = %q, want %q", res.Outcome, tt.want)
			}
			if len(fr.calls) != tt.wantCalls {
				t.Fatalf("Repair() made %d calls, want %d (%v)", len(fr.calls), tt.wantCalls, fr.calls)
			}
			if tt.wantCalls == 2 {
				want := []string{"xfs_repair", "-L", "/dev/nvme0n1"}
				if !equalArgs(fr.calls[1], want) {
					t.Errorf("escalation ran %v, want %v", fr.calls[1], want)
				}
			}
		})
	}
}

func TestExtCheck(t *testing.T) {
	tests := []struct {
		name string
		want Outcome
		code int
	}{
		{name: "clean", code: 0, want: OutcomeClean},
		{name: "errors uncorrected", code: 4, want: OutcomeDirty},
		{name: "corrected bit also set", code: 1 | 4, want: OutcomeDirty},
		{name: "operational error", code: 8, want: OutcomeFailed},
		{name: "usage error", code: 16, want: OutcomeFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fr := &fakeRunner{results: []fakeResult{{code: tt.code}}}
			r := extRepairer{fsType: "ext4", run: fr.run}
			res, err := r.Check(context.Background(), "/dev/nvme0n1")
			if err != nil {
				t.Fatalf("Check() unexpected error: %v", err)
			}
			if res.Outcome != tt.want {
				t.Errorf("Check(code=%d) outcome = %q, want %q", tt.code, res.Outcome, tt.want)
			}
			want := []string{"e2fsck", "-f", "-n", "/dev/nvme0n1"}
			if !equalArgs(fr.calls[0], want) {
				t.Errorf("Check() ran %v, want %v", fr.calls[0], want)
			}
		})
	}
}

func TestExtRepair(t *testing.T) {
	tests := []struct {
		name      string
		want      Outcome
		results   []fakeResult
		wantCalls int
		allow     bool
	}{
		{name: "nothing to do", results: []fakeResult{{code: 0}}, want: OutcomeRepaired, wantCalls: 1},
		{name: "preen corrected", results: []fakeResult{{code: 1}}, want: OutcomeRepaired, wantCalls: 1},
		{name: "preen corrected reboot", results: []fakeResult{{code: 2}}, want: OutcomeRepaired, wantCalls: 1},
		{name: "preen stuck, not allowed", results: []fakeResult{{code: 4}}, want: OutcomeNeedsDestructive, wantCalls: 1},
		{name: "preen stuck, allowed, -fy succeeds", results: []fakeResult{{code: 4}, {code: 1}}, allow: true, want: OutcomeRepairedDestructive, wantCalls: 2},
		{name: "preen stuck, allowed, -fy fails", results: []fakeResult{{code: 4}, {code: 8}}, allow: true, want: OutcomeFailed, wantCalls: 2},
		{name: "operational error", results: []fakeResult{{code: 8}}, allow: true, want: OutcomeFailed, wantCalls: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fr := &fakeRunner{results: tt.results}
			r := extRepairer{fsType: "ext4", run: fr.run}
			res, err := r.Repair(context.Background(), "/dev/nvme0n1", tt.allow)
			if err != nil {
				t.Fatalf("Repair() unexpected error: %v", err)
			}
			if res.Outcome != tt.want {
				t.Errorf("Repair() outcome = %q, want %q", res.Outcome, tt.want)
			}
			if len(fr.calls) != tt.wantCalls {
				t.Fatalf("Repair() made %d calls, want %d (%v)", len(fr.calls), tt.wantCalls, fr.calls)
			}
			if tt.wantCalls == 2 {
				want := []string{"e2fsck", "-f", "-y", "/dev/nvme0n1"}
				if !equalArgs(fr.calls[1], want) {
					t.Errorf("escalation ran %v, want %v", fr.calls[1], want)
				}
			}
		})
	}
}
