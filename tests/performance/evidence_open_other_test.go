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

func TestWitnessTargetOpenRequiresSupportedPlatform(t *testing.T) {
	plan := WitnessFilePlan{Stream: WitnessStream{RunID: "synthetic", StreamID: "stream-a", Generation: 1},
		CapacityBytes: 4096, MaxPayloadBytes: 1024, MaxRecords: 1}
	if target, parent, err := CreateWitnessTarget(nil, plan); target != nil || parent != nil || !errors.Is(err, ErrEvidencePlatform) {
		t.Fatal("unsupported platform admitted target creation")
	}
	if source, err := OpenWitnessSource(nil, plan); source != nil || !errors.Is(err, ErrEvidencePlatform) {
		t.Fatal("unsupported platform used a weaker source open")
	}
}
