//go:build !linux && !darwin

package performance

import "os"

func openEvidenceFile(_ *os.File, _ string) (*os.File, error) {
	return nil, ErrEvidencePlatform
}
