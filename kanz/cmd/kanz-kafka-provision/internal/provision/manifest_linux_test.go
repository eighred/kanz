//go:build linux

package provision

import (
	"os"
	"path/filepath"
	"testing"
)

func TestManifestFileCustody(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "manifest")
	if err := os.WriteFile(p, []byte(testManifest), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadManifest(p); err != nil || string(got) != testManifest {
		t.Fatal("regular manifest refused")
	}
	link := filepath.Join(d, "link")
	if err := os.Symlink(p, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadManifest(link); err == nil {
		t.Fatal("symlink accepted")
	}
	if _, err := ReadManifest(d); err == nil {
		t.Fatal("directory accepted")
	}
	hard := filepath.Join(d, "hard")
	if err := os.Link(p, hard); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadManifest(p); err == nil {
		t.Fatal("hardlink accepted")
	}
	if err := os.Remove(hard); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, make([]byte, MaxManifestBytes+1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadManifest(p); err == nil {
		t.Fatal("oversize accepted")
	}
}
