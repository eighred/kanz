package edgeconfig

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestInvalidProfilesDoNotReplaceExistingConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "envoy.json")
	if err := Write(path, Default()); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Options){
		func(o *Options) { o.ListenPort = 80 },
		func(o *Options) { o.AdminPort = o.BFFPort },
		func(o *Options) { o.BFFPort = 65536 },
		func(o *Options) { o.RequestsPerSecond = 0 },
		func(o *Options) { o.RequestsPerSecond = 10001 },
	} {
		o := Default()
		change(&o)
		if Write(path, o) == nil {
			t.Fatal("invalid profile replaced the configuration")
		}
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, original) {
			t.Fatal("refused profile changed existing bytes")
		}
	}
	o := Default()
	o.RequestsPerSecond = 100
	if err := Write(path, o); err != nil {
		t.Fatal("valid retry must replace a complete configuration", err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatal("configuration generation left temporary entries")
	}
}

func TestWriterReplacesFinalSymlinkWithoutFollowingItsTarget(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "untouched")
	if err := os.WriteFile(outside, []byte("synthetic sentinel"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "envoy.json")
	if err := os.Symlink(outside, path); err != nil {
		t.Skip("host does not permit a symlink fixture")
	}
	if err := Write(path, Default()); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(outside)
	if err != nil || string(got) != "synthetic sentinel" {
		t.Fatal("writer followed the previous configuration symlink")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatal("configuration is not a new regular file")
	}
}
