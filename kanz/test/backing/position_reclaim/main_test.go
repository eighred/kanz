//go:build linux

package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestReceiptFilesRefuseLinksPipesAndOversize(t *testing.T) {
	dir := t.TempDir()
	ordinary := filepath.Join(dir, "ordinary")
	if err := os.WriteFile(ordinary, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(ordinary, filepath.Join(dir, "symlink")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "oversize"), make([]byte, 16385), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"symlink", "pipe", "oversize"} {
		if _, err := readReceipt(filepath.Join(dir, name)); err == nil {
			t.Errorf("accepted %s", name)
		}
	}
	if err := os.Link(ordinary, filepath.Join(dir, "hardlink")); err != nil {
		t.Fatal(err)
	}
	if _, err := readReceipt(ordinary); err == nil {
		t.Error("accepted multiply linked receipt")
	}
	if err := os.Symlink(dir, filepath.Join(dir, "parentlink")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := parentFD(filepath.Join(dir, "parentlink", "new")); err == nil {
		t.Error("followed linked parent")
	}
}

func TestReceiptExclusiveRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "receipt.json")
	want := receipt{Schema: 1, Phase: "seed", Source: "test"}
	if err := writeReceipt(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := readReceipt(path)
	if err != nil || got != want {
		t.Fatalf("round trip: %#v %v", got, err)
	}
	if err := writeReceipt(path, want); err == nil {
		t.Fatal("overwrote retained receipt")
	}
}
