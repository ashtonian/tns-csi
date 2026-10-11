package driver

import (
	"context"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/fenio/tns-csi/pkg/mount"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The filesystem helpers already return the right gRPC code (Unimplemented for a
// filesystem that cannot be grown, FailedPrecondition when nothing is mounted).
// NodeExpandVolume re-wrapped every one of them as Internal, so the CO retried forever
// on errors that will never succeed.
func TestNodeExpandKeepsFilesystemErrorCodes(t *testing.T) {
	volumePath := t.TempDir()
	orig := lookupMountedEntry
	lookupMountedEntry = func(context.Context, string) (mount.Entry, bool, error) {
		return mount.Entry{Source: "/dev/nvme0n1", FSType: "vfat"}, true, nil
	}
	t.Cleanup(func() { lookupMountedEntry = orig })

	s := NewNodeService("test-node", nil, false, nil, false, 5)
	_, err := s.NodeExpandVolume(context.Background(), &csi.NodeExpandVolumeRequest{
		VolumeId:      "pvc-1",
		VolumePath:    volumePath,
		CapacityRange: &csi.CapacityRange{RequiredBytes: 2 << 30},
	})
	if got := status.Code(err); got != codes.Unimplemented {
		t.Fatalf("NodeExpandVolume(vfat) code = %v (%v), want Unimplemented", got, err)
	}
}
