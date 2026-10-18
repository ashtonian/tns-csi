//go:build linux || darwin

package performance

import (
	"os"

	"golang.org/x/sys/unix"
)

func openEvidenceFile(directory *os.File, name string) (*os.File, error) {
	connection, err := directory.SyscallConn()
	if err != nil {
		return nil, ErrEvidenceCorrupt
	}
	var openErr error
	var descriptor int
	// Root.OpenFile resolves in-root symlinks on read. Native openat must refuse
	// the final component, and nonblocking open must not wait for a FIFO writer.
	controlErr := connection.Control(func(parent uintptr) {
		descriptor, openErr = unix.Openat(int(parent), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	})
	if controlErr != nil || openErr != nil {
		return nil, ErrEvidenceCorrupt
	}
	return os.NewFile(uintptr(descriptor), name), nil
}
