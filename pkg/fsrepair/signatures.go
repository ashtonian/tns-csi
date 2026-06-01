package fsrepair

import (
	"regexp"
	"strings"
)

// FSFamily identifies the broad filesystem family a kernel message came from.
type FSFamily string

// Recognized filesystem families.
const (
	FamilyXFS FSFamily = "xfs"
	FamilyExt FSFamily = "ext"
)

// ShutdownSignal describes a filesystem-shutdown event extracted from a kernel
// log line (/dev/kmsg, dmesg) or a mount(8) failure message.
type ShutdownSignal struct {
	Device  string
	Family  FSFamily
	Raw     string
	Corrupt bool
}

var (
	// XFS messages are prefixed "XFS (<dev>): ...".
	xfsDeviceRe = regexp.MustCompile(`XFS \(([^)]+)\):`)
	// ext4 errors are prefixed "EXT4-fs error (device <dev>): ..." or
	// "EXT4-fs (<dev>): ...".
	ext4DeviceRe = regexp.MustCompile(`EXT4-fs(?: error)? \((?:device )?([^)]+)\)`)
	// JBD2 (the ext journaling layer) names the device as "<dev>-<inode>".
	jbd2DeviceRe = regexp.MustCompile(`(?:Aborting journal on device|journal on device) ([^ .,]+)`)

	// Phrases that indicate the filesystem has shut down / gone read-only.
	xfsShutdownMarkers = []string{
		"shut down",
		"shutting down",
		"force_shutdown",
		"forced shutdown",
		"corruption of in-memory data",
		"metadata corruption detected",
		"internal error",
	}
	extShutdownMarkers = []string{
		"remounting filesystem read-only",
		"ext4-fs error",
		"detected aborted journal",
		"aborting journal",
	}
	corruptMarkers = []string{"0x8", "corrupt"}
)

func containsAny(haystack string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}

func isCorrupt(lower string) bool { return containsAny(lower, corruptMarkers) }

// stripJBD2Suffix turns "nvme0n1-8" (JBD2's "<dev>-<journal-inode>" form) back
// into the bare device "nvme0n1".
func stripJBD2Suffix(dev string) string {
	i := strings.LastIndex(dev, "-")
	if i > 0 {
		return dev[:i]
	}
	return dev
}

// kmsgMatcher pairs a device-extracting regexp with the shutdown-marker phrases
// that qualify a matching line as a real shutdown for that filesystem family.
type kmsgMatcher struct {
	re       *regexp.Regexp
	family   FSFamily
	markers  []string
	stripDev func(string) string // optional device-name normalizer
}

// kmsgMatchers are tried in order; the first regexp that matches decides the
// family. XFS is checked before the ext/JBD2 forms.
var kmsgMatchers = []kmsgMatcher{
	{re: xfsDeviceRe, family: FamilyXFS, markers: xfsShutdownMarkers},
	{re: ext4DeviceRe, family: FamilyExt, markers: extShutdownMarkers},
	{re: jbd2DeviceRe, family: FamilyExt, markers: extShutdownMarkers, stripDev: stripJBD2Suffix},
}

// ParseKmsgLine extracts a ShutdownSignal from a single kernel log line. The
// second return is false if the line is not a recognized filesystem-shutdown
// message. Only XFS and ext (ext2/3/4 + JBD2) families are recognized.
func ParseKmsgLine(line string) (ShutdownSignal, bool) {
	trimmed := strings.TrimSpace(line)
	lower := strings.ToLower(trimmed)

	for _, matcher := range kmsgMatchers {
		groups := matcher.re.FindStringSubmatch(trimmed)
		if groups == nil {
			continue
		}
		if !containsAny(lower, matcher.markers) {
			return ShutdownSignal{}, false
		}
		device := groups[1]
		if matcher.stripDev != nil {
			device = matcher.stripDev(device)
		}
		return ShutdownSignal{
			Device:  device,
			Family:  matcher.family,
			Corrupt: isCorrupt(lower),
			Raw:     trimmed,
		}, true
	}

	return ShutdownSignal{}, false
}

// mountShutdownMarkers are mount(8) failure phrases that indicate a structural /
// shutdown problem the repair path can plausibly address. A plain "no such
// device" or "wrong fs type" is deliberately excluded — those are not repairable
// and must surface as the original error.
var mountShutdownMarkers = []string{
	"structure needs cleaning", // classic XFS "needs xfs_repair" mount error
	"can't read superblock",    // ext superblock unreadable
	"cannot read superblock",   // alternate phrasing across util-linux versions
	"input/output error",       // EIO from a shut-down device
	"mount(2) system call failed: structure needs cleaning",
}

// MountErrorIndicatesShutdown reports whether a failed mount's output looks like
// a filesystem-shutdown/structure problem (vs a missing device or wrong fstype).
// It is one of the gates the repair-on-stage path requires before doing anything.
func MountErrorIndicatesShutdown(mountOutput string) bool {
	lower := strings.ToLower(mountOutput)
	// Explicitly exclude non-repairable causes even if other markers match.
	if strings.Contains(lower, "no such device") ||
		strings.Contains(lower, "no such file") ||
		strings.Contains(lower, "wrong fs type") {

		return false
	}
	return containsAny(lower, mountShutdownMarkers)
}
