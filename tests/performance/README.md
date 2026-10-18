# Offline performance evidence

This package validates a finite file-test request, evaluates captured fio output,
preserves supplied captures and verifies their read-back against a trusted receipt.
It also checks finite extents against independently supplied SHA-256 digests.
Checksum plans can be retained before writing data and checked before reuse.
Finalized witness records can be compared with recovery and delivered replies.
The witness store records commits and seals a generation for finalization.
The append-file writer orders target syncs before witness submission, and the
recovery reader verifies actual headers and payloads against that final ledger.
Target creation and recovery opens can use an explicitly supplied private
directory handle, with exclusive creation and no-follow read-only recovery.
It does not connect to Kubernetes/TrueNAS, run fio, inspect a mount, change
settings or delete data. Tests use synthetic fixtures, fake process receipts and
temporary local directories. No production driver behavior changes are included.

## Request and evaluation

`DecodeManifest` requires all fields shown in
[`testdata/manifest.json`](testdata/manifest.json), with their exact case and
types. Schema 1 currently covers separate finite read and write phases on XFS
or ext4. Mixed/time-based profiles and raw devices need additional contracts
before use. Finite checksum verification uses the separate helper below.

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

## Finite checksum verification

`VerifyChecksums(ctx, manifest, expected, sources)` verifies a finite read-phase
manifest against `ExtentChecksum` records. Each record binds the complete job
plan to a canonical SHA-256. Supply one caller-owned `io.ReaderAt` per filename.
All plans, digests and reader bindings are validated and pinned before any read;
missing/duplicate records, changed extents, unavailable readers and offsets
outside ReaderAt's signed range reject the entire request without data I/O.

Establish and retain the expected checksums independently before writing the
test data, together with the corresponding manifest. Hashing the current file
to manufacture its expected checksum cannot detect prior corruption. The caller
owns source identity, read-only opening, flush/close ordering and the trusted
association between the saved plan and readers. This helper opens/closes no
files, discovers no mounts and admits no live workload.

The verifier reads only each declared offset/length, using a shared buffer of
at most 64 KiB, also capped by the request's buffer budget. Short reads, invalid
reader counts and reader errors fail without retry. `ReadBytes` records actual
coverage; per-job `VerifiedBytes` is set only when that entire extent's digest
matches. Aggregate `VerifiedBytes` remains null unless every planned extent
matches. A failing job preserves successful observations from other jobs.
`EvaluateFio.VerifiedBytes` still always remains null; the two results make
different assertions.

Cancellation stops subsequent reads and leaves incomplete verification as FAIL.
It cannot interrupt a blocked ReaderAt, prove kernel I/O stopped, establish
writer exclusion or demonstrate crash durability. No write, repair or cleanup
path exists. Tests write nonzero payloads to temporary regular files, check
sync/close, then reopen read-only: a byte flipped inside an extent and a short
file fail, while a changed prefix outside the declared extent does not count
toward verification. These are scratch-file tooling checks, not NVMe/XFS results.

## Retained checksum plans

`SaveChecksumPlan(root, manifest, expected)` saves the intended **read-phase**
manifest and its complete expected checksum records to
`<run_id>.checksums.json`. Establish those digests independently, save the plan
successfully before writing test data, and keep the returned `ArtifactReceipt`
in a separate trusted location. A plan file's existence cannot prove that its
digests were computed before corruption or that its save completed.

The caller selects a private evidence directory outside the measured storage.
Saving uses exclusive creation, private file permissions and checked full-write,
file-sync, close and directory-sync completion. Existing files and symlinks
refuse reuse; failed artifacts remain intact without overwrite, retry or cleanup.
The caller's root remains open. This is an independent version-1 format, capped
at 2 MiB for the manifest and at most 1024 bound digest records. It does not
change capture-index version 2 or its four-artifact receipt format.

`LoadChecksumPlan(root, receipt, manifest)` requires the separately trusted receipt
and the full intended read manifest. It checks the private regular file through
a descriptor-relative no-follow open, its exact size/hash, required record shape,
format version, every bound extent/digest and exact manifest equality. Changed
run/volume identities, budgets or job plans fail. Unsupported versions fail
explicitly; any error returns no checksum records. No workload file is opened.
Pass the successfully loaded records to `VerifyChecksums` with independently
identified read-only sources. Loading performs no writes or data-path admission.

Linux and Darwin support the reviewed no-follow read path; other platforms reject
loading explicitly. Caller-owned directory security, source identity, writer
exclusion and process ordering remain required. The scratch regression saves a
plan before creating data files, reopens both and detects a changed data byte.
Other controls cover malformed records, substituted manifests/receipts, concurrent
savers, symlinks and retained evidence. These checks do not prove crash durability.

## Finalized witness reconciliation

`ReconcileWitness(stream, receipt, rawLedger, recovered, replies)` compares a
finalized witness prefix with independently collected target records. The caller
supplies the expected run/stream/generation and a separately trusted receipt for
`<run_id>.witness.json`. The witness finalizer must issue that receipt only after
reconciling accepted commits, closing the generation to further appends and
durably retaining the ledger. Computing a receipt from the current ledger cannot
establish that trust. The local store below handles witness commit/finalization;
network transport, target collection and fault control remain unimplemented.

Ledger format version 1 requires every `WitnessLedger` field, with a nonempty
sequence prefix starting at 1 and ending at `final_sequence`. Records bind the
complete stream identity, sequence, payload offset/length and canonical SHA-256.
Offsets must be ordered and nonoverlapping; byte arithmetic is checked. The
strict JSON, exact length/hash and supported-version checks reject torn or
substituted ledgers before using their observations. Ledgers are capped at 2 MiB
and 4096 entries; recovered/reply lists are separately capped at 4096 entries.

Recovered records must describe complete independently read and hashed payloads,
including their actual header identities and offsets. The collector must check
that required records' stored header checksums agree with computed payload
checksums and fail collection on required-record read/header errors. Incomplete
unwitnessed tail bytes need a separate collector report. Records must not be copied
from expected witness records. The caller owns collection completeness, immutable
volume identity and read-only access. The helper performs no I/O and cannot prove
that these observations or finalization claims are truthful.

Every durable ledger entry must match recovery, including an entry whose witness
commit reply never reached the writer. Missing entries, metadata/hash mismatches,
conflicting replies and duplicate recovered sequences fail. Identical reply
retries count once. A reply outside the finalized ledger fails; replies are
reported separately and cannot shrink the required recovery set. Recovered
sequences beyond the durable prefix are reported as a sorted unwitnessed tail.
They cannot overlap the prefix or each other and never add verified bytes.

`VerifiedRecords` preserves matching prefix coverage on comparison failures;
aggregate `VerifiedBytes` remains null unless the complete comparison passes.
The byte total counts witnessed payload bytes only.
Invalid ledgers or malformed record sets expose no trusted coverage. Findings
use stable codes and sequence numbers without echoing hashes or free-form input.
PASS covers this comparison only; process drain, writer exclusion, actual fsync
durability, independent witness storage qualification and live faults remain
separate gates.

## Witness commit and finalization

`NewWitnessStore(root, stream)` reserves one fresh run in a caller-selected
private directory outside the target and fault domains. Use a directory for one
run, with no other writer modifying its witness files. The store duplicates and
pins the supplied root. Linux and Darwin support the required no-follow reads;
other platforms reject construction. Existing run artifacts, including orphan
entries or a final ledger, refuse reuse. There is no restart/resume API.

The reservation is `<run_id>.witness.start.json`. `Append(record)` requires the
exact stream/generation, next contiguous sequence, a valid nonoverlapping payload
extent and canonical SHA-256. The caller must complete target writes and required
target file/directory syncs **before** submitting a record; the witness store
cannot observe them. Each accepted record is exclusively written to
`<run_id>.witness.<20-digit-sequence>.json`, with schema version and record fields.
The returned receipt follows checked full-write, file-sync, close and directory-
sync completion. It proves witness artifact persistence through those calls,
not target durability or fault admission.

An identical retry checks the private regular artifact against its original
receipt and returns that receipt without another write. Conflicting duplicates,
invalid records, sequence gaps, overlaps and persistence errors make the instance
refuse all further appends and finalization. A changed/missing reservation also
prevents new commits. Partial files remain preserved. Restoring a file or clearing
a collision cannot reopen the instance. Failed writes/validation never trigger
retry, overwrite or cleanup. Identical successful requests remain idempotent.

`Finalize()` seals the generation under the same mutex as appends, verifies the
reservation and every accepted entry against retained receipts, rejects extra
run artifacts, then exclusively writes the nonempty `<run_id>.witness.json`
ledger. Every accepted commit is included, even if its reply was lost. A racing
append either commits before the seal and is included, or returns
`ErrWitnessSealed`. The caller must drain the target writer and account for
requests not yet submitted. No later append is admitted, including a retry.
Successful repeated finalization verifies and returns the original ledger
receipt without writing. Failed or empty finalization cannot be retried.

`LoadWitnessLedger(root, stream, receipt)` reads the final ledger through the
private descriptor-relative no-follow path and authenticates its full format,
prefix and identity. It returns no bytes on error. Retain the final receipt
independently and supply the loaded bytes to `ReconcileWitness`. An entry receipt
cannot substitute for this finalization receipt. `Close` releases only the owned
descriptor; it does not finalize or remove evidence. Abandoned runs stay
unfinalized, and new store construction refuses to resume them.

Entries are capped at 2 KiB and the finalized ledger at 2 MiB/4096 records.
Append reserves a conservative serialized ledger budget before accepting each
entry. Directory scans use bounded batches and reject more than 16,384 entries,
including unrelated files. Tests cover lost replies, concurrent ownership and
finalization, file alterations/symlinks, closed/renamed roots and failures that
prevent continuation. Temporary-file sync checks are not power-loss tests or
qualification of a live witness service.

## Witness target writes and recovery

`NewWitnessFileWriter(plan, target, parent, witness)` borrows an empty regular
target file, its caller-identified parent directory and a `WitnessCommitter`.
The caller creates and opens those handles safely, establishes target identity,
exclusive ownership and fault-domain separation, and keeps them open. Constructor
checks cannot establish the parent relationship or ownership. `WitnessStore`
implements the committer interface; no network transport is included.

`WitnessFilePlan` binds the stream and limits total file capacity, payload size
and record count. Capacity includes headers and must fit signed file offsets.
Payloads are limited to 32 MiB and records to 4096. The writer keeps a private
payload copy bounded by the planned maximum; the caller's original allocation
and operating-system overhead are additional. A plan does not admit live I/O.

`Append(ctx, payload)` pins the payload and computes SHA-256 before writing its
header and complete payload. Full writes, exact file size, target sync and the
first record's parent-directory sync must succeed before witness submission.
The returned receipt must match the canonical version-1 witness entry exactly.
Target errors permanently stop the writer, retaining partial bytes without
rewrite or cleanup. An absent, invalid or canceled witness reply returns
`ErrWitnessPending` with submitted metadata and no receipt. New target writes
are blocked until explicit `RetryWitness` resolves that same request; retry
never reads, writes or syncs the target or parent. `Close` stops calls without
flushing, closing borrowed handles or finalizing the witness; `Pending` retains
unresolved metadata for reconciliation. There is no restart/resume API.

Target header format version 1 is independent of ledger version 1 and capture
version 2. Each record starts with a 1024-byte header: `TNSWIT01`, big-endian
64-bit version and JSON-body length, a 32-byte body SHA-256, eight reserved zero
bytes, then a strict `WitnessRecord` JSON body and zero padding. The body is at
most 960 bytes. Payload offsets include preceding headers; records are contiguous
from file offset zero. Headers never contribute verified payload bytes.

`VerifyWitnessFile(ctx, plan, source, receipt, rawLedger, replies)` borrows an
independently identified read-only regular file. It authenticates the finalized
ledger, reply membership and finite layout before any source call, then reads
each required header and hashes every complete payload. Header identity,
checksum, strict fields, padding and payload digest must match. Healthy record
coverage remains visible when another required record fails; aggregate
`VerifiedBytes` is unknown on any failure. Cancellation and observed file size
or modification-time changes fail verification. Those checks do not establish
exclusion or detect every concurrent change; the caller must ensure exclusion.

`ReadBytes` includes actual header and payload reads. `UnwitnessedTailBytes`
reports stable file bytes after the authenticated prefix, including incomplete
headers/payloads; the reader does not inspect or verify that suffix. A prefix
PASS cannot erase producer errors, failed syncs or uncertain process results;
the supervising workload must retain those as failures. Context checks occur
between calls and cannot interrupt blocked I/O or witness submission.

Scratch controls exercise this sequence with real temporary files and the local
witness store. Workload root selection/opening, immutable source identity,
exclusion, independent witness transport/storage, process supervision and crash/fault
qualification remain integration obligations. No runtime admission or fault
controller is included.

## Anchored witness target handles

`CreateWitnessTarget(root, plan)` creates `<run_id>.witness-data.bin` with
exclusive creation, write-only access and mode 0600 inside an already selected
private `*os.Root`. It returns the target and an independently owned handle to
that same directory for `NewWitnessFileWriter`. The filename derives from the
validated run ID; no caller path or generation can redirect it or resume an
existing file. Existing files, directories, FIFOs and symlinks are refused.
Creation does not sync the empty file; the writer performs the required target
and initial directory syncs before witness submission. Failures retain created
files without cleanup, truncation or retry.

`OpenWitnessSource(root, plan)` opens the same name read-only through the pinned
directory, using native no-follow/nonblocking open before regular-file checks.
It rejects symlinks, nonregular files, group/other permissions and files larger
than the finite capacity. It reads no payloads and authenticates no content;
pass the handle to `VerifyWitnessFile` with the independently trusted ledger.
An empty or truncated file may open but cannot satisfy required recovery reads.

Both functions validate the plan before directory access, require a private
directory, and support the reviewed Linux/Darwin path only. Renaming the
directory or replacing its old pathname cannot redirect either open. The
caller owns all returned handles; closing the supplied root does not close them.
Close the writer before closing its target/parent, and close each returned file
after use. Internal close/open failures expose only sanitized errors.

The caller still selects and opens the correct workload root, establishes its
immutable storage identity and ownership, prevents concurrent replacement or
modification, and separates witness storage from the fault domain. A private
directory, pinned handle or exclusive file creation cannot establish those
facts or admit a live workload. Formats and existing borrowed-handle APIs are
unchanged; no automatic source discovery, resume or cleanup is included.

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
| Part of Q02 | Component-boundary/lexical rejection; evidence/target creation pinned to private handles; exclusive target creation and no-follow recovery despite directory renames | Workload root selection, identity and mount check/use races |
| Q08 | Per-file totals/headroom, buffer lower bound, overflow and alignment | Actual PVC/pool capacity and memory/overhead observations |
| Q09/Q10/Q11 | Every job/error, document completeness, optional metrics, explicit-null percentile rejection and integer preservation | Real sanitized success/error captures and required-metric scenario policy |
| Part of Q12/Q13/Q14 | Per-job counts, distinct plans/options, finite SHA-256 extent checks and independently receipted checksum plans; scratch corruption/short-read controls | Independent reader/source identity, trusted receipt custody, write/read process ordering and lab integration |
| Part of Q15/Q16/Q18 | Version check, fake termination/stream evidence, sanitized findings/receipt signals, preservation on persistence failure | Capability probes, real process tracking/cancellation and credential-safe adapters |
| Part of Q17 | Exclusive run reservation/capture writes and independent receipt-based read-back; no overwrite or automatic retry/cleanup | Trusted receipt retention, audited exact-resource cleanup and crash reconciliation |
| Part of fault-durability evidence | Anchored target handles, write/sync ordering, checked witness commit/finalization, actual header/payload recovery, lost-reply reconciliation and separate unwitnessed-tail accounting | Workload root selection, independent source/storage identity, witness transport, process supervision, ownership/fault admission and live durability qualification |
| Q04/Q05/Q07/Q19/Q20 | Not implemented in this slice | Visibility/multipath/caps, XFS tools and raw-block regression |

All B/I/L/F/H/A/U/C target scenarios, runtime images, live durability witness,
target fault injection and benchmarks are **NOT RUN**. Scratch checksum tests
exercise regular temporary files only.
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

The finite-checksum slice passed the Go 1.26.2 focused race tests, complete
`make check` on darwin/arm64 (including 80 sanity cases), Linux/arm64 shuffled
race tests and the unchanged nolint budget (283 pairs). The cancellation-between-
extents regression failed before its fix and passes afterward. Scratch-file
controls detect one-byte corruption and short reads without modifying data.
No dedicated fuzz target or session was added/run. Production code, dependencies
and coverage policy are unchanged; production coverage was not repeated locally.

The checksum-plan retention slice passed focused Darwin/Linux shuffled race
tests, complete `make check` (80/80 CSI sanity cases), the unchanged nolint
budget (283 pairs) and diff checks. Controls retain the plan before creating
scratch data and detect corruption after reopening; altered plans, mismatched
manifests, symlinks and concurrent saves are rejected. No dedicated fuzz test or
session was added/run. Production code, dependencies and coverage policy are
unchanged; production coverage was not repeated locally.

The finalized-witness slice passed Go 1.26.2 focused shuffled race tests on
Darwin/Linux, complete `make check` (80/80 CSI sanity cases), the unchanged
nolint budget (283 pairs) and diff checks. Controls cover lost replies, missing
durable entries, exact retry accounting, conflicting records, separate tail
coverage and invalid finalized evidence. No dedicated fuzz test/session was
added/run. Production code, dependencies and coverage policy are unchanged;
production coverage was not repeated locally. These are offline comparison
checks; no witness service, target collector or fault was run.

The witness-store slice passed Go 1.26.2 focused Darwin/Linux shuffled race
tests, complete `make check` (80/80 CSI sanity cases), the unchanged nolint
budget (283 pairs) and diff checks. Controls cover persistent commit/finalize/
reopen, lost replies, concurrent owners and sealing, altered artifacts, sticky
failures and private descriptor lifetime. No dedicated fuzz test/session was
added/run. Production code, dependencies and coverage policy are unchanged;
production coverage was not repeated locally. No network witness service,
target writer/collector, live workload or fault was run.

The target-file slice passed Go 1.26.2 focused Darwin/Linux shuffled race tests,
complete `make check` (80/80 required CSI sanity cases), repository lint, the
unchanged nolint budget (283 pairs) and diff checks. Real temporary-file controls
cover append/sync/commit/finalize/read-only recovery, lost replies, witness-only
resolution, partial writes, corrupted headers/payloads, cancellation and retained
unwitnessed suffixes. No dedicated fuzz test/session was added/run. Production
code, dependencies and coverage policy are unchanged; production coverage was
not repeated locally. No network witness service, live workload or storage fault
was run; scratch syscall ordering does not establish crash durability.

The anchored-target slice passed Go 1.26.2 focused Darwin/Linux shuffled race
tests, complete `make check` (80/80 required CSI sanity cases), repository lint,
the unchanged nolint budget (283 pairs) and diff checks. Controls exercise
exclusive creation, existing-byte retention, symlink/FIFO refusal, private/size
bounds, concurrent creation, generation reuse and real write/recovery after root
close and directory pathname replacement. The unsupported-platform path compiled
for Windows/amd64; Windows tests were not executed. No dedicated fuzz target or
session was added/run. Production code, dependencies and coverage policy remain
unchanged; production coverage was not repeated locally. No live storage
workload or fault was run.
