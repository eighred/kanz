package shadowprocess

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEnvironmentHelper(t *testing.T) {
	if len(os.Args) < 2 || os.Args[len(os.Args)-1] != "shadow-env-helper" {
		return
	}
	cwd, e := os.Getwd()
	if e != nil || os.Getenv("SHADOW_TEST_SECRET") != "" || os.Getenv("PATH") != "" || cwd != os.Getenv("HOME") || cwd != os.Getenv("TMPDIR") {
		os.Exit(2)
	}
	_, _ = os.Stdout.WriteString(cwd)
	os.Exit(0)
}
func TestTemporaryDirectoryAndScrubbedEnvironment(t *testing.T) {
	t.Setenv("SHADOW_TEST_SECRET", "fixture")
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	out, status := Run(context.Background(), Config{Executable: exe, Args: []string{"-test.run=^TestEnvironmentHelper$", "shadow-env-helper"}, Timeout: 3 * time.Second}, nil)
	if status != "" {
		t.Fatal(status)
	}
	if !filepath.IsAbs(string(out)) {
		t.Fatal("missing scratch cwd")
	}
	if _, e := os.Stat(string(out)); !os.IsNotExist(e) {
		t.Fatal("scratch not removed", e)
	}
}
