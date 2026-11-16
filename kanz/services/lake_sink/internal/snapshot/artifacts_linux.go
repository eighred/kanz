//go:build linux

package snapshot

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func artifactRoot(root string) (int, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || root == "/" {
		return -1, ErrInvalid
	}
	base, err := unix.Open("/", unix.O_DIRECTORY|unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, ErrCustody
	}
	defer unix.Close(base)
	fd, err := unix.Openat2(base, strings.TrimPrefix(root, "/"), &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_NONBLOCK, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS})
	if err != nil {
		return -1, ErrCustody
	}
	return fd, nil
}

func publishArtifactFiles(ctx context.Context, root, name string, manifest, parquet []byte) error {
	if !idPattern.MatchString(name) || len(manifest) == 0 || len(manifest) > MaxControlBytes || len(parquet) == 0 || len(parquet) > MaxParquetBytes {
		return ErrInvalid
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	fd, err := artifactRoot(root)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	var existing unix.Stat_t
	err = unix.Fstatat(fd, name, &existing, unix.AT_SYMLINK_NOFOLLOW)
	if err == nil {
		if existing.Mode&unix.S_IFMT != unix.S_IFDIR {
			return ErrCustody
		}
		storedManifest, err := readArtifactFile(ctx, root, name, "manifest.json", MaxControlBytes)
		if err != nil || !bytes.Equal(storedManifest, manifest) {
			return ErrCustody
		}
		storedParquet, err := readArtifactFile(ctx, root, name, "events.parquet", MaxParquetBytes)
		if err != nil || !bytes.Equal(storedParquet, parquet) {
			return ErrCustody
		}
		return nil
	}
	if !errors.Is(err, unix.ENOENT) {
		return ErrCustody
	}
	staging := name + "-pending"
	if err := unix.Mkdirat(fd, staging, 0o700); err != nil {
		return ErrCustody
	}
	dir, err := unix.Openat(fd, staging, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return ErrCustody
	}
	defer unix.Close(dir)
	for _, member := range []struct {
		name  string
		bytes []byte
	}{{"manifest.json", manifest}, {"events.parquet", parquet}} {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		fileFD, err := unix.Openat(dir, member.name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o400)
		if err != nil {
			return ErrCustody
		}
		f := os.NewFile(uintptr(fileFD), member.name)
		writeErr := writeArtifact(ctx, f, member.bytes)
		closeErr := f.Close()
		if writeErr != nil {
			return writeErr
		}
		if closeErr != nil {
			return ErrCustody
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if unix.Fsync(dir) != nil || unix.Renameat2(fd, staging, fd, name, unix.RENAME_NOREPLACE) != nil || unix.Fsync(fd) != nil {
		return ErrCustody
	}
	return nil
}

func writeArtifact(ctx context.Context, f *os.File, data []byte) error {
	for len(data) > 0 {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		n := min(len(data), 64<<10)
		written, err := f.Write(data[:n])
		if err != nil || written != n {
			return ErrCustody
		}
		data = data[n:]
	}
	if err := f.Sync(); err != nil {
		return ErrCustody
	}
	return nil
}

func readArtifactFile(ctx context.Context, root, name, member string, maximum int) ([]byte, error) {
	if !idPattern.MatchString(name) || (member != "manifest.json" && member != "events.parquet") || maximum < 1 {
		return nil, ErrInvalid
	}
	fd, err := artifactRoot(root)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)
	fileFD, err := unix.Openat2(fd, name+"/"+member, &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_NONBLOCK | unix.O_CLOEXEC, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS})
	if err != nil {
		return nil, ErrCustody
	}
	f := os.NewFile(uintptr(fileFD), member)
	defer f.Close()
	var before, after unix.Stat_t
	if unix.Fstat(fileFD, &before) != nil || before.Mode&unix.S_IFMT != unix.S_IFREG || before.Nlink != 1 || before.Size <= 0 || before.Size > int64(maximum) {
		return nil, ErrCustody
	}
	data := make([]byte, int(before.Size))
	for start := 0; start < len(data); {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		end := min(start+(64<<10), len(data))
		n, err := f.ReadAt(data[start:end], int64(start))
		if err != nil || n != end-start {
			return nil, ErrCustody
		}
		start = end
	}
	if unix.Fstat(fileFD, &after) != nil || before.Dev != after.Dev || before.Ino != after.Ino || before.Size != after.Size || before.Mtim != after.Mtim || before.Ctim != after.Ctim || after.Nlink != 1 {
		return nil, ErrCustody
	}
	return data, nil
}
