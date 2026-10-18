# Offline performance evidence

This package validates a finite file-test request, evaluates captured fio output,
preserves supplied captures and verifies their read-back against a trusted receipt.
It does not
connect to Kubernetes/TrueNAS, run fio, inspect a mount, change settings or delete
data. Tests use synthetic fixtures, fake process receipts and temporary local
directories. No production driver behavior changes are included.

## Request and evaluation

`DecodeManifest` requires all fields shown in
[`testdata/manifest.json`](testdata/manifest.json), with their exact case and
types. Schema 1 currently covers separate finite read and write phases on XFS
or ext4. Mixed/time-based profiles, raw devices and checksum verification need
additional contracts before use.

Requests are limited to 1 MiB. Unknown, missing, null, duplicate and conflicting
case variants of keys are rejected without echoing their values. File names are
single relative components and unique per job. Dataset membership is checked at
component boundaries. Offsets/sizes must align to a requested block size that is
a multiple of 512 bytes. The caller must additionally establish actual device
alignment and filesystem support; this arithmetic check cannot discover them.

Allocation is conservatively bounded by the sum of each unique file's
`offset_bytes + size_bytes`, plus explicit headroom. The in-flight buffer lower
bound is the sum of `depth × block_size_bytes`; actual fio/process overhead and
target capacity must be budgeted separately by a future runner. All arithmetic
uses checked unsigned integers. Image references require a SHA-256 digest.

`Manifest.CheckIdentity` compares a separately collected identity tuple against
the request. Device names/major:minor are deliberately not stable identity keys.
A matching tuple cannot prove freshness, complete host/target visibility, safe
ownership or exclusion of another writer. No collector or workload admission
path exists yet. Lexical path checks cannot detect symlinks or a mount changing
between observation and use.

`EvaluateFio` takes a valid manifest, an independently observed `ProcessReceipt`
and unmodified final stdout. The caller must retain raw stdout, stderr and the
process receipt even when evaluation fails. The evaluator:

- Requires a known exit, complete streams and confirmed child exits; timeout,
  cancellation, signal or process error fails even when stdout looks healthy.
- Parses exactly one JSON/JSON+ document up to 32 MiB, without stripping a
  diagnostic prefix or accepting successive status documents. Duplicate keys
  include Unicode case aliases recognized by Go's JSON decoder, such as long s
  in `short_ios`; literal and escaped spellings receive the same check.
- Matches every unique expected job by name, checks all job errors and optional
  continued-error fields, and retains observations from other valid jobs.
- Requires explicit canonical per-job options for filename, offset, size,
  io_size, block size, depth, phase, one job, one loop, no group/time-based
  reporting and verification disabled. See the fixture for exact strings;
  suffixes/inherited defaults are not normalized in this first format.
- Checks read/write/trim bytes and operation counts separately for each job,
  including required short/dropped-I/O counters. A matching aggregate cannot
  hide an absent job or activity in an unexpected direction.
- Preserves missing optional rates/latencies as null and nanosecond percentiles
  per job. Invalid metric values fail; percentiles are never averaged or pooled.
  An explicitly null percentile sample fails instead of becoming zero.

`PASS` means only that this offline process/output/byte-accounting evaluation
passed. `verified_bytes` always remains null: counters and reported options do
not establish exact offset coverage, checksum correctness, flush durability,
writer exclusion or cleanup. Missing durable witness or runtime facts cannot
be replaced by these parser tests. Any required metric acceptance policy belongs
to the eventual scenario evaluator; optional missing metrics do not become zero.

## Capture retention

`NewEvidenceStore` takes a validated manifest and a caller-selected `*os.Root`
directory handle. The directory's group/other permission bits must be zero;
the caller must establish its ownership, filesystem security and separation
from the measured/faulted storage. The store duplicates the handle and pins
the manifest, so a later directory rename or caller-side job-plan change cannot
redirect a capture. It does not open a path from a manifest or create a default
output location.

An exclusive `<run_id>.manifest.json` reserves the run. Existing manifests,
orphaned capture files and indexes refuse reuse. `RecordCapture` accepts already
captured stdout/stderr plus a process receipt; it never launches a process. It
creates only these single-component names:

- `<run_id>.stdout.raw` and `.stderr.raw`, unchanged even when evaluation fails.
- `<run_id>.process.json`, with typed process state and allowlisted signal names.
- `<run_id>.result.json`, with evaluation and receipts for completed artifacts.

Each file uses exclusive creation and private permissions, followed by full-write,
file-sync, close and directory-sync checks. Existing files and final symlinks
are not followed or replaced. Returned receipts contain byte lengths and SHA-256
hashes. The result index receipt is returned separately, avoiding a self hash.
A file merely existing is not a completion receipt.

Inspect both `CaptureResult.Evaluation.Status` and the returned error: a failed
fio evaluation can have fully preserved evidence and a nil persistence error.
Any persistence/finalization failure forces the returned evaluation to FAIL.
Failure on one stream still attempts retention of the other stream, process
receipt and failing index. A capture is attempted once, including when it fails;
there is no retry, overwrite or deletion path. `Close` releases descriptors only.

Raw streams must come from a reviewed credential-free fio invocation. This API
is not a sanitizer for API responses or arbitrary command logs. It does not
observe process exits, supervise descendants, enforce a write timeout, discover
storage topology or prove crash durability. Those remain separate runner/lab
contracts. Tests cover concurrent reservation/recording, file/symlink collisions,
directory renames, failed output retention and injected short-write/sync/close
failures on scratch evidence files only.

## Independent read-back

`VerifyEvidence(root, indexReceipt)` reads the retained files without changing
them. Supply the index receipt returned by `RecordCapture` through a separately
trusted channel. Computing an expected receipt from the current index cannot
detect replacement of that index and its inputs. These hashes establish
consistency with the supplied receipt; they do not authenticate the collector
or prove its process observations, filesystem crash durability or storage results.

Capture-index version 2 identifies both its JSON format and evaluation rules,
independently of manifest schema 1. The saved index requires `index: null`; its
own receipt remains separate. Version 1 used weaker Unicode duplicate-key rules
and is no longer supported: it returns `ErrEvidenceVersion` without changing
saved files. Unversioned indexes also fail. Future changes to evaluation
semantics need a new version or an explicit version-specific evaluator. There
is no automatic migration.

The verifier requires exactly four uniquely named receipts for the same run's
manifest, stdout, stderr and process record. It checks private regular files,
exact lengths and hashes, required record fields/types and manifest/run identity,
then recomputes the complete fio evaluation. Missing process booleans, ambiguous
JSON keys and saved/recomputed disagreement fail. A saved PASS or invented
`verified_bytes` cannot replace this check. Correctly retained failing fio output
or an unknown exit can verify with a nil error while the evaluation remains FAIL.

Reads are limited to 1 MiB for manifests, 32 MiB each for stdout/stderr, 4 KiB
for process records and 8 MiB for indexes. Oversized streams remain preserved
unchanged by the writer, with incomplete-evidence failure; they cannot pass
read-back. Oversized manifests are refused before reservation. If an index
cannot be finalized, already-written evidence stays intact.

Linux and Darwin use native descriptor-relative no-follow opens. Symlinks are
refused even when their target matches the receipt, and nonblocking open avoids
waiting for a FIFO writer. Other platforms return `ErrEvidencePlatform`; there
is no weaker fallback. A pinned directory handle survives renames. These checks
do not impose an I/O deadline or prevent another authorized process modifying
files after verification; the caller owns directory security and writer exclusion.

On rejection, the verifier returns a sanitized error and a fresh failing result
without unverified observations. It performs no repair, retry, overwrite or
cleanup. Tests use synthetic captures and temporary evidence files, including
altered/missing files, forged conclusions, symlinks, FIFOs and renamed directories.

## Reviewed fio format and fixture provenance

The initial format is exactly `fio-3.39`, reviewed against upstream
[`stat.c`](https://github.com/axboe/fio/blob/fio-3.39/stat.c) and
[`options.c`](https://github.com/axboe/fio/blob/fio-3.39/options.c). This is a
parser support boundary, not a choice of lab runtime image or a recommendation
to change an installed fio version. Other versions fail explicitly until their
format and fixtures are reviewed.

`total_err` and `first_error` are emitted only for continue-on-error reporting;
their absence is supported. The per-job `error` field is required. Unknown
upstream output fields are tolerated, while duplicate keys remain invalid.
JSON+ histogram fields survive in the caller's raw document; this package does
not summarize or claim to validate their distributions.

Both fixture files are hand-authored synthetic controls, not captures from a
successful fio/storage run. They contain no private target identities or keys.
The tests inject malformed output, last-job errors, short coverage, reordered
jobs, missing metrics, Unicode counter aliases, bad process receipts and
credential sentinels. Integer tests include counts above the exact floating-point
integer range. Fuzz targets
exercise untrusted JSON without opening devices or spawning processes.

## Scenario coverage and remaining work

| Planning IDs | Implemented offline scope | Remaining live/tooling prerequisite |
| --- | --- | --- |
| Q01, part of Q03/Q06 | Expected/observed immutable tuple comparison and required fields | Independent collectors, source scope/freshness, device generation and actual mount checks |
| Part of Q02 | Component-boundary/lexical path rejection; evidence writes pinned to a private directory handle with exclusive file creation | Workload path symlink handling and mount check/use races |
| Q08 | Per-file totals/headroom, buffer lower bound, overflow and alignment | Actual PVC/pool capacity and memory/overhead observations |
| Q09/Q10/Q11 | Every job/error, document completeness, optional metrics, explicit-null percentile rejection and integer preservation | Real sanitized success/error captures and required-metric scenario policy |
| Part of Q12/Q13 | Per-job counts, distinct file plans and reported options | Independent exact-extent/checksum verification across processes |
| Part of Q15/Q16/Q18 | Version check, fake termination/stream evidence, sanitized findings/receipt signals, preservation on persistence failure | Capability probes, real process tracking/cancellation and credential-safe adapters |
| Part of Q17 | Exclusive run reservation/capture writes and independent receipt-based read-back; no overwrite or automatic retry/cleanup | Trusted receipt retention, audited exact-resource cleanup and crash reconciliation |
| Q04/Q05/Q07/Q14/Q19/Q20 | Not implemented in this slice | Visibility/multipath/caps, actual corruption detection, XFS tools and raw-block regression |

All B/I/L/F/H/A/U/C target scenarios, actual scratch-file corruption, runtime
images, durability witness, fault injection and benchmarks are **NOT RUN**.
The baseline contains no performance package, so there is no baseline parser
test result. Healthy/error controls qualify the new tooling only; they do not
demonstrate that a baseline production driver/kernel defect was fixed.

## Local checks

From the repository root:

```sh
GOTOOLCHAIN=go1.26.2 go test -short -race -shuffle=on -cover ./tests/performance/...
golangci-lint run --config .golangci.yml ./tests/performance/...
GOTOOLCHAIN=go1.26.2 make check
make nolint-budget
```

`make test-unit` includes this offline package. The existing production coverage
scope and floors are unchanged; report helper coverage separately. Tests have no
live setup in init/TestMain. Future executable adapters must remain explicit
opt-in consumers, use a reviewed disposable lab and preserve failure evidence
before any existing E2E cleanup callbacks run.

## Validation recorded 2026-10-18

At base `59f8373846f84e3c4606c0c8a1ceba04a889f4ab` plus this offline slice:

| Check | Result |
| --- | --- |
| Focused Go 1.26.2 shuffled race tests, darwin/arm64 and linux/arm64 | PASS; helper statement coverage 95.5% |
| golangci-lint 2.14.0, focused and repository-wide | PASS |
| Go 1.26.2 `make check`, darwin/arm64 | PASS, including 80 required CSI sanity cases |
| `make nolint-budget`, `git diff --check` | PASS; no suppression or floor changes |
| Both fuzz targets, separate `-fuzztime=5s -parallel=2` runs | PASS |
| `make coverage-check`, darwin/arm64 | FAIL: unchanged macOS mount implementation 19.0%, below 57% floor |
| `make coverage-check`, linux/arm64 | PASS with unchanged floors; mount package 86.7% |

Linux used the existing official Go image
`golang@sha256:820153096fda4de6ac1b7194d12e416b7dd8f4e9a33d17e0345b111b945162d8`,
with `GOTOOLCHAIN=go1.26.2` for tests and the Makefile-pinned Go 1.27.2 coverage
tool. Only selected source/configuration files were streamed into its disposable
filesystem. It had no host storage mounts, target devices or target credentials.
No runtime image was built. A coverage comparison against a selected upstream
main build and all target/lab qualification remain NOT RUN.

The subsequent capture-retention slice passed the same focused shuffled race
tests on darwin/arm64 and linux/arm64, with overall helper coverage 93.1%, clean
focused lint, `make check` (including 80 sanity cases), the nolint budget and
diff checks. Production source/dependencies and the coverage policy did not
change; the production coverage gate was not repeated for capture retention.
These results do not constitute scratch workload or target qualification.

The read-back slice passed Go 1.26.2 shuffled race tests on darwin/arm64 (93.1%
helper coverage) and linux/arm64 (92.8%), focused/repository lint, `make check`
(80 sanity cases), the unchanged nolint budget and diff checks. All three fuzz
targets passed separate five-second runs with two workers. The unsupported-
platform path compiled for windows/amd64; Windows tests were not executed.
The explicit-null percentile regression fails against published commit
`2295970618d50c864e03a31943c99b0d58131803` and passes with this correction.
Linux used the same pinned disposable container/source-stream setup above.
Production code, module versions and coverage policy are unchanged; the
production coverage gate was not repeated for this offline slice.

The Unicode-key correction passed focused shuffled race tests on darwin/arm64
and linux/arm64 (92.7% helper coverage), focused/repository lint, `make check`
(80 sanity cases), the unchanged nolint budget and diff checks. All four fuzz
targets passed separate five-second/two-worker runs. The four counter-alias
regressions failed against `bb96f08` before the correction. No production code,
dependency version or coverage policy changed.
