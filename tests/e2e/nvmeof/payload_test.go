package nvmeof

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/fenio/tns-csi/tests/e2e/framework"
)

var (
	errPayloadMismatch  = errors.New("read-back payload differs from written payload")
	errPayloadAlignment = errors.New("payload size is not a positive multiple of the logical block size")
	errPayloadZero      = errors.New("random payload is all zeros")
	errPayloadCapacity  = errors.New("payload exceeds device capacity")
	errUnexpectedOutput = errors.New("unexpected command output")
	errMountNotFound    = errors.New("mount point not found in /proc/mounts")
)

// podExec runs argv directly in a test pod, without a shell, and returns its
// trimmed stdout.
type podExec func(ctx context.Context, argv []string) (string, error)

func podRunner(f *framework.Framework, podName string) podExec {
	return func(ctx context.Context, argv []string) (string, error) {
		return f.K8s.ExecInPod(ctx, podName, argv)
	}
}

// payloadDigest identifies finite data by exact byte count and SHA-256.
type payloadDigest struct {
	sha256 string
	bytes  int64
}

func (d payloadDigest) String() string {
	return fmt.Sprintf("%d bytes, sha256 %s", d.bytes, d.sha256)
}

// writeBlockPayload writes size random bytes at offset zero of a raw block
// device and verifies an exact-length read-back. size must be a multiple of the
// device's logical block size; the payload is never padded or truncated to fit.
// The returned digest is computed from the pod-local source, not the device.
func writeBlockPayload(ctx context.Context, run podExec, device string, size int64) (payloadDigest, error) {
	blockSize, err := logicalBlockSize(ctx, run, device)
	if err != nil {
		return payloadDigest{}, err
	}
	if size <= 0 || size%blockSize != 0 {
		return payloadDigest{}, fmt.Errorf("%w: %d bytes, logical block %d", errPayloadAlignment, size, blockSize)
	}
	capacity, err := blockdevInt(ctx, run, "--getsize64", device)
	if err != nil {
		return payloadDigest{}, err
	}
	if size > capacity {
		return payloadDigest{}, fmt.Errorf("%w: %d bytes, device %d", errPayloadCapacity, size, capacity)
	}

	const source = "/tmp/block-payload.bin"
	blocks := ddBlocks(blockSize, size)
	want, err := randomSource(ctx, run, source, blocks, size)
	if err != nil {
		return payloadDigest{}, err
	}
	write := append([]string{"dd", "if=" + source, "of=" + device, "conv=notrunc,fsync"}, blocks...)
	if _, err = run(ctx, write); err != nil {
		return payloadDigest{}, fmt.Errorf("write payload to %s: %w", device, err)
	}
	return want, verifyBlockPayload(ctx, run, device, want)
}

// verifyBlockPayload reads want.bytes from offset zero of device and requires
// the exact length and SHA-256. Within one attachment the read can be served
// from the page cache: the pinned BusyBox dd has no O_DIRECT and dropping
// caches needs privileges. Verify from a new pod after re-attachment to read
// from the target.
func verifyBlockPayload(ctx context.Context, run podExec, device string, want payloadDigest) error {
	blockSize, err := logicalBlockSize(ctx, run, device)
	if err != nil {
		return err
	}
	if want.bytes <= 0 || want.bytes%blockSize != 0 {
		return fmt.Errorf("%w: %d bytes, logical block %d", errPayloadAlignment, want.bytes, blockSize)
	}

	const readBack = "/tmp/block-readback.bin"
	read := append([]string{"dd", "if=" + device, "of=" + readBack}, ddBlocks(blockSize, want.bytes)...)
	if _, err = run(ctx, read); err != nil {
		return fmt.Errorf("read payload from %s: %w", device, err)
	}
	got, err := fileDigest(ctx, run, readBack)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("%w: %s: wrote %s, read %s", errPayloadMismatch, device, want, got)
	}
	return nil
}

// writeFilePayload writes size random bytes to path with one write call and
// fsync, then verifies the file's exact length and SHA-256. The application
// buffer size is not evidence of NVMe command size.
func writeFilePayload(ctx context.Context, run podExec, path string, size int64) (payloadDigest, error) {
	const source = "/tmp/file-payload.bin"
	oneWrite := []string{"bs=" + strconv.FormatInt(size, 10), "count=1", "iflag=fullblock"}
	want, err := randomSource(ctx, run, source, oneWrite, size)
	if err != nil {
		return payloadDigest{}, err
	}
	write := append([]string{"dd", "if=" + source, "of=" + path, "conv=fsync"}, oneWrite...)
	if _, err = run(ctx, write); err != nil {
		return payloadDigest{}, fmt.Errorf("write payload to %s: %w", path, err)
	}
	return want, verifyFilePayload(ctx, run, path, want)
}

func verifyFilePayload(ctx context.Context, run podExec, path string, want payloadDigest) error {
	got, err := fileDigest(ctx, run, path)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("%w: %s: wrote %s, read %s", errPayloadMismatch, path, want, got)
	}
	return nil
}

// randomSource fills a pod-local file from /dev/urandom using the given dd
// geometry and returns its digest. A nonzero payload is required so that a
// dropped write to zeroed or unwritten storage cannot match.
func randomSource(ctx context.Context, run podExec, path string, geometry []string, size int64) (payloadDigest, error) {
	generate := append([]string{"dd", "if=/dev/urandom", "of=" + path}, geometry...)
	if _, err := run(ctx, generate); err != nil {
		return payloadDigest{}, fmt.Errorf("generate payload %s: %w", path, err)
	}
	digest, err := fileDigest(ctx, run, path)
	if err != nil {
		return payloadDigest{}, err
	}
	if digest.bytes != size {
		return payloadDigest{}, fmt.Errorf("%w: generated %d of %d bytes in %s", errUnexpectedOutput, digest.bytes, size, path)
	}
	if digest.sha256 == zeroDigest(size) {
		return payloadDigest{}, fmt.Errorf("%w: %s", errPayloadZero, path)
	}
	return digest, nil
}

// fileDigest returns the exact size and SHA-256 of a pod-local file.
func fileDigest(ctx context.Context, run podExec, path string) (payloadDigest, error) {
	out, err := run(ctx, []string{"wc", "-c", path})
	if err != nil {
		return payloadDigest{}, fmt.Errorf("size %s: %w", path, err)
	}
	fields := strings.Fields(out)
	if len(fields) != 2 || fields[1] != path {
		return payloadDigest{}, fmt.Errorf("%w: wc -c %s: %q", errUnexpectedOutput, path, out)
	}
	size, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil || size < 0 {
		return payloadDigest{}, fmt.Errorf("%w: wc -c %s: %q", errUnexpectedOutput, path, out)
	}

	out, err = run(ctx, []string{"sha256sum", path})
	if err != nil {
		return payloadDigest{}, fmt.Errorf("hash %s: %w", path, err)
	}
	fields = strings.Fields(out)
	if len(fields) != 2 || fields[1] != path || !isSHA256Hex(fields[0]) {
		return payloadDigest{}, fmt.Errorf("%w: sha256sum %s: %q", errUnexpectedOutput, path, out)
	}
	return payloadDigest{sha256: fields[0], bytes: size}, nil
}

func logicalBlockSize(ctx context.Context, run podExec, device string) (int64, error) {
	size, err := blockdevInt(ctx, run, "--getss", device)
	if err != nil {
		return 0, err
	}
	if size&(size-1) != 0 {
		return 0, fmt.Errorf("%w: logical block size %d of %s is not a power of two", errUnexpectedOutput, size, device)
	}
	return size, nil
}

// blockdevInt returns the positive integer printed by blockdev for one query.
func blockdevInt(ctx context.Context, run podExec, query, device string) (int64, error) {
	out, err := run(ctx, []string{"blockdev", query, device})
	if err != nil {
		return 0, fmt.Errorf("blockdev %s %s: %w", query, device, err)
	}
	value, err := strconv.ParseInt(out, 10, 64)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%w: blockdev %s %s: %q", errUnexpectedOutput, query, device, out)
	}
	return value, nil
}

// mountedFSType returns the filesystem type of mountPoint from the pod's
// /proc/mounts. The last entry wins because later mounts shadow earlier ones.
func mountedFSType(ctx context.Context, run podExec, mountPoint string) (string, error) {
	out, err := run(ctx, []string{"cat", "/proc/mounts"})
	if err != nil {
		return "", fmt.Errorf("read /proc/mounts: %w", err)
	}
	fsType := ""
	for line := range strings.Lines(out) {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[1] == mountPoint {
			fsType = fields[2]
		}
	}
	if fsType == "" {
		return "", fmt.Errorf("%w: %s", errMountNotFound, mountPoint)
	}
	return fsType, nil
}

func ddBlocks(blockSize, size int64) []string {
	return []string{
		"bs=" + strconv.FormatInt(blockSize, 10),
		"count=" + strconv.FormatInt(size/blockSize, 10),
		"iflag=fullblock",
	}
}

func zeroDigest(size int64) string {
	hash := sha256.New()
	zeros := make([]byte, 64<<10)
	for remaining := size; remaining > 0; {
		n := min(remaining, int64(len(zeros)))
		hash.Write(zeros[:n])
		remaining -= n
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func isSHA256Hex(s string) bool {
	if len(s) != sha256.Size*2 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
