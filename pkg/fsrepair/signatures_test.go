package fsrepair

import "testing"

func TestParseKmsgLine(t *testing.T) {
	tests := []struct {
		name        string
		line        string
		wantDevice  string
		wantFamily  FSFamily
		wantOK      bool
		wantCorrupt bool
	}{
		{
			name:        "xfs corrupt in-memory 0x8",
			line:        "XFS (nvme0n1): Corruption of in-memory data (0x8) detected at xfs_trans_cancel+0x...",
			wantOK:      true,
			wantDevice:  "nvme0n1",
			wantFamily:  FamilyXFS,
			wantCorrupt: true,
		},
		{
			name:       "xfs forced shutdown",
			line:       "[ 1234.5] XFS (nvme31n1): xfs_do_force_shutdown(0x2) called from line 1...",
			wantOK:     true,
			wantDevice: "nvme31n1",
			wantFamily: FamilyXFS,
		},
		{
			name:       "xfs shut down",
			line:       "XFS (dm-3): Filesystem has been shut down due to log error (0x2).",
			wantOK:     true,
			wantDevice: "dm-3",
			wantFamily: FamilyXFS,
		},
		{
			name:       "ext4 error aborted journal",
			line:       "EXT4-fs error (device nvme1n1): ext4_journal_check_start:84: comm foo: Detected aborted journal",
			wantOK:     true,
			wantDevice: "nvme1n1",
			wantFamily: FamilyExt,
		},
		{
			name:       "ext4 remounting read-only",
			line:       "EXT4-fs (nvme1n1): Remounting filesystem read-only",
			wantOK:     true,
			wantDevice: "nvme1n1",
			wantFamily: FamilyExt,
		},
		{
			name:       "jbd2 aborting journal strips suffix",
			line:       "JBD2: Detected aborted journal. Aborting journal on device nvme2n1-8.",
			wantOK:     true,
			wantDevice: "nvme2n1",
			wantFamily: FamilyExt,
		},
		{
			name:   "xfs mount info line is not a shutdown",
			line:   "XFS (nvme0n1): Mounting V5 Filesystem",
			wantOK: false,
		},
		{
			name:   "unrelated kernel line",
			line:   "nvme nvme0: resetting controller",
			wantOK: false,
		},
		{
			name:   "empty",
			line:   "",
			wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sig, ok := ParseKmsgLine(tt.line)
			if ok != tt.wantOK {
				t.Fatalf("ParseKmsgLine(%q) ok = %v, want %v", tt.line, ok, tt.wantOK)
			}
			if !tt.wantOK {
				return
			}
			if sig.Device != tt.wantDevice {
				t.Errorf("device = %q, want %q", sig.Device, tt.wantDevice)
			}
			if sig.Family != tt.wantFamily {
				t.Errorf("family = %q, want %q", sig.Family, tt.wantFamily)
			}
			if sig.Corrupt != tt.wantCorrupt {
				t.Errorf("corrupt = %v, want %v", sig.Corrupt, tt.wantCorrupt)
			}
		})
	}
}

func TestMountErrorIndicatesShutdown(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   bool
	}{
		{"xfs structure needs cleaning", "mount: /var/x: mount(2) system call failed: Structure needs cleaning.", true},
		{"ext superblock unreadable", "mount: can't read superblock on /dev/nvme0n1", true},
		{"io error", "mount: /var/x: can't read superblock on /dev/nvme0n1: Input/output error", true},
		{"no such device excluded", "mount: special device /dev/nvme9n9 does not exist (No such device)", false},
		{"wrong fs type excluded", "mount: /var/x: wrong fs type, bad option, bad superblock", false},
		{"unrelated", "mount: only root can do that", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MountErrorIndicatesShutdown(tt.output); got != tt.want {
				t.Errorf("MountErrorIndicatesShutdown(%q) = %v, want %v", tt.output, got, tt.want)
			}
		})
	}
}

func TestStripJBD2Suffix(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"nvme0n1-8", "nvme0n1"},
		{"dm-3", "dm"}, // JBD2 form is "<dev>-<inode>"; dm-3 collapses to dm (acceptable: dm devices don't use ext journals here)
		{"sda1", "sda1"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := stripJBD2Suffix(tt.in); got != tt.want {
				t.Errorf("stripJBD2Suffix(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
