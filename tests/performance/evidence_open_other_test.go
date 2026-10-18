//go:build !linux && !darwin

package performance

import (
	"errors"
	"testing"
)

func TestEvidenceReadRequiresSupportedPlatform(t *testing.T) {
	if file, err := openEvidenceFile(nil, "unused"); file != nil || !errors.Is(err, ErrEvidencePlatform) {
		t.Fatal("unsupported platform used a weaker file open")
	}
}
