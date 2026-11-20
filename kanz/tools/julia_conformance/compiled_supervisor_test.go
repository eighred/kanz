//go:build linux && julia_application_qualification

package juliaconformance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/shadowprocess"
)

var launcherSHA = flag.String("application-launcher-sha256", "", "Expected launcher digest from trusted OCI readback")

// Run only inside the credential-free, resource-bounded application container.
func TestCompiledApplicationSupervisor(t *testing.T) {
	const launcher = "/opt/kanz/bin/curve-worker"
	if os.Getuid() != 65532 || os.Getgid() != 65532 {
		t.Fatal("incorrect supervisor identity")
	}
	launcherBytes, err := os.ReadFile(launcher)
	digest := sha256.Sum256(launcherBytes)
	if err != nil || hex.EncodeToString(digest[:]) != *launcherSHA {
		t.Fatal("launcher differs from saved OCI")
	}
	dir := t.TempDir()
	*ociDirectory, *ociPhase = dir, "prepare"
	if !t.Run("prepare", TestJuliaOCIOracle) {
		return
	}
	data, err := os.ReadFile(filepath.Join(dir, "cases.json"))
	var cases []struct {
		Name     string
		ExitZero bool
	}
	if err != nil || json.Unmarshal(data, &cases) != nil || len(cases) != 13 {
		t.Fatal("invalid oracle corpus")
	}
	cfg := shadowprocess.Config{Executable: launcher, Timeout: 20 * time.Second}
	for _, item := range cases {
		t.Run("supervise/"+item.Name, func(t *testing.T) {
			input, err := os.ReadFile(filepath.Join(dir, item.Name+".input"))
			if err != nil {
				t.Fatal(err)
			}
			output, status := shadowprocess.Run(context.Background(), cfg, input)
			want := ""
			switch item.Name {
			case "empty", "malformed":
				want = "worker_failure"
			case "oversize":
				want = "invalid_input"
			}
			if status != want || item.ExitZero != (status == "") || (want != "" && len(output) != 0) {
				t.Fatalf("unexpected supervised result %q", status)
			}
			if err := os.WriteFile(filepath.Join(dir, item.Name+".output"), output, 0600); err != nil {
				t.Fatal(err)
			}
		})
	}
	*ociPhase = "verify"
	t.Run("verify", TestJuliaOCIOracle)
	// The real worker is still starting when its deadline expires. Run waits
	// for process termination and removes its private temporary directory.
	cfg.Timeout = time.Millisecond
	if _, status := shadowprocess.Run(context.Background(), cfg, nil); status != "timeout" {
		t.Fatalf("unexpected deadline status %q", status)
	}
	leftovers, err := filepath.Glob("/tmp/kanz-shadow-*")
	if err != nil || len(leftovers) > 0 {
		t.Fatal("worker scratch survived completion")
	}
	children, err := filepath.Glob("/proc/self/task/*/children")
	if err != nil || len(children) == 0 {
		t.Fatal("process children unavailable")
	}
	for _, path := range children {
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		} // the runtime may retire an OS thread
		if err != nil || strings.TrimSpace(string(data)) != "" {
			t.Fatal("supervisor still owns a child")
		}
	}
}
