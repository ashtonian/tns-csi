package performance

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func object(t *testing.T, value any) map[string]any {
	t.Helper()
	result, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("expected object, got %T", value)
	}
	return result
}

func array(t *testing.T, value any) []any {
	t.Helper()
	result, ok := value.([]any)
	if !ok {
		t.Fatalf("expected array, got %T", value)
	}
	return result
}

func editOutput(t *testing.T, raw []byte, edit func(map[string]any)) []byte {
	t.Helper()
	var document map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil {
		t.Fatal(err)
	}
	edit(document)
	result, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func editJob(t *testing.T, raw []byte, index int, edit func(map[string]any)) []byte {
	t.Helper()
	return editOutput(t, raw, func(document map[string]any) {
		edit(object(t, array(t, document["jobs"])[index]))
	})
}

func successfulProcess() ProcessReceipt {
	zero := 0
	return ProcessReceipt{ExitCode: &zero, ChildrenExited: true, StdoutComplete: true, StderrComplete: true}
}

func hasCode(result Evaluation, code string) bool {
	for _, finding := range result.Findings {
		if finding.Code == code {
			return true
		}
	}
	return false
}

func TestEvaluateFioHealthyAndMissingOptionalMetrics(t *testing.T) {
	raw := fixture(t, "fio-success.json")
	before := bytes.Clone(raw)
	result := EvaluateFio(testManifest(t), successfulProcess(), raw)
	if result.Status != Pass || len(result.Findings) != 0 || len(result.Jobs) != 2 {
		t.Fatalf("healthy control: %+v", result)
	}
	if result.VerifiedBytes != nil {
		t.Fatal("byte counters were misrepresented as checksum verification")
	}
	if result.Jobs[1].Write.BytesPerSecond != nil || result.Jobs[1].Write.CompletionLatency != nil {
		t.Fatal("missing optional metrics became zero values")
	}
	if value := result.Jobs[0].Write.CompletionLatency.Percentiles["99.000000"]; value == nil || *value != 1100 {
		t.Fatal("per-job nanosecond percentile was not preserved")
	}
	if !bytes.Equal(raw, before) {
		t.Fatal("evaluator modified raw evidence")
	}
}

func TestEvaluateFioJobFailures(t *testing.T) {
	t.Parallel()
	cases := []struct {
		change func(*testing.T, map[string]any)
		name   string
		code   string
	}{
		{name: "Q09 last job error", code: "PROCESS_ERROR", change: func(_ *testing.T, j map[string]any) { j["error"] = 5 }},
		{name: "Q09 continued error", code: "PROCESS_ERROR", change: func(_ *testing.T, j map[string]any) { j["total_err"] = 1 }},
		{name: "Q09 first error", code: "PROCESS_ERROR", change: func(_ *testing.T, j map[string]any) { j["first_error"] = 5 }},
		{name: "Q09 missing error", code: "OUTPUT_INVALID", change: func(_ *testing.T, j map[string]any) { delete(j, "error") }},
		{name: "Q09 null error", code: "OUTPUT_INVALID", change: func(_ *testing.T, j map[string]any) { j["error"] = nil }},
		{name: "Q10 duplicate job", code: "OUTPUT_INVALID", change: func(_ *testing.T, j map[string]any) { j["jobname"] = "job-a" }},
		{name: "Q10 unknown job", code: "OUTPUT_INVALID", change: func(_ *testing.T, j map[string]any) { j["jobname"] = "unrelated" }},
		{name: "Q12 short count", code: "COVERAGE_INCOMPLETE", change: func(t *testing.T, j map[string]any) { t.Helper(); object(t, j["write"])["io_bytes"] = 2048 }},
		{name: "Q12 extra bytes", code: "COVERAGE_INCOMPLETE", change: func(t *testing.T, j map[string]any) { t.Helper(); object(t, j["write"])["io_bytes"] = 8192 }},
		{name: "Q12 zero I/O", code: "COVERAGE_INCOMPLETE", change: func(t *testing.T, j map[string]any) { t.Helper(); object(t, j["write"])["total_ios"] = 0 }},
		{name: "short I/O", code: "PROCESS_ERROR", change: func(t *testing.T, j map[string]any) { t.Helper(); object(t, j["write"])["short_ios"] = 1 }},
		{name: "dropped I/O", code: "PROCESS_ERROR", change: func(t *testing.T, j map[string]any) { t.Helper(); object(t, j["write"])["drop_ios"] = 1 }},
		{name: "missing counter", code: "OUTPUT_INVALID", change: func(t *testing.T, j map[string]any) { t.Helper(); delete(object(t, j["write"]), "io_bytes") }},
		{name: "null counter", code: "OUTPUT_INVALID", change: func(t *testing.T, j map[string]any) { t.Helper(); object(t, j["write"])["io_bytes"] = nil }},
		{name: "missing direction", code: "OUTPUT_INVALID", change: func(_ *testing.T, j map[string]any) { delete(j, "trim") }},
		{name: "unexpected read", code: "COVERAGE_INCOMPLETE", change: func(t *testing.T, j map[string]any) { t.Helper(); object(t, j["read"])["io_bytes"] = 4096 }},
		{name: "Q13 wrong filename", code: "OUTPUT_INVALID", change: func(t *testing.T, j map[string]any) {
			t.Helper()
			object(t, j["job options"])["filename"] = "job-a.bin"
		}},
		{name: "Q13 wrong offset", code: "OUTPUT_INVALID", change: func(t *testing.T, j map[string]any) { t.Helper(); object(t, j["job options"])["offset"] = "0" }},
		{name: "group reporting", code: "OUTPUT_INVALID", change: func(t *testing.T, j map[string]any) { t.Helper(); object(t, j["job options"])["group_reporting"] = "1" }},
		{name: "unbounded run", code: "OUTPUT_INVALID", change: func(t *testing.T, j map[string]any) { t.Helper(); object(t, j["job options"])["time_based"] = "1" }},
		{name: "missing job options", code: "OUTPUT_INVALID", change: func(_ *testing.T, j map[string]any) { delete(j, "job options") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			raw := editJob(t, fixture(t, "fio-success.json"), 1, func(job map[string]any) { tc.change(t, job) })
			result := EvaluateFio(testManifest(t), successfulProcess(), raw)
			if result.Status != Fail || !hasCode(result, tc.code) {
				t.Fatalf("expected %s: %+v", tc.code, result)
			}
			if len(result.Jobs) == 0 || result.Jobs[0].Name != "job-a" {
				t.Fatal("last-job failure discarded first-job observations")
			}
		})
	}
}

func TestEvaluateFioRejectsInvalidDocuments(t *testing.T) {
	valid := fixture(t, "fio-success.json")
	cases := map[string][]byte{
		"empty":              nil,
		"null":               []byte("null"),
		"truncated":          valid[:len(valid)/2],
		"diagnostic prefix":  append([]byte("fio: verification failed\n"), valid...),
		"second document":    append(bytes.Clone(valid), valid...),
		"duplicate job key":  bytes.Replace(valid, []byte(`"error": 0`), []byte(`"error": 5, "error": 0`), 1),
		"null jobs":          []byte(`{"fio version":"fio-3.39","jobs":null}`),
		"no jobs":            []byte(`{"fio version":"fio-3.39","jobs":[]}`),
		"integer overflow":   bytes.Replace(valid, []byte(`"io_bytes": 8192`), []byte(`"io_bytes":18446744073709551616`), 1),
		"negative integer":   bytes.Replace(valid, []byte(`"io_bytes": 8192`), []byte(`"io_bytes":-8192`), 1),
		"fractional integer": bytes.Replace(valid, []byte(`"io_bytes": 8192`), []byte(`"io_bytes":8192.1`), 1),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			result := EvaluateFio(testManifest(t), successfulProcess(), raw)
			if result.Status != Fail || !hasCode(result, "OUTPUT_INVALID") {
				t.Fatalf("invalid document accepted: %+v", result)
			}
		})
	}
}

func TestEvaluateFioProcessFailures(t *testing.T) {
	cases := []struct {
		change func(*ProcessReceipt)
		name   string
		code   string
	}{
		{name: "unknown exit", code: "EVIDENCE_INCOMPLETE", change: func(p *ProcessReceipt) { p.ExitCode = nil }},
		{name: "nonzero exit", code: "PROCESS_ERROR", change: func(p *ProcessReceipt) { n := 1; p.ExitCode = &n }},
		{name: "signal", code: "PROCESS_ERROR", change: func(p *ProcessReceipt) { p.Signal = "SIGTERM" }},
		{name: "timeout", code: "PROCESS_ERROR", change: func(p *ProcessReceipt) { p.TimedOut = true }},
		{name: "cancel", code: "PROCESS_ERROR", change: func(p *ProcessReceipt) { p.Canceled = true }},
		{name: "unconfirmed child", code: "PROCESS_UNDRAINED", change: func(p *ProcessReceipt) { p.ChildrenExited = false }},
		{name: "truncated stderr", code: "EVIDENCE_INCOMPLETE", change: func(p *ProcessReceipt) { p.StderrComplete = false }},
		{name: "truncated stdout", code: "EVIDENCE_INCOMPLETE", change: func(p *ProcessReceipt) { p.StdoutComplete = false }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			process := successfulProcess()
			tc.change(&process)
			result := EvaluateFio(testManifest(t), process, fixture(t, "fio-success.json"))
			if result.Status != Fail || !hasCode(result, tc.code) || len(result.Jobs) != 2 {
				t.Fatalf("process failure lost or discarded observations: %+v", result)
			}
		})
	}
}

func TestEvaluateFioRejectsUnicodeCounterCollisions(t *testing.T) {
	valid := fixture(t, "fio-success.json")
	for _, test := range []struct {
		name        string
		original    string
		replacement string
	}{
		{name: "literal long s", original: `"short_ios": 0`, replacement: `"short_ios": 1, "` + "\u017f" + `hort_ios": 0`},
		{name: "escaped long s", original: `"short_ios": 0`, replacement: `"short_ios": 1, "\u017fhort_ios": 0`},
		{name: "reversed aliases", original: `"short_ios": 0`, replacement: `"\u017fhort_ios": 1, "short_ios": 0`},
		{name: "dropped I/O", original: `"drop_ios": 0`, replacement: `"drop_ios": 1, "drop_io\u017f": 0`},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := bytes.Replace(valid, []byte(test.original), []byte(test.replacement), 1)
			if bytes.Equal(raw, valid) {
				t.Fatal("counter fixture did not change")
			}
			result := EvaluateFio(testManifest(t), successfulProcess(), raw)
			if result.Status != Fail || !hasCode(result, "OUTPUT_INVALID") {
				t.Fatal("Unicode field alias hid a nonzero error counter")
			}
		})
	}
}

func TestEvaluateFioReadPhaseAndJobOrder(t *testing.T) {
	manifest := testManifest(t)
	manifest.Phase = "read"
	raw := editOutput(t, fixture(t, "fio-success.json"), func(document map[string]any) {
		jobs := array(t, document["jobs"])
		for _, value := range jobs {
			job := object(t, value)
			job["read"], job["write"] = job["write"], job["read"]
			object(t, job["job options"])["rw"] = "read"
			job["total_err"] = 0
		}
		jobs[0], jobs[1] = jobs[1], jobs[0]
	})
	result := EvaluateFio(manifest, successfulProcess(), raw)
	if result.Status != Pass || len(result.Jobs) != 2 || result.Jobs[0].Name != "job-b" || *result.Jobs[0].Read.Bytes != 4096 {
		t.Fatalf("read or reordered job control rejected: %+v", result)
	}
}

func TestEvaluateFioDoesNotUseAggregateBytesToHideMissingJob(t *testing.T) {
	raw := editOutput(t, fixture(t, "fio-success.json"), func(document map[string]any) {
		jobs := array(t, document["jobs"])
		object(t, object(t, jobs[0])["write"])["io_bytes"] = 12288
		document["jobs"] = jobs[:1]
	})
	result := EvaluateFio(testManifest(t), successfulProcess(), raw)
	if result.Status != Fail || !hasCode(result, "OUTPUT_INVALID") || !hasCode(result, "COVERAGE_INCOMPLETE") {
		t.Fatalf("total bytes hid missing job: %+v", result)
	}
}

func TestEvaluateFioSanitizedErrorsAndUnsupportedVersion(t *testing.T) {
	const sentinel = "credential-sentinel-never-copy"
	raw := editJob(t, fixture(t, "fio-success.json"), 1, func(job map[string]any) {
		job["jobname"] = sentinel
		job["job options"] = map[string]string{"api_key": sentinel}
		job["auth error"] = sentinel
	})
	raw = bytes.Replace(raw, []byte(FioVersion), []byte("fio-unsupported"), 1)
	result := EvaluateFio(testManifest(t), successfulProcess(), raw)
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != Fail || !hasCode(result, "UNSUPPORTED_TOOL") || strings.Contains(string(encoded), sentinel) {
		t.Fatalf("unsupported/unsafe output handling: %s", encoded)
	}
}

func TestEvaluateFioRejectsMalformedMetricsWithoutEcho(t *testing.T) {
	const sentinel = "credential-sentinel-never-copy"
	for name, stats := range map[string]map[string]any{
		"negative rate":          {"iops": -1},
		"negative latency":       {"clat_ns": map[string]any{"N": 1, "mean": -1}},
		"missing samples":        {"clat_ns": map[string]any{"mean": 10}},
		"non-numeric percentile": {"clat_ns": map[string]any{"N": 1, "percentile": map[string]uint64{sentinel: 1}}},
		"invalid percentile":     {"clat_ns": map[string]any{"N": 1, "percentile": map[string]uint64{"101": 1}}},
		"NaN percentile":         {"clat_ns": map[string]any{"N": 1, "percentile": map[string]uint64{"NaN": 1}}},
		"null percentile":        {"clat_ns": map[string]any{"N": 1, "percentile": map[string]any{"99.000000": nil}}},
		"no percentile samples":  {"clat_ns": map[string]any{"N": 0, "percentile": map[string]uint64{"99": 1}}},
	} {
		t.Run(name, func(t *testing.T) {
			raw := editJob(t, fixture(t, "fio-success.json"), 1, func(job map[string]any) {
				direction := object(t, job["write"])
				for key, value := range stats {
					direction[key] = value
				}
			})
			result := EvaluateFio(testManifest(t), successfulProcess(), raw)
			encoded, err := json.Marshal(result)
			if err != nil || result.Status != Fail || !hasCode(result, "OUTPUT_INVALID") || strings.Contains(string(encoded), sentinel) {
				t.Fatalf("malformed metrics accepted or echoed: %s (%v)", encoded, err)
			}
		})
	}
}

func TestEvaluateFioPreservesIntegerCounters(t *testing.T) {
	manifest := testManifest(t)
	manifest.Jobs[0].SizeBytes = (1 << 53) + 4096
	manifest.Budget.CapacityBytes = 1 << 60
	raw := editJob(t, fixture(t, "fio-success.json"), 0, func(job map[string]any) {
		stats := object(t, job["write"])
		stats["io_bytes"] = manifest.Jobs[0].SizeBytes
		stats["total_ios"] = manifest.Jobs[0].SizeBytes / 4096
		options := object(t, job["job options"])
		options["size"], options["io_size"] = "9007199254745088", "9007199254745088"
	})
	result := EvaluateFio(manifest, successfulProcess(), raw)
	if result.Status != Pass || *result.Jobs[0].Write.Bytes != manifest.Jobs[0].SizeBytes {
		t.Fatalf("counter rounded or rejected: %+v", result)
	}
}

func FuzzEvaluateFio(f *testing.F) {
	manifestRaw, err := os.ReadFile("testdata/manifest.json")
	if err != nil {
		f.Fatal(err)
	}
	manifest, err := DecodeManifest(manifestRaw)
	if err != nil {
		f.Fatal(err)
	}
	raw, err := os.ReadFile("testdata/fio-success.json")
	if err != nil {
		f.Fatal(err)
	}
	f.Add(raw)
	f.Add([]byte(`{"jobs":[]}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		before := bytes.Clone(data)
		result := EvaluateFio(manifest, successfulProcess(), data)
		if !bytes.Equal(data, before) || result.VerifiedBytes != nil {
			t.Fatal("raw evidence modified or checksum proof invented")
		}
		if (result.Status == Pass) != (len(result.Findings) == 0) {
			t.Fatal("status disagrees with recorded findings")
		}
	})
}
