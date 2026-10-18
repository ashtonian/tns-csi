package performance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"sync"
)

// ErrWitnessPending requires resolution of a submitted witness request before
// another target record can be written. A missing reply is not a failed commit.
var ErrWitnessPending = errors.New("performance witness acknowledgement pending")

// WitnessFileTarget borrows an exclusively owned empty regular file. The caller
// creates it safely, establishes its identity and keeps the handle open.
type WitnessFileTarget interface {
	io.WriterAt
	Stat() (os.FileInfo, error)
	Sync() error
}

// WitnessFileDirectory borrows the target file's caller-identified parent.
type WitnessFileDirectory interface {
	Stat() (os.FileInfo, error)
	Sync() error
}

// WitnessCommitter submits metadata to independent witness storage. WitnessStore
// implements it locally; transport authentication and deadlines are external.
type WitnessCommitter interface {
	Append(WitnessRecord) (ArtifactReceipt, error)
}

// WitnessWriteResult distinguishes a flushed target record with an unresolved
// witness request from an acknowledged witness commit. Errors expose no receipt.
type WitnessWriteResult struct {
	Receipt *ArtifactReceipt `json:"receipt"`
	Record  WitnessRecord    `json:"record"`
}

// WitnessFileWriter serializes target write, target sync, initial parent sync and
// witness submission. It never opens/closes handles, rewrites a record or retries
// target I/O. The caller owns identity/ownership admission and fault isolation.
// Context checks cannot interrupt a blocked WriteAt, Sync or committer call.
type WitnessFileWriter struct {
	target       WitnessFileTarget
	directory    WitnessFileDirectory
	witness      WitnessCommitter
	pending      *WitnessRecord
	expected     ArtifactReceipt
	plan         WitnessFilePlan
	mu           sync.Mutex
	end          uint64
	sequence     uint64
	parentSynced bool
	failed       bool
	closed       bool
}

// NewWitnessFileWriter requires a valid finite plan, an empty regular target and
// its parent directory. These handle checks do not establish their relationship
// or exclusive ownership; the caller must provide both before construction.
func NewWitnessFileWriter(plan WitnessFilePlan, target WitnessFileTarget, directory WitnessFileDirectory, witness WitnessCommitter) (*WitnessFileWriter, error) {
	if !validWitnessFilePlan(plan) || target == nil || directory == nil || witness == nil {
		return nil, ErrInvalidWitness
	}
	info, err := target.Stat()
	if err != nil || info == nil || !info.Mode().IsRegular() || info.Size() != 0 {
		return nil, ErrInvalidWitness
	}
	parent, err := directory.Stat()
	if err != nil || parent == nil || !parent.IsDir() {
		return nil, ErrInvalidWitness
	}
	return &WitnessFileWriter{target: target, directory: directory, witness: witness, plan: plan}, nil
}

// Append pins the payload and computes its expected digest before any write.
// It acknowledges only a matching witness receipt after required target syncs.
// Any target error stops the writer permanently with the data retained. A witness
// error leaves a pending record; only RetryWitness may then submit it again.
func (w *WitnessFileWriter) Append(ctx context.Context, payload []byte) (WitnessWriteResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if stateErr := w.available(); stateErr != nil {
		return w.unresolved(), stateErr
	}
	if ctxErr := witnessContextError(ctx); ctxErr != nil {
		return w.fail(ctxErr)
	}
	if len(payload) == 0 || uint64(len(payload)) > w.plan.MaxPayloadBytes || w.sequence >= w.plan.MaxRecords {
		return w.fail(ErrInvalidWitness)
	}
	offset, valid := add(w.end, WitnessHeaderBytes)
	end, validEnd := add(offset, uint64(len(payload)))
	if !valid || !validEnd || end > w.plan.CapacityBytes {
		return w.fail(ErrInvalidWitness)
	}
	if sizeErr := w.checkSize(w.end); sizeErr != nil {
		return w.fail(ErrEvidenceIncomplete)
	}
	payload = bytes.Clone(payload)
	hash := sha256.Sum256(payload)
	record := WitnessRecord{Stream: w.plan.Stream, Sequence: w.sequence + 1, OffsetBytes: offset, PayloadBytes: uint64(len(payload)), SHA256: hex.EncodeToString(hash[:])}
	header, err := encodeWitnessHeader(record)
	if err != nil {
		return w.fail(ErrInvalidWitness)
	}
	w.expected, err = witnessRecordReceipt(record)
	if err != nil {
		return w.fail(ErrInvalidWitness)
	}
	if writeErr := w.writeAndSync(ctx, header, payload, offset, end); writeErr != nil {
		return w.fail(writeErr)
	}
	w.end, w.pending = end, &record
	return w.submit(ctx)
}

func (w *WitnessFileWriter) writeAndSync(ctx context.Context, header, payload []byte, offset, end uint64) error {
	if ctxErr := witnessContextError(ctx); ctxErr != nil {
		return ctxErr
	}
	if writeErr := writeWitnessBytes(w.target, header, w.end); writeErr != nil {
		return writeErr
	}
	if ctxErr := witnessContextError(ctx); ctxErr != nil {
		return ctxErr
	}
	if writeErr := writeWitnessBytes(w.target, payload, offset); writeErr != nil {
		return writeErr
	}
	if ctxErr := witnessContextError(ctx); ctxErr != nil {
		return ctxErr
	}
	if sizeErr := w.checkSize(end); sizeErr != nil {
		return sizeErr
	}
	if ctxErr := witnessContextError(ctx); ctxErr != nil {
		return ctxErr
	}
	if syncErr := w.target.Sync(); syncErr != nil {
		return ErrEvidenceIncomplete
	}
	if ctxErr := witnessContextError(ctx); ctxErr != nil {
		return ctxErr
	}
	if !w.parentSynced {
		if syncErr := w.directory.Sync(); syncErr != nil {
			return ErrEvidenceIncomplete
		}
		w.parentSynced = true
	}
	return witnessContextError(ctx)
}

func (w *WitnessFileWriter) checkSize(expected uint64) error {
	info, err := w.target.Stat()
	if err != nil || info == nil || !info.Mode().IsRegular() {
		return ErrEvidenceIncomplete
	}
	size := info.Size()
	if size < 0 || uint64(size) != expected {
		return ErrEvidenceIncomplete
	}
	return nil
}

// RetryWitness resubmits only the already flushed pending metadata. It never
// touches the target or parent. It is the sole retry path after an uncertain
// witness reply and is explicit rather than an automatic retry policy.
func (w *WitnessFileWriter) RetryWitness(ctx context.Context) (WitnessWriteResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return w.unresolved(), ErrEvidenceClosed
	}
	if w.failed {
		return WitnessWriteResult{}, ErrWitnessFailed
	}
	if w.pending == nil {
		return WitnessWriteResult{}, ErrInvalidWitness
	}
	if err := witnessContextError(ctx); err != nil {
		return w.unresolved(), err
	}
	return w.submit(ctx)
}

func (w *WitnessFileWriter) submit(ctx context.Context) (WitnessWriteResult, error) {
	receipt, err := w.witness.Append(*w.pending)
	if err != nil || receipt != w.expected || witnessContextError(ctx) != nil {
		return w.unresolved(), ErrWitnessPending
	}
	result := WitnessWriteResult{Record: *w.pending, Receipt: &receipt}
	w.sequence, w.pending = w.pending.Sequence, nil
	return result, nil
}

func (w *WitnessFileWriter) unresolved() WitnessWriteResult {
	if w.pending != nil {
		return WitnessWriteResult{Record: *w.pending}
	}
	return WitnessWriteResult{}
}

func (w *WitnessFileWriter) available() error {
	switch {
	case w.closed:
		return ErrEvidenceClosed
	case w.failed:
		return ErrWitnessFailed
	case w.pending != nil:
		return ErrWitnessPending
	default:
		return nil
	}
}

func (w *WitnessFileWriter) fail(err error) (WitnessWriteResult, error) {
	w.failed = true
	return WitnessWriteResult{}, err
}

// Close stops new calls without flushing, closing handles or finalizing witness
// storage. An unresolved record remains available in Pending for reconciliation.
func (w *WitnessFileWriter) Close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
}

// Pending returns a copy of submitted metadata whose receipt is unresolved.
func (w *WitnessFileWriter) Pending() (WitnessRecord, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.pending == nil {
		return WitnessRecord{}, false
	}
	return *w.pending, true
}

func witnessRecordReceipt(record WitnessRecord) (ArtifactReceipt, error) {
	raw, err := json.Marshal(witnessEntry{Record: record, SchemaVersion: WitnessVersion})
	if err != nil || len(raw) > maxWitnessEntryBytes {
		return ArtifactReceipt{}, ErrInvalidWitness
	}
	hash := sha256.Sum256(raw)
	return ArtifactReceipt{Name: witnessEntryName(record.Stream.RunID, record.Sequence), Bytes: uint64(len(raw)), SHA256: hex.EncodeToString(hash[:])}, nil
}

func writeWitnessBytes(target io.WriterAt, raw []byte, offset uint64) error {
	if offset > math.MaxInt64 {
		return ErrEvidenceIncomplete
	}
	n, err := target.WriteAt(raw, int64(offset))
	if err != nil || n != len(raw) {
		return ErrEvidenceIncomplete
	}
	return nil
}

func witnessContextError(ctx context.Context) error {
	if ctx == nil {
		return ErrInvalidWitness
	}
	if err := ctx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return context.DeadlineExceeded
		}
		return context.Canceled
	}
	return nil
}
