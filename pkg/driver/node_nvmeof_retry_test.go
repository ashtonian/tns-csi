package driver

import (
	"errors"
	"testing"
)

// nvme-cli prints libc error strings with a capital first letter ("Connection refused",
// "Connection timed out"); lower-case patterns never matched them, so a target that was
// still starting up failed the stage immediately instead of being retried.
func TestIsRetryableNVMeConnectErrorMatchesNVMeCLIWording(t *testing.T) {
	retryable := []string{
		"nvme connect failed: exit status 1, output: Failed to write to /dev/nvme-fabrics: Connection refused",
		"nvme connect failed: exit status 1, output: Failed to write to /dev/nvme-fabrics: Connection timed out",
		"failed to write to nvme-fabrics device",
		"could not add new controller: invalid arguments/configuration",
		"Failed to write to /dev/nvme-fabrics: No route to host",
		"Failed to write to /dev/nvme-fabrics: Network is unreachable",
	}
	for _, msg := range retryable {
		if !isRetryableNVMeConnectError(errors.New(msg)) {
			t.Errorf("isRetryableNVMeConnectError(%q) = false, want true", msg)
		}
	}
	for _, msg := range []string{"Failed to write to /dev/nvme-fabrics: Invalid argument", "permission denied"} {
		if isRetryableNVMeConnectError(errors.New(msg)) {
			t.Errorf("isRetryableNVMeConnectError(%q) = true, want false", msg)
		}
	}
	if isRetryableNVMeConnectError(nil) {
		t.Error("isRetryableNVMeConnectError(nil) = true")
	}
}
