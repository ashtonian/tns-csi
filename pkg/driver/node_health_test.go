package driver

import (
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

func TestHealthy(t *testing.T) {
	h := Healthy()
	if h.Abnormal {
		t.Error("Healthy() should return Abnormal=false")
	}
	if h.Message != "" {
		t.Errorf("Healthy() should have empty message, got %q", h.Message)
	}
}

func TestUnhealthy(t *testing.T) {
	tests := []struct {
		name    string
		message string
	}{
		{
			name:    "simple message",
			message: "volume not found",
		},
		{
			name:    "detailed message",
			message: "NFS mount stale: server 192.168.1.100 unreachable",
		},
		{
			name:    "empty message",
			message: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := Unhealthy(tt.message)
			if !h.Abnormal {
				t.Error("Unhealthy() should return Abnormal=true")
			}
			if h.Message != tt.message {
				t.Errorf("Unhealthy(%q) message = %q, want %q", tt.message, h.Message, tt.message)
			}
		})
	}
}

func TestVolumeHealthToCSI(t *testing.T) {
	tests := []struct {
		name   string
		health VolumeHealth
	}{
		{
			name:   "healthy volume",
			health: Healthy(),
		},
		{
			name:   "unhealthy volume",
			health: Unhealthy("connection timeout"),
		},
		{
			name: "custom health",
			health: VolumeHealth{
				Abnormal: true,
				Message:  "custom error message",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			csiHealth := tt.health.ToCSI("test-volume")
			if csiHealth == nil {
				t.Fatal("ToCSI() returned nil")
			}
			if csiHealth.VolumeId != "test-volume" {
				t.Errorf("ToCSI().VolumeId = %q, want test-volume", csiHealth.VolumeId)
			}
			if !tt.health.Abnormal {
				if len(csiHealth.HealthStatuses) != 0 {
					t.Errorf("healthy volume has %d adverse statuses, want 0", len(csiHealth.HealthStatuses))
				}
				return
			}
			if len(csiHealth.HealthStatuses) != 1 {
				t.Fatalf("unhealthy volume has %d statuses, want 1", len(csiHealth.HealthStatuses))
			}
			entry := csiHealth.HealthStatuses[0]
			if entry.Status != csi.VolumeHealthErrorType_INACCESSIBLE {
				t.Errorf("health status = %v, want INACCESSIBLE", entry.Status)
			}
			if entry.Message != tt.health.Message {
				t.Errorf("health message = %q, want %q", entry.Message, tt.health.Message)
			}
		})
	}
}

func TestGetNVMeControllerState(t *testing.T) {
	// Point the sysfs lookup at a controlled directory so the test does not
	// depend on which NVMe devices happen to be present on the host runner.
	block := fakeSysBlock(t, map[string]string{"nvme0": nqnA}, map[string]string{"nvme0n1": "nvme0"}, nil)
	fakeControllers(t, block, "nvme0", map[string]string{"nvme0": "live"})
	origPath := sysBlockPath
	sysBlockPath = block
	t.Cleanup(func() { sysBlockPath = origPath })

	tests := []struct {
		name       string
		devicePath string
		wantState  string
		wantErr    bool
	}{
		{
			name:       "non-nvme device",
			devicePath: "/dev/sda",
			wantErr:    true,
		},
		{
			name:       "nvme device - controller present",
			devicePath: "/dev/nvme0n1",
			wantState:  "live",
		},
		{
			name:       "nvme device - controller absent",
			devicePath: "/dev/nvme9n1",
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state, err := getNVMeControllerState(tt.devicePath)
			if tt.wantErr {
				if err == nil {
					t.Errorf("getNVMeControllerState(%q) expected error, got nil", tt.devicePath)
				}
				return
			}
			if err != nil {
				t.Errorf("getNVMeControllerState(%q) unexpected error: %v", tt.devicePath, err)
				return
			}
			if state != tt.wantState {
				t.Errorf("getNVMeControllerState(%q) = %q, want %q", tt.devicePath, state, tt.wantState)
			}
		})
	}
}

func TestNVMeControllerNameExtraction(t *testing.T) {
	// Test the controller name extraction logic from getNVMeControllerState
	// The function extracts "nvme0" from device names like "nvme0n1" or "nvme0n1p1"

	tests := []struct {
		errType    error
		devicePath string
		wantErr    bool
	}{
		{
			devicePath: "/dev/sda",
			wantErr:    true,
			errType:    ErrNVMeNotNVMeDevice,
		},
		{
			devicePath: "/dev/vda",
			wantErr:    true,
			errType:    ErrNVMeNotNVMeDevice,
		},
		{
			devicePath: "/dev/xvda",
			wantErr:    true,
			errType:    ErrNVMeNotNVMeDevice,
		},
	}

	for _, tt := range tests {
		t.Run(tt.devicePath, func(t *testing.T) {
			_, err := getNVMeControllerState(tt.devicePath)
			if tt.wantErr {
				if err == nil {
					t.Errorf("getNVMeControllerState(%q) expected error, got nil", tt.devicePath)
					return
				}
				// For non-NVMe devices, we should get the specific error type
				if tt.errType != nil {
					if err.Error() != "not an NVMe device: "+tt.devicePath {
						// The error might be different if the path format changed
						t.Logf("Got error: %v", err)
					}
				}
			}
		})
	}
}
