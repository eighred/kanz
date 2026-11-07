//go:build linux

package sink

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

type captureRoot struct {
	fd            int
	path          string
	device, inode uint64
}

func openCaptureRoot(path string) (*captureRoot, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return nil, ErrCapture
	}
	base, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, ErrCapture
	}
	defer unix.Close(base)
	fd, err := unix.Openat2(base, strings.TrimPrefix(path, "/"), &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_NONBLOCK,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS,
	})
	if err != nil {
		return nil, ErrCapture
	}
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil {
		_ = unix.Close(fd)
		return nil, ErrCapture
	}
	return &captureRoot{fd: fd, path: path, device: uint64(st.Dev), inode: st.Ino}, nil
}

func (r *captureRoot) close() error { return unix.Close(r.fd) }

func (r *captureRoot) create(path string) (*os.File, error) {
	// The date partition contains '='; validate it independently of the source
	// domain/entity sanitization used by the existing landing layout.
	if !validCapturePath(path) {
		return nil, ErrCapture
	}
	parts := strings.Split(path, "/")
	fd, err := unix.Dup(r.fd)
	if err != nil {
		return nil, ErrCapture
	}
	defer func() { _ = unix.Close(fd) }()
	for _, part := range parts[:3] {
		err := unix.Mkdirat(fd, part, 0o755)
		if err != nil && !errors.Is(err, unix.EEXIST) {
			return nil, ErrCapture
		}
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
		if err != nil {
			return nil, ErrCapture
		}
		if err := unix.Fsync(fd); err != nil {
			_ = unix.Close(next)
			return nil, ErrCapture
		}
		_ = unix.Close(fd)
		fd = next
	}
	fileFD, err := unix.Openat(fd, parts[3], unix.O_RDWR|unix.O_APPEND|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, ErrCapture
	}
	if err := unix.Fsync(fd); err != nil {
		_ = unix.Close(fileFD)
		return nil, ErrCapture
	}
	return os.NewFile(uintptr(fileFD), filepath.Join(r.path, path)), nil
}

func validCapturePath(path string) bool {
	parts := strings.Split(path, "/")
	if len(parts) != 4 || !strings.HasPrefix(parts[2], "dt=") {
		return false
	}
	copyParts := append([]string(nil), parts...)
	copyParts[2] = strings.TrimPrefix(parts[2], "dt=")
	// Reuse segment checks without permitting separators or dot segments.
	for _, part := range copyParts {
		if part == "" || len(part) > 255 || part == "." || part == ".." || sanitize(part) != part {
			return false
		}
	}
	return len(path) <= 4096 && strings.HasPrefix(parts[3], "part-") && strings.HasSuffix(parts[3], ".ndjson")
}

func (r *captureRoot) open(path string) (*os.File, error) {
	if !validCapturePath(path) {
		return nil, ErrCapture
	}
	fd, err := unix.Openat2(r.fd, path, &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NONBLOCK, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS})
	if err != nil {
		return nil, ErrCapture
	}
	return os.NewFile(uintptr(fd), filepath.Join(r.path, path)), nil
}

func captureIdentity(f *os.File) (uint64, uint64, int64, error) {
	var st unix.Stat_t
	if unix.Fstat(int(f.Fd()), &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 || st.Size < 0 {
		return 0, 0, 0, ErrCapture
	}
	return uint64(st.Dev), st.Ino, st.Size, nil
}
