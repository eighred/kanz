//go:build linux && amd64

package snapshot

import (
	"bytes"
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func openWorkerExecutable(path string, expected [32]byte) (*os.File, error) {
	if os.Geteuid() == 0 {
		return nil, ErrWorker
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return nil, ErrInvalid
	}
	root, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, ErrWorker
	}
	defer unix.Close(root)
	fd, err := unix.Openat2(root, strings.TrimPrefix(path, "/"), &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS})
	if err != nil {
		return nil, ErrWorker
	}
	f := os.NewFile(uintptr(fd), path)
	var before, after unix.Stat_t
	if unix.Fstat(fd, &before) != nil || before.Mode&unix.S_IFMT != unix.S_IFREG || before.Uid != 0 || before.Nlink != 1 || before.Size <= 0 || before.Size > 512<<20 || before.Mode&0o222 != 0 || before.Mode&0o111 == 0 {
		_ = f.Close()
		return nil, ErrWorker
	}
	var magic [4]byte
	if _, err := f.ReadAt(magic[:], 0); err != nil || !bytes.Equal(magic[:], []byte{0x7f, 'E', 'L', 'F'}) {
		_ = f.Close()
		return nil, ErrWorker
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, before.Size+1))
	if err != nil || n != before.Size || unix.Fstat(fd, &after) != nil || before.Dev != after.Dev || before.Ino != after.Ino || before.Mode != after.Mode || before.Nlink != after.Nlink || before.Size != after.Size || before.Mtim != after.Mtim || before.Ctim != after.Ctim || !bytes.Equal(h.Sum(nil), expected[:]) {
		_ = f.Close()
		return nil, ErrWorker
	}
	if _, err := f.Seek(0, 0); err != nil {
		_ = f.Close()
		return nil, ErrWorker
	}
	return f, nil
}
