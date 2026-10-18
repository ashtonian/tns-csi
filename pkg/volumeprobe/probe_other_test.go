//go:build !linux

package volumeprobe

import (
	"context"
	"testing"
)

func TestUnsupportedPlatformDoesNotReportUsable(t *testing.T) {
	runner := newTestRunner(t, t.TempDir())
	result, err := runner.Probe(context.Background(), "v", testRequest("/a", "xfs"))
	if err != nil || result.Code != Unsupported {
		t.Fatalf("unsupported platform: %+v %v", result, err)
	}
}
