package performance

import (
	"encoding/json"
	"strconv"
)

// Status is the outcome of byte-accounting evaluation, not live qualification.
type Status string

const (
	// Pass means required process and byte-accounting assertions succeeded.
	Pass Status = "PASS"
	// Fail means process, output or byte-accounting evidence failed.
	Fail Status = "FAIL"
)

// Finding records a stable failure code without echoing raw output or options.
type Finding struct {
	JobIndex *int   `json:"job_index,omitempty"`
	Code     string `json:"code"`
}

// ProcessReceipt must be supplied by a caller that observed process termination.
type ProcessReceipt struct {
	ExitCode       *int   `json:"exit_code"`
	Signal         string `json:"signal"`
	TimedOut       bool   `json:"timed_out"`
	Canceled       bool   `json:"canceled"`
	ChildrenExited bool   `json:"children_exited"`
	StdoutComplete bool   `json:"stdout_complete"`
	StderrComplete bool   `json:"stderr_complete"`
}

// Evaluation retains per-job metrics even when a later job or process failed.
// VerifiedBytes is deliberately unknown: fio counters alone cannot prove checksums.
type Evaluation struct {
	VerifiedBytes *uint64          `json:"verified_bytes"`
	Status        Status           `json:"status"`
	Findings      []Finding        `json:"findings"`
	Jobs          []JobObservation `json:"jobs"`
}

// JobObservation uses the expected job name, never an untrusted output label.
type JobObservation struct {
	Read  *IOStats `json:"read"`
	Write *IOStats `json:"write"`
	Trim  *IOStats `json:"trim"`
	Name  string   `json:"name"`
}

// IOStats preserves missing optional metrics as nil, with explicit fio units.
type IOStats struct {
	Bytes             *uint64  `json:"io_bytes"`
	TotalIOs          *uint64  `json:"total_ios"`
	ShortIOs          *uint64  `json:"short_ios"`
	DroppedIOs        *uint64  `json:"drop_ios"`
	BytesPerSecond    *uint64  `json:"bw_bytes"`
	IOPS              *float64 `json:"iops"`
	RuntimeMillis     *uint64  `json:"runtime"`
	CompletionLatency *Latency `json:"clat_ns"`
	TotalLatency      *Latency `json:"lat_ns"`
}

// Latency preserves nanosecond statistics and per-job percentile values.
type Latency struct {
	Samples     *uint64            `json:"N"`
	Mean        *float64           `json:"mean"`
	Percentiles map[string]*uint64 `json:"percentile"`
}

type fioOutput struct {
	Version string   `json:"fio version"`
	Jobs    []fioJob `json:"jobs"`
}

type fioJob struct {
	Error      *uint64           `json:"error"`
	TotalError *uint64           `json:"total_err"`
	FirstError *uint64           `json:"first_error"`
	Options    map[string]string `json:"job options"`
	Read       *IOStats          `json:"read"`
	Write      *IOStats          `json:"write"`
	Trim       *IOStats          `json:"trim"`
	Name       string            `json:"jobname"`
}

// EvaluateFio evaluates one final JSON/JSON+ document for a finite file phase.
// The caller must retain stdout/stderr unchanged, including on parse failure.
// This function performs no file, process, environment, API or cluster operations.
func EvaluateFio(manifest Manifest, process ProcessReceipt, raw []byte) Evaluation {
	result := Evaluation{Status: Pass}
	if err := manifest.Validate(); err != nil {
		result.reject("INVALID_REQUEST", nil)
		return result
	}
	result.checkProcess(process)
	if err := checkJSON(raw, maxFioBytes); err != nil {
		result.reject("OUTPUT_INVALID", nil)
		return result
	}
	var output fioOutput
	if err := json.Unmarshal(raw, &output); err != nil {
		result.reject("OUTPUT_INVALID", nil)
		return result
	}
	if output.Version != manifest.FioVersion {
		result.reject("UNSUPPORTED_TOOL", nil)
	}
	if len(output.Jobs) != len(manifest.Jobs) {
		result.reject("OUTPUT_INVALID", nil)
	}
	result.checkJobs(manifest, output.Jobs)
	return result
}

func (e *Evaluation) reject(code string, index *int) {
	e.Status = Fail
	e.Findings = append(e.Findings, Finding{Code: code, JobIndex: index})
}

func (e *Evaluation) checkProcess(process ProcessReceipt) {
	if !process.ChildrenExited {
		e.reject("PROCESS_UNDRAINED", nil)
	}
	if process.ExitCode == nil || !process.StdoutComplete || !process.StderrComplete {
		e.reject("EVIDENCE_INCOMPLETE", nil)
	}
	if process.TimedOut || process.Canceled || process.Signal != "" || (process.ExitCode != nil && *process.ExitCode != 0) {
		e.reject("PROCESS_ERROR", nil)
	}
}

func (e *Evaluation) checkJobs(manifest Manifest, jobs []fioJob) {
	expected := make(map[string]int, len(manifest.Jobs))
	for i, job := range manifest.Jobs {
		expected[job.Name] = i
	}
	seen := make(map[string]bool, len(jobs))
	for _, job := range jobs {
		index, ok := expected[job.Name]
		if !ok || seen[job.Name] {
			e.reject("OUTPUT_INVALID", nil)
			continue
		}
		seen[job.Name] = true
		if job.Error == nil {
			e.reject("OUTPUT_INVALID", &index)
		} else if *job.Error != 0 {
			e.reject("PROCESS_ERROR", &index)
		}
		if (job.TotalError != nil && *job.TotalError != 0) || (job.FirstError != nil && *job.FirstError != 0) {
			e.reject("PROCESS_ERROR", &index)
		}
		if !optionsMatch(manifest.Phase, manifest.Jobs[index], job.Options) {
			e.reject("OUTPUT_INVALID", &index)
		}
		for _, stats := range []*IOStats{job.Read, job.Write, job.Trim} {
			e.checkMetrics(stats, index)
		}
		e.Jobs = append(e.Jobs, JobObservation{Name: manifest.Jobs[index].Name, Read: job.Read, Write: job.Write, Trim: job.Trim})
		e.checkDirections(manifest.Phase, manifest.Jobs[index], job, index)
	}
	for index, plan := range manifest.Jobs {
		if !seen[plan.Name] {
			e.reject("OUTPUT_INVALID", &index)
		}
	}
}

func (e *Evaluation) checkMetrics(stats *IOStats, index int) {
	if stats == nil {
		return
	}
	if stats.IOPS != nil && *stats.IOPS < 0 {
		stats.IOPS = nil
		e.reject("OUTPUT_INVALID", &index)
	}
	for _, latency := range []**Latency{&stats.CompletionLatency, &stats.TotalLatency} {
		if !validLatency(*latency) {
			*latency = nil
			e.reject("OUTPUT_INVALID", &index)
		}
	}
}

func validLatency(latency *Latency) bool {
	if latency == nil {
		return true
	}
	if latency.Samples == nil || (latency.Mean != nil && *latency.Mean < 0) {
		return false
	}
	for percentile, nanos := range latency.Percentiles {
		value, err := strconv.ParseFloat(percentile, 64)
		if err != nil || !(value > 0 && value <= 100) || *latency.Samples == 0 || nanos == nil {
			return false
		}
	}
	return true
}

func optionsMatch(phase string, job JobPlan, options map[string]string) bool {
	// Explicit canonical options exclude grouping, loops and time-based accounting.
	expected := map[string]string{
		"filename": job.Filename, "rw": phase,
		"offset": strconv.FormatUint(job.OffsetBytes, 10),
		"size":   strconv.FormatUint(job.SizeBytes, 10), "io_size": strconv.FormatUint(job.SizeBytes, 10),
		"bs": strconv.FormatUint(job.BlockSizeBytes, 10), "iodepth": strconv.FormatUint(job.Depth, 10),
		"numjobs": "1", "group_reporting": "0", "time_based": "0", "loops": "1", "verify": "0",
	}
	for name, value := range expected {
		if options[name] != value {
			return false
		}
	}
	return true
}

func (e *Evaluation) checkDirections(phase string, plan JobPlan, job fioJob, index int) {
	for _, direction := range []struct {
		stats *IOStats
		name  string
	}{{stats: job.Read, name: "read"}, {stats: job.Write, name: "write"}, {stats: job.Trim, name: "trim"}} {
		stats := direction.stats
		if stats == nil || stats.Bytes == nil || stats.TotalIOs == nil || stats.ShortIOs == nil || stats.DroppedIOs == nil {
			e.reject("OUTPUT_INVALID", &index)
			continue
		}
		if *stats.ShortIOs != 0 || *stats.DroppedIOs != 0 {
			e.reject("PROCESS_ERROR", &index)
		}
		wantBytes, wantIOs := uint64(0), uint64(0)
		if direction.name == phase {
			wantBytes, wantIOs = plan.SizeBytes, plan.SizeBytes/plan.BlockSizeBytes
		}
		if *stats.Bytes != wantBytes || *stats.TotalIOs != wantIOs {
			e.reject("COVERAGE_INCOMPLETE", &index)
		}
	}
}
