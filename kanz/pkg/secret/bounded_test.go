package secret

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestReadBounded(t *testing.T) {
	const key = "KANZ_BOUNDED_TEST"
	t.Setenv(key, "fallback")
	t.Setenv(key+"_FILE", "")
	for _, limit := range []int64{0, -1, 1 << 62} {
		if _, err := ReadBounded(key, limit); err == nil {
			t.Fatal("invalid bound accepted")
		}
	}
	path := filepath.Join(t.TempDir(), "mounted")
	t.Setenv(key+"_FILE", path)
	if value, err := ReadBounded(key, 32); err == nil || value != "" || strings.Contains(err.Error(), path) {
		t.Fatal("missing mount fell back or disclosed path")
	}
	if err := os.WriteFile(path, []byte("  mounted  "), 0600); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "linux" {
		if _, err := ReadBounded(key, 32); err == nil {
			t.Fatal("unsupported bounded mount accepted")
		}
		return
	}
	if value, err := ReadBounded(key, 32); err != nil || value != "mounted" {
		t.Fatal("file preference failed")
	}
	if value, err := ReadBounded(key, 3); err == nil || value != "" {
		t.Fatal("oversized mount accepted")
	}
	t.Setenv(key+"_FILE", filepath.Dir(path))
	if _, err := ReadBounded(key, 32); err == nil {
		t.Fatal("directory accepted")
	}
}
