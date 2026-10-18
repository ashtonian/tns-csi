package performance

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
)

var (
	// ErrEvidenceCorrupt rejects evidence that cannot reproduce its capture receipt.
	ErrEvidenceCorrupt = errors.New("performance evidence rejected")
	// ErrEvidenceVersion rejects unsupported capture formats or evaluation rules.
	ErrEvidenceVersion = errors.New("unsupported performance evidence version")
	// ErrEvidencePlatform requires reviewed descriptor-relative no-follow reads.
	ErrEvidencePlatform = errors.New("performance evidence verification unsupported on this platform")
)

// VerifyEvidence checks retained files against a separately trusted index receipt
// and recomputes the saved evaluation. It never modifies evidence or runs fio.
// A nil error can accompany a correctly retained failing workload evaluation.
func VerifyEvidence(root *os.Root, index ArtifactReceipt) (CaptureResult, error) {
	runID := strings.TrimSuffix(index.Name, ".result.json")
	if !identifier.MatchString(runID) || index.Bytes == 0 ||
		!validReceipt(index, runID+".result.json", maxIndexBytes) {

		return failedCapture(), ErrEvidenceCorrupt
	}
	if root == nil {
		return failedCapture(), ErrEvidenceRoot
	}
	directory, err := root.Open(".")
	if err != nil {
		return failedCapture(), ErrEvidenceRoot
	}
	result, verifyErr := verifyDirectory(directory, index, runID)
	closeErr := directory.Close()
	if closeErr != nil {
		return failedCapture(), ErrEvidenceCorrupt
	}
	return result, verifyErr
}

func verifyDirectory(directory *os.File, index ArtifactReceipt, runID string) (CaptureResult, error) {
	info, err := directory.Stat()
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return failedCapture(), ErrEvidenceRoot
	}
	raw, err := readArtifact(directory, index, maxIndexBytes)
	if err != nil {
		return failedCapture(), err
	}
	result, err := decodeRecord[CaptureResult](raw, maxIndexBytes)
	if err != nil {
		return failedCapture(), err
	}
	if result.SchemaVersion != EvidenceVersion {
		return failedCapture(), ErrEvidenceVersion
	}
	if result.Index != nil || len(result.Artifacts) != 4 {
		return failedCapture(), ErrEvidenceCorrupt
	}
	artifacts, err := readCaptureArtifacts(directory, runID, result.Artifacts)
	if err != nil {
		return failedCapture(), err
	}
	manifest, err := DecodeManifest(artifacts[runID+".manifest.json"])
	if err != nil || manifest.RunID != runID {
		return failedCapture(), ErrEvidenceCorrupt
	}
	process, err := decodeRecord[ProcessReceipt](artifacts[runID+".process.json"], maxProcessBytes)
	if err != nil || !validSignal(process.Signal) {
		return failedCapture(), ErrEvidenceCorrupt
	}
	evaluation := EvaluateFio(manifest, process, artifacts[runID+".stdout.raw"])
	if !reflect.DeepEqual(evaluation, result.Evaluation) {
		return failedCapture(), ErrEvidenceCorrupt
	}
	result.Evaluation = evaluation
	result.Index = &index
	return result, nil
}

func readCaptureArtifacts(directory *os.File, runID string, receipts []ArtifactReceipt) (map[string][]byte, error) {
	limits := map[string]int{
		runID + ".manifest.json": maxManifestBytes,
		runID + ".stdout.raw":    maxFioBytes,
		runID + ".stderr.raw":    maxFioBytes,
		runID + ".process.json":  maxProcessBytes,
	}
	seen := make(map[string]bool, len(limits))
	for _, receipt := range receipts {
		limit, ok := limits[receipt.Name]
		if !ok || seen[receipt.Name] || !validReceipt(receipt, receipt.Name, limit) {
			return nil, ErrEvidenceCorrupt
		}
		seen[receipt.Name] = true
	}
	artifacts := make(map[string][]byte, len(limits))
	for _, receipt := range receipts {
		raw, err := readArtifact(directory, receipt, limits[receipt.Name])
		if err != nil {
			return nil, err
		}
		artifacts[receipt.Name] = raw
	}
	return artifacts, nil
}

func validReceipt(receipt ArtifactReceipt, name string, limit int) bool {
	if limit < 0 {
		return false
	}
	if receipt.Name != name || receipt.Bytes > uint64(limit) {
		return false
	}
	_, valid := decodeSHA256(receipt.SHA256)
	return valid
}

func decodeSHA256(value string) ([]byte, bool) {
	if len(value) != sha256.Size*2 {
		return nil, false
	}
	hash, err := hex.DecodeString(value)
	if err != nil || hex.EncodeToString(hash) != value {
		return nil, false
	}
	return hash, true
}

func readArtifact(directory *os.File, receipt ArtifactReceipt, limit int) ([]byte, error) {
	file, err := openEvidenceFile(directory, receipt.Name)
	if err != nil {
		if errors.Is(err, ErrEvidencePlatform) {
			return nil, ErrEvidencePlatform
		}
		return nil, ErrEvidenceCorrupt
	}
	raw, readErr := readChecked(file, receipt, limit)
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return nil, ErrEvidenceCorrupt
	}
	return raw, nil
}

func readChecked(file *os.File, receipt ArtifactReceipt, limit int) ([]byte, error) {
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, ErrEvidenceCorrupt
	}
	size := info.Size()
	if size < 0 || size > int64(limit) {
		return nil, ErrEvidenceCorrupt
	}
	if uint64(size) != receipt.Bytes {
		return nil, ErrEvidenceCorrupt
	}
	raw, err := io.ReadAll(io.LimitReader(file, size+1))
	if err != nil || uint64(len(raw)) != receipt.Bytes {
		return nil, ErrEvidenceCorrupt
	}
	hash := sha256.Sum256(raw)
	if hex.EncodeToString(hash[:]) != receipt.SHA256 {
		return nil, ErrEvidenceCorrupt
	}
	return raw, nil
}

func decodeRecord[T any](raw []byte, limit int) (T, error) {
	var value T
	if err := checkJSON(raw, limit); err != nil {
		return value, ErrEvidenceCorrupt
	}
	if err := checkTypedShape(raw, reflect.TypeFor[T](), true); err != nil {
		return value, ErrEvidenceCorrupt
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return value, ErrEvidenceCorrupt
	}
	return value, nil
}
