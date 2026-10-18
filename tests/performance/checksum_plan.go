package performance

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
)

// ChecksumPlanVersion identifies the independently retained checksum-plan format.
const ChecksumPlanVersion = 1

// The manifest and at most 1024 bound job digests share one finite document.
const maxChecksumPlanBytes = 2 << 20

// ErrInvalidChecksumPlan rejects inputs that cannot describe finite verification.
var ErrInvalidChecksumPlan = errors.New("invalid performance checksum plan")

type checksumPlan struct {
	Expected      []ExtentChecksum `json:"expected"`
	Manifest      Manifest         `json:"manifest"`
	SchemaVersion uint64           `json:"schema_version"`
}

// SaveChecksumPlan exclusively retains expected digests and the intended read
// manifest in a private directory outside measured storage. The caller must save
// it before writing test data and retain the returned receipt independently.
// Persistence cannot establish when or how the expected digests were computed.
func SaveChecksumPlan(root *os.Root, manifest Manifest, expected []ExtentChecksum) (ArtifactReceipt, error) {
	if _, valid := checksumDigests(manifest, expected); !valid {
		return ArtifactReceipt{}, ErrInvalidChecksumPlan
	}
	raw, err := json.Marshal(checksumPlan{Manifest: manifest, Expected: expected, SchemaVersion: ChecksumPlanVersion})
	if err != nil || len(raw) > maxChecksumPlanBytes {
		return ArtifactReceipt{}, ErrInvalidChecksumPlan
	}
	if root == nil {
		return ArtifactReceipt{}, ErrEvidenceRoot
	}
	private, err := root.OpenRoot(".")
	if err != nil {
		return ArtifactReceipt{}, ErrEvidenceRoot
	}
	receipt, saveErr := saveChecksumPlan(private, manifest.RunID, raw)
	if closeErr := private.Close(); closeErr != nil {
		return ArtifactReceipt{}, ErrEvidenceIncomplete
	}
	return receipt, saveErr
}

func saveChecksumPlan(root *os.Root, runID string, raw []byte) (ArtifactReceipt, error) {
	info, err := root.Stat(".")
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return ArtifactReceipt{}, ErrEvidenceRoot
	}
	return persistArtifact(root, runID+".checksums.json", raw)
}

// LoadChecksumPlan authenticates retained expectations against a separately
// trusted receipt and the caller's complete intended read manifest. It returns
// no expectations on error and never opens workload data or changes evidence.
func LoadChecksumPlan(root *os.Root, receipt ArtifactReceipt, manifest Manifest) ([]ExtentChecksum, error) {
	if manifest.Validate() != nil || manifest.Phase != phaseRead {
		return nil, ErrInvalidChecksumPlan
	}
	if receipt.Bytes == 0 || !validReceipt(receipt, manifest.RunID+".checksums.json", maxChecksumPlanBytes) {
		return nil, ErrEvidenceCorrupt
	}
	if root == nil {
		return nil, ErrEvidenceRoot
	}
	directory, err := root.Open(".")
	if err != nil {
		return nil, ErrEvidenceRoot
	}
	expected, loadErr := loadChecksumPlan(directory, receipt, manifest)
	if closeErr := directory.Close(); closeErr != nil {
		return nil, ErrEvidenceCorrupt
	}
	return expected, loadErr
}

func loadChecksumPlan(directory *os.File, receipt ArtifactReceipt, manifest Manifest) ([]ExtentChecksum, error) {
	info, err := directory.Stat()
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return nil, ErrEvidenceRoot
	}
	raw, err := readArtifact(directory, receipt, maxChecksumPlanBytes)
	if err != nil {
		return nil, err
	}
	plan, err := decodeRecord[checksumPlan](raw, maxChecksumPlanBytes)
	if err != nil {
		return nil, err
	}
	if plan.SchemaVersion != ChecksumPlanVersion {
		return nil, ErrEvidenceVersion
	}
	if _, valid := checksumDigests(plan.Manifest, plan.Expected); !valid || !reflect.DeepEqual(plan.Manifest, manifest) {
		return nil, ErrEvidenceCorrupt
	}
	return plan.Expected, nil
}
