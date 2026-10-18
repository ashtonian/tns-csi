//go:build linux

package mount

import (
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
	mountutils "k8s.io/mount-utils"
)

type rawMountSource struct {
	root  string
	major uint32
	minor uint32
}

// Raw-device bind mounts identify the containing devtmpfs and its root, not Rdev.
func rawSourceForMountInfo(canonical string, device uint64, mountInfoPath string) (rawMountSource, error) {
	entries, err := mountutils.ParseMountInfo(mountInfoPath)
	if err != nil {
		return rawMountSource{}, fmt.Errorf("failed to read source mount information: %w", err)
	}
	result := rawMountSource{major: unix.Major(device), minor: unix.Minor(device)}
	longest := -1
	matches := make([]mountutils.MountInfo, 0, len(entries))
	for index := range entries {
		entry := &entries[index]
		point := filepath.Clean(unescapeMountField(entry.MountPoint))
		if int64(entry.Major) != int64(result.major) || int64(entry.Minor) != int64(result.minor) {
			continue
		}
		relative, relativeErr := filepath.Rel(point, canonical)
		if relativeErr != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			continue
		}
		if len(point) < longest {
			continue
		}
		if len(point) > longest {
			longest = len(point)
			matches = matches[:0]
		}
		matches = append(matches, *entry)
	}
	if longest < 0 {
		return rawMountSource{}, fmt.Errorf("%w: source filesystem is not identifiable", errInvalidMountInfo)
	}
	visible := topmostMount(matches)
	relative, err := filepath.Rel(filepath.Clean(unescapeMountField(visible.MountPoint)), canonical)
	if err != nil {
		return rawMountSource{}, err
	}
	result.root = filepath.Join(unescapeMountField(visible.Root), relative)
	return result, nil
}
