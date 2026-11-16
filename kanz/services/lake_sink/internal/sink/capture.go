package sink

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
)

type FileOption uint8

// EnableActiveRunCapture requires an existing Linux landing root and a generated
// run identity. Its files are exclusively created and Close is terminal. The
// default sink and explicit legacy append identities keep their old semantics.
const EnableActiveRunCapture FileOption = 1

const MaxCutFiles = 64
const MaxCutBytes int64 = 64 << 20

var ErrCapture = errors.New("landing capture unavailable")

// LandingCut is private persisted custody metadata, never a public API response.
// Paths identify only the exact current-run handles present at capture. They do
// not enumerate earlier runs or assert complete archival coverage.
type LandingCut struct {
	RootDevice uint64    `json:"root_device"`
	RootInode  uint64    `json:"root_inode"`
	Files      []CutFile `json:"files"`
}

type CutFile struct {
	Path   string   `json:"path"`
	Device uint64   `json:"device"`
	Inode  uint64   `json:"inode"`
	Bytes  int64    `json:"bytes"`
	SHA256 [32]byte `json:"sha256"`
}

// CaptureCut flushes and copies complete finite prefixes while the owning writer
// mutex is held. The caller persists this cut once before conversion; a retry
// calls ReadLandingCut and never captures a replacement selection.
func (s *FileSink) CaptureCut(ctx context.Context) (LandingCut, [][]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.capture == nil || s.closed || len(s.writers) > MaxCutFiles || ctx.Err() != nil {
		return LandingCut{}, nil, ErrCapture
	}
	cut := LandingCut{RootDevice: s.capture.device, RootInode: s.capture.inode}
	dirs := make([]string, 0, len(s.writers))
	for dir := range s.writers {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	var total int64
	var contents [][]byte
	for _, dir := range dirs {
		pw := s.writers[dir]
		if ctx.Err() != nil {
			return LandingCut{}, nil, ErrCapture
		}
		if err := pw.flush(); err != nil {
			return LandingCut{}, nil, err
		}
		device, inode, size, err := captureIdentity(pw.f)
		if err != nil || size > MaxCutBytes-total {
			return LandingCut{}, nil, ErrCapture
		}
		rel, err := filepath.Rel(s.root, pw.f.Name())
		if err != nil {
			return LandingCut{}, nil, ErrCapture
		}
		part, err := readPrefix(ctx, pw.f, size)
		if err != nil {
			return LandingCut{}, nil, err
		}
		cut.Files = append(cut.Files, CutFile{Path: filepath.ToSlash(rel), Device: device, Inode: inode, Bytes: size, SHA256: sha256.Sum256(part)})
		contents = append(contents, part)
		total += size
	}
	return cut, contents, nil
}

// ReadLandingCut reopens only persisted relative paths. Replacement, truncation,
// prefix mutation, symlinks, and a different root refuse. Appended suffixes are
// deliberately excluded. No current writer scan or new cut occurs on retry.
func ReadLandingCut(ctx context.Context, root string, cut LandingCut) ([][]byte, error) {
	if len(cut.Files) > MaxCutFiles || ctx.Err() != nil {
		return nil, ErrCapture
	}
	r, err := openCaptureRoot(root)
	if err != nil {
		return nil, err
	}
	defer r.close()
	if r.device != cut.RootDevice || r.inode != cut.RootInode {
		return nil, ErrCapture
	}
	var total int64
	var result [][]byte
	previous := ""
	for _, entry := range cut.Files {
		if entry.Path <= previous || entry.Bytes < 0 || entry.Bytes > MaxCutBytes-total {
			return nil, ErrCapture
		}
		previous = entry.Path
		f, err := r.open(entry.Path)
		if err != nil {
			return nil, err
		}
		device, inode, size, statErr := captureIdentity(f)
		if statErr != nil || device != entry.Device || inode != entry.Inode || size < entry.Bytes {
			_ = f.Close()
			return nil, ErrCapture
		}
		part, readErr := readPrefix(ctx, f, entry.Bytes)
		closeErr := f.Close()
		if readErr != nil || closeErr != nil || sha256.Sum256(part) != entry.SHA256 {
			return nil, ErrCapture
		}
		result = append(result, part)
		total += entry.Bytes
	}
	return result, nil
}

func readPrefix(ctx context.Context, f *os.File, size int64) ([]byte, error) {
	if size < 0 || size > MaxCutBytes || ctx.Err() != nil {
		return nil, ErrCapture
	}
	b := make([]byte, int(size))
	reader := io.NewSectionReader(f, 0, size)
	for start := 0; start < len(b); {
		if ctx.Err() != nil {
			return nil, ErrCapture
		}
		end := min(start+64*1024, len(b))
		if _, err := io.ReadFull(reader, b[start:end]); err != nil {
			return nil, ErrCapture
		}
		start = end
	}
	if len(b) != 0 && b[len(b)-1] != '\n' {
		return nil, ErrCapture
	}
	return b, nil
}
