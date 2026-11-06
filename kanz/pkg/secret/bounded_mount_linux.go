//go:build linux

package secret

import (
	"os"

	"golang.org/x/sys/unix"
)

// CSI mounts use symlinks, so follow the configured path intentionally. Opening
// nonblocking and validating the opened descriptor (not a pre-open pathname)
// prevents FIFO/device replacement from trapping the verifier before its limit.
func openBoundedMount(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), "bounded-secret"), nil
}
