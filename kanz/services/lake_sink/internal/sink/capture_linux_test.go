//go:build linux

package sink

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestActiveRunCutRetainsPrefixAcrossAppendAndRestart(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()
	legacy, err := NewFileSink(root, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	row := Row{Domain: "market", Entity: "trade", EventID: "old", EventTime: time.Unix(1, 0).UTC()}
	if err := legacy.Write(ctx, row); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := NewFileSink(root, "", EnableActiveRunCapture)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	row.EventID = "first"
	if err := s.Write(ctx, row); err != nil {
		t.Fatal(err)
	}
	cut, original, err := s.CaptureCut(ctx)
	if err != nil || len(cut.Files) != 1 || len(original) != 1 {
		t.Fatalf("capture: %v", err)
	}
	if bytes.Contains(original[0], []byte(`"old"`)) || !bytes.Contains(original[0], []byte(`"first"`)) {
		t.Fatal("wrong run captured")
	}
	row.EventID = "suffix"
	if err := s.Write(ctx, row); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Write(ctx, row); err == nil {
		t.Fatal("capture owner reopened after close")
	}
	got, err := ReadLandingCut(ctx, root, cut)
	if err != nil || len(got) != 1 || !bytes.Equal(got[0], original[0]) {
		t.Fatalf("persisted prefix changed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "market", "trade", "dt=1970-01-01", "part-legacy.ndjson")); err != nil {
		t.Fatal("legacy file changed")
	}
}

func TestActiveRunCutRefusesReplacementTruncationAndMutation(t *testing.T) {
	for _, mode := range []string{"replacement", "truncation", "mutation", "symlink", "hardlink"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			s, err := NewFileSink(root, "", EnableActiveRunCapture)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Write(context.Background(), Row{Domain: "market", Entity: "trade", EventID: "first", EventTime: time.Unix(1, 0).UTC()}); err != nil {
				t.Fatal(err)
			}
			cut, contents, err := s.CaptureCut(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, cut.Files[0].Path)
			switch mode {
			case "replacement":
				if err := os.Rename(path, path+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, contents[0], 0o600); err != nil {
					t.Fatal(err)
				}
			case "truncation":
				if err := os.Truncate(path, 1); err != nil {
					t.Fatal(err)
				}
			case "mutation":
				b := bytes.Replace(contents[0], []byte("first"), []byte("other"), 1)
				if err := os.WriteFile(path, b, 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Rename(path, path+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".old", path); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(path, path+".alias"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := ReadLandingCut(context.Background(), root, cut); err == nil {
				t.Fatal("altered custody accepted")
			}
		})
	}
}

func TestActiveRunCaptureRefusesLegacyIdentityAndSymlinkRoot(t *testing.T) {
	root := t.TempDir()
	if _, err := NewFileSink(root, "legacy", EnableActiveRunCapture); err == nil {
		t.Fatal("explicit identity accepted")
	}
	link := filepath.Join(t.TempDir(), "root")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileSink(link, "", EnableActiveRunCapture); err == nil {
		t.Fatal("symlink root accepted")
	}
}
