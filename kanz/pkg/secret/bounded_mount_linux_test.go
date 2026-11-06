//go:build linux

package secret

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestReadBoundedRejectsFIFOAndPreservesCSISymlinks(t *testing.T) {
	const key = "KANZ_BOUNDED_FIFO_TEST"
	t.Setenv(key, "must-not-fallback")
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	if err := unix.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "fifo-link")
	if err := os.Symlink(fifo, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{fifo, link} {
		t.Setenv(key+"_FILE", path)
		result := make(chan bool, 1)
		go func() { value, err := ReadBounded(key, 32); result <- err != nil && value == "" }()
		select {
		case refused := <-result:
			if !refused {
				t.Fatal("FIFO accepted")
			}
		case <-time.After(time.Second):
			t.Fatal("FIFO blocked opener")
		}
	}
	regular := filepath.Join(dir, "value")
	if err := os.WriteFile(regular, []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	csi := filepath.Join(dir, "csi-link")
	if err := os.Symlink(regular, csi); err != nil {
		t.Fatal(err)
	}
	t.Setenv(key+"_FILE", csi)
	if value, err := ReadBounded(key, 32); err != nil || value != "synthetic" {
		t.Fatal("CSI regular-file symlink refused")
	}
}
