//go:build linux

package provision

import (
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// ReadManifest reads the single mounted input without following a leaf link.
// ConfigMap subPath mounts expose the pinned file, not its projected symlink.
func ReadManifest(path string) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, ErrInput
	}
	f := os.NewFile(uintptr(fd), "topic-manifest")
	defer f.Close()
	var before, after unix.Stat_t
	if unix.Fstat(fd, &before) != nil || before.Mode&unix.S_IFMT != unix.S_IFREG || before.Nlink != 1 || before.Size <= 0 || before.Size > MaxManifestBytes {
		return nil, ErrInput
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxManifestBytes+1))
	if err != nil || int64(len(b)) != before.Size || unix.Fstat(fd, &after) != nil || before.Dev != after.Dev || before.Ino != after.Ino || before.Mode != after.Mode || before.Nlink != after.Nlink || before.Size != after.Size || before.Mtim != after.Mtim || before.Ctim != after.Ctim {
		return nil, ErrInput
	}
	return b, nil
}
