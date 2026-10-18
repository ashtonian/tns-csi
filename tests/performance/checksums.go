package performance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"math"
)

// ExtentChecksum binds a trusted expected digest to the complete finite job plan.
// The caller must establish it independently before writing the data under test.
type ExtentChecksum struct {
	SHA256 string  `json:"sha256"`
	Plan   JobPlan `json:"plan"`
}

// IntegrityResult describes checksum coverage of the supplied readers only.
// Aggregate verified bytes remain unknown unless every planned extent matches.
type IntegrityResult struct {
	VerifiedBytes *uint64        `json:"verified_bytes"`
	Status        Status         `json:"status"`
	Jobs          []IntegrityJob `json:"jobs"`
	Findings      []Finding      `json:"findings"`
}

// IntegrityJob separates bytes read from bytes whose expected digest matched.
type IntegrityJob struct {
	VerifiedBytes *uint64 `json:"verified_bytes"`
	Name          string  `json:"name"`
	ReadBytes     uint64  `json:"read_bytes"`
}

type checksumSource struct {
	reader   io.ReaderAt
	expected []byte
	plan     JobPlan
}

// VerifyChecksums reads every finite extent from caller-supplied readers keyed by
// filename. It opens/closes no files and never writes, repairs or retries a read.
// The caller owns source identity, read-only handles and expected-checksum trust.
// Cancellation is checked between reads; it cannot interrupt a blocked ReaderAt.
func VerifyChecksums(ctx context.Context, manifest Manifest, expected []ExtentChecksum, sources map[string]io.ReaderAt) IntegrityResult {
	result := IntegrityResult{Status: Pass}
	inputs, valid := checksumInputs(manifest, expected, sources)
	if ctx == nil || !valid {
		result.reject("INVALID_REQUEST", nil)
		return result
	}
	bufferBytes := manifest.Budget.MaxMemoryBytes
	if bufferBytes > 64<<10 {
		bufferBytes = 64 << 10
	}
	buffer := make([]byte, bufferBytes)
	var total uint64
	for index, input := range inputs {
		if ctx.Err() != nil {
			result.reject(reasonProcessError, &index)
			break
		}
		job, code := verifyExtent(ctx, input.plan, input.expected, input.reader, buffer)
		result.Jobs = append(result.Jobs, job)
		if code != "" {
			result.reject(code, &index)
		} else {
			total += input.plan.SizeBytes
		}
		if ctx.Err() != nil {
			if code != reasonProcessError {
				result.reject(reasonProcessError, &index)
			}
			break
		}
	}
	if result.Status == Pass {
		result.VerifiedBytes = &total
	}
	return result
}

func (r *IntegrityResult) reject(code string, index *int) {
	r.Status = Fail
	r.Findings = append(r.Findings, Finding{Code: code, JobIndex: index})
}

func checksumInputs(manifest Manifest, expected []ExtentChecksum, sources map[string]io.ReaderAt) ([]checksumSource, bool) {
	wanted, valid := checksumDigests(manifest, expected)
	if !valid || len(sources) != len(manifest.Jobs) {
		return nil, false
	}
	inputs := make([]checksumSource, 0, len(manifest.Jobs))
	for _, plan := range manifest.Jobs {
		if sources[plan.Filename] == nil {
			return nil, false
		}
		inputs = append(inputs, checksumSource{plan: plan, expected: wanted[plan.Name], reader: sources[plan.Filename]})
	}
	return inputs, true
}

func checksumDigests(manifest Manifest, expected []ExtentChecksum) (map[string][]byte, bool) {
	if manifest.Validate() != nil || manifest.Phase != phaseRead || len(expected) != len(manifest.Jobs) {
		return nil, false
	}
	plans := make(map[string]JobPlan, len(manifest.Jobs))
	for _, plan := range manifest.Jobs {
		if plan.SizeBytes > math.MaxInt64 || plan.OffsetBytes > math.MaxInt64-plan.SizeBytes {
			return nil, false
		}
		plans[plan.Name] = plan
	}
	wanted := make(map[string][]byte, len(expected))
	for _, checksum := range expected {
		plan, ok := plans[checksum.Plan.Name]
		if !ok || plan != checksum.Plan || wanted[plan.Name] != nil {
			return nil, false
		}
		digest, valid := decodeSHA256(checksum.SHA256)
		if !valid {
			return nil, false
		}
		wanted[plan.Name] = digest
	}
	return wanted, true
}

func verifyExtent(ctx context.Context, plan JobPlan, expected []byte, source io.ReaderAt, buffer []byte) (job IntegrityJob, code string) {
	job = IntegrityJob{Name: plan.Name}
	digest := sha256.New()
	for job.ReadBytes < plan.SizeBytes {
		if ctx.Err() != nil {
			return job, reasonProcessError
		}
		chunk := buffer[:min(uint64(len(buffer)), plan.SizeBytes-job.ReadBytes)]
		// checksumInputs bounds the entire extent to ReaderAt's signed offset range.
		offset := plan.OffsetBytes + job.ReadBytes
		if offset > math.MaxInt64 {
			return job, reasonEvidenceIncomplete
		}
		n, err := source.ReadAt(chunk, int64(offset))
		if n < 0 || n > len(chunk) {
			return job, reasonEvidenceIncomplete
		}
		job.ReadBytes += uint64(n)
		if n != len(chunk) {
			return job, "COVERAGE_INCOMPLETE"
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return job, reasonEvidenceIncomplete
		}
		if _, hashErr := digest.Write(chunk); hashErr != nil {
			return job, reasonEvidenceIncomplete
		}
	}
	if ctx.Err() != nil {
		return job, reasonProcessError
	}
	if !bytes.Equal(digest.Sum(nil), expected) {
		return job, "CHECKSUM_MISMATCH"
	}
	verified := plan.SizeBytes
	job.VerifiedBytes = &verified
	return job, ""
}
