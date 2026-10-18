//go:build linux

package volumeprobe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/fenio/tns-csi/pkg/cmdexec"
	"github.com/fenio/tns-csi/pkg/mount"
	"golang.org/x/sys/unix"
	mountutils "k8s.io/mount-utils"
)

// sysDevBlockPath stays on local sysfs; tests substitute synthetic metadata.
var sysDevBlockPath = "/sys/dev/block"

func observe(ctx context.Context, request Request, leases []*os.File) Result {
	if request.ResolveMount {
		return resolveMount(ctx, request.Path)
	}
	result := observeFilesystem(ctx, request, leases)
	var err error
	result.StagingNQN, err = readMetadata(request.MetadataPath)
	if err != nil {
		return Result{Code: Unknown}
	}
	result.PolicyRecord, err = readMetadata(request.MarkerPath)
	if err != nil {
		return Result{Code: Unknown}
	}
	return result
}

func resolveMount(ctx context.Context, path string) Result {
	return resolveMountWithStat(ctx, path, unix.Stat)
}

func resolveMountWithStat(ctx context.Context, path string, statPath func(string, *unix.Stat_t) error) Result {
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return Result{Code: classifyError(err)}
	}
	entry, mounted, err := mount.MountedEntryLexical(ctx, canonical)
	if err != nil {
		return Result{Code: Unknown}
	}
	var stat unix.Stat_t
	if statErr := statPath(canonical, &stat); statErr != nil {
		return Result{Code: classifyError(statErr)}
	}
	mountPath := canonical
	raw := stat.Mode&unix.S_IFMT == unix.S_IFBLK
	rawDevice := ""
	if raw {
		rawDevice = fmt.Sprintf("%d:%d", unix.Major(stat.Rdev), unix.Minor(stat.Rdev))
	}
	if !mounted {
		if !raw {
			return Result{Code: Missing}
		}
		mountPath, err = containingMount(canonical)
		if err != nil {
			return Result{Code: Unknown}
		}
		entry, mounted, err = mount.MountedEntryLexical(ctx, mountPath)
	}
	if err != nil || !mounted {
		return Result{Code: Unknown}
	}
	return Result{Code: OK, RawDevice: rawDevice, Mount: &ResolvedMount{Path: mountPath, Source: entry.Source, Filesystem: entry.FSType, Device: entry.Device, MountID: entry.MountID, Raw: raw}}
}

func containingMount(path string) (string, error) {
	entries, err := mountutils.ParseMountInfo("/proc/self/mountinfo")
	if err != nil {
		return "", err
	}
	selected := ""
	for index := range entries {
		point := entries[index].MountPoint
		if (point == "/" || strings.HasPrefix(path, point+"/")) && len(point) > len(selected) {
			selected = point
		}
	}
	if selected == "" {
		return "", ErrProtocol
	}
	return selected, nil
}

func readMetadata(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	file, openErr := root.Open(filepath.Base(path))
	closeErr := root.Close()
	if errors.Is(openErr, os.ErrNotExist) && closeErr == nil {
		return "", nil
	}
	if openErr != nil {
		return "", errors.Join(openErr, closeErr)
	}
	info, statErr := file.Stat()
	if statErr != nil || !info.Mode().IsRegular() {
		return "", errors.Join(ErrProtocol, statErr, closeErr, file.Close())
	}
	data, readErr := io.ReadAll(io.LimitReader(file, 4097))
	fileErr := file.Close()
	if err := errors.Join(readErr, closeErr, fileErr); err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(data))
	if len(data) > 4096 || value == "" || strings.ContainsAny(value, "\r\n\x00") {
		return "", ErrProtocol
	}
	return value, nil
}

func observeFilesystem(ctx context.Context, request Request, leases []*os.File) Result {
	root, err := os.OpenRoot("/")
	if err != nil {
		return Result{Code: classifyError(err)}
	}
	name := strings.TrimPrefix(filepath.Clean(request.Path), "/")
	if name == "" {
		name = "."
	}
	file, openErr := root.Open(name)
	closeRootErr := root.Close()
	if openErr != nil || closeRootErr != nil {
		if file != nil {
			closeLease(file)
		}
		return Result{Code: classifyError(errors.Join(openErr, closeRootErr))}
	}
	defer closeLease(file)
	var stat unix.Stat_t
	if statErr := unix.Fstat(int(file.Fd()), &stat); statErr != nil {
		return Result{Code: classifyError(statErr)}
	}
	mountID, err := descriptorMountID(file)
	device := fmt.Sprintf("%d:%d", unix.Major(stat.Dev), unix.Minor(stat.Dev))
	if err != nil || mountID != request.MountID || device != request.Device {
		return Result{Code: IdentityChanged}
	}
	if stat.Mode&unix.S_IFMT == unix.S_IFBLK {
		return rawCapacity(stat.Rdev)
	}
	if request.CheckSCSI {
		if code := scsiTransportState(request.Device); code != OK {
			return Result{Code: code}
		}
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return Result{Code: Unsupported}
	}
	if _, err := file.Readdirnames(1); err != nil && !errors.Is(err, io.EOF) {
		return Result{Code: classifyError(err)}
	}
	var statfs unix.Statfs_t
	if err := unix.Fstatfs(int(file.Fd()), &statfs); err != nil {
		return Result{Code: classifyError(err)}
	}
	result := Result{Code: OK}
	if request.Filesystem == "xfs" {
		if statfs.Type != unix.XFS_SUPER_MAGIC {
			return Result{Code: IdentityChanged}
		}
		result = probeXFS(ctx, file, leases)
		if result.Code != OK {
			return result
		}
	}
	if statfs.Bsize <= 0 || statfs.Bfree > statfs.Blocks || statfs.Bavail > statfs.Blocks || statfs.Ffree > statfs.Files {
		return Result{Code: Unknown}
	}
	blockSize := uint64(statfs.Bsize)
	if statfs.Blocks > math.MaxUint64/blockSize {
		return Result{Code: Unknown}
	}
	result.TotalBytes = statfs.Blocks * blockSize
	result.FreeBytes = statfs.Bfree * blockSize
	result.AvailableBytes = statfs.Bavail * blockSize
	result.TotalInodes = statfs.Files
	result.FreeInodes = statfs.Ffree
	return result
}

func rawCapacity(rdev uint64) Result {
	device := fmt.Sprintf("%d:%d", unix.Major(rdev), unix.Minor(rdev))
	root, err := os.OpenRoot(filepath.Join(sysDevBlockPath, device))
	if err != nil {
		return Result{Code: classifyError(err)}
	}
	file, openErr := root.Open("size")
	closeErr := root.Close()
	if openErr != nil {
		return Result{Code: classifyError(errors.Join(openErr, closeErr))}
	}
	data, readErr := io.ReadAll(io.LimitReader(file, 65))
	closeFileErr := file.Close()
	if errors.Join(readErr, closeErr, closeFileErr) != nil {
		return Result{Code: Unknown}
	}
	link, linkErr := os.Readlink(filepath.Join(sysDevBlockPath, device))
	if linkErr != nil {
		return Result{Code: Unknown}
	}
	if strings.HasPrefix(filepath.Base(link), "sd") {
		if code := scsiTransportState(device); code != OK {
			return Result{Code: code}
		}
	}
	return parseRawCapacity(data, device)
}

func parseRawCapacity(data []byte, device string) Result {
	sectors, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil || len(data) > 64 || sectors == 0 || sectors > math.MaxInt64/512 {
		return Result{Code: Unknown}
	}
	return Result{Code: OK, RawDevice: device, TotalBytes: sectors * 512}
}

func scsiTransportState(device string) string {
	root, err := os.OpenRoot(filepath.Join(sysDevBlockPath, device, "device"))
	if err != nil {
		return Unknown
	}
	file, openErr := root.Open("state")
	closeErr := root.Close()
	if openErr != nil {
		return Unknown
	}
	data, readErr := io.ReadAll(io.LimitReader(file, 65))
	fileErr := file.Close()
	if errors.Join(readErr, closeErr, fileErr) != nil || len(data) > 64 {
		return Unknown
	}
	return classifySCSIState(strings.TrimSpace(string(data)))
}

func classifySCSIState(state string) string {
	switch state {
	case "running":
		return OK
	case "":
		return Unknown
	default:
		return TransportUnavailable
	}
}

func descriptorMountID(file *os.File) (int, error) {
	root, err := os.OpenRoot("/proc/self/fdinfo")
	if err != nil {
		return 0, err
	}
	info, openErr := root.Open(strconv.FormatUint(uint64(file.Fd()), 10))
	closeRootErr := root.Close()
	if openErr != nil {
		return 0, errors.Join(openErr, closeRootErr)
	}
	data, readErr := io.ReadAll(io.LimitReader(info, 4097))
	closeErr := info.Close()
	if err := errors.Join(readErr, closeErr, closeRootErr); err != nil || len(data) > 4096 {
		return 0, ErrProtocol
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "mnt_id:" {
			return strconv.Atoi(fields[1])
		}
	}
	return 0, ErrProtocol
}

func probeXFS(ctx context.Context, file *os.File, leases []*os.File) Result {
	// xfs_io supplies the supported UAPI layout. Force v5 root-only bulkstat;
	// geometry and ordinary stat alone can succeed after a forced shutdown.
	cmd := cmdexec.CommandContext(ctx, "xfs_io", "-r", "-c", "bulkstat_single -v 5 root", "/proc/self/fd/5")
	cmd.Env = probeEnvironment()
	cmd.ExtraFiles = []*os.File{leases[0], leases[1], file}
	var output, diagnostic limitedOutput
	cmd.Stdout = &output
	cmd.Stderr = &diagnostic
	err := cmd.Run()
	data, overflow := output.contents()
	stderr, diagnosticOverflow := diagnostic.contents()
	if overflow || diagnosticOverflow {
		return Result{Code: Unknown}
	}
	// Some xfs_io versions print a failed bulkstat diagnostic but exit zero.
	// Require a parsed result as well as successful exit and empty stderr.
	if err != nil || len(bytes.TrimSpace(stderr)) > 0 {
		if errors.Is(err, os.ErrNotExist) || strings.Contains(string(stderr), "Inappropriate ioctl") || strings.Contains(string(stderr), "Operation not supported") {
			return Result{Code: Unsupported}
		}
		if strings.Contains(string(stderr), "Input/output error") {
			return Result{Code: IOFailure}
		}
		return Result{Code: Unknown}
	}
	return parseXFSRoot(data)
}

func parseXFSRoot(data []byte) Result {
	fields := make(map[string]uint64)
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), " = ")
		if !ok {
			continue
		}
		switch key {
		case "bs_ino", "bs_version", "bs_sick", "bs_checked", "bs_mode":
		default:
			continue
		}
		if _, duplicate := fields[key]; duplicate {
			return Result{Code: Unknown}
		}
		number, err := strconv.ParseUint(value, 0, 64)
		if err != nil {
			return Result{Code: Unknown}
		}
		fields[key] = number
	}
	if len(fields) != 5 || fields["bs_ino"] == 0 || fields["bs_version"] != 5 || fields["bs_mode"]&unix.S_IFMT != unix.S_IFDIR || fields["bs_sick"] > math.MaxUint16 || fields["bs_checked"] > math.MaxUint16 {
		return Result{Code: Unknown}
	}
	return Result{Code: OK, RootSick: fields["bs_sick"], RootChecked: fields["bs_checked"]}
}

func classifyError(err error) string {
	switch {
	case errors.Is(err, unix.EIO), errors.Is(err, unix.EUCLEAN):
		return IOFailure
	case errors.Is(err, os.ErrNotExist):
		return Missing
	case errors.Is(err, unix.ENOTTY), errors.Is(err, unix.EOPNOTSUPP):
		return Unsupported
	default:
		return Unknown
	}
}
