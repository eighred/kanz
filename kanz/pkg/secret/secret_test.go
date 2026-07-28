package secret

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestReadFallsBackToThePlaintextEnvWhenNoMountIsDeclared(t *testing.T) {
	t.Setenv("KANZ_TEST_VALUE", "from-env")

	got, err := Read("KANZ_TEST_VALUE")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != "from-env" {
		t.Errorf("Read = %q, want %q", got, "from-env")
	}
}

func TestReadPrefersTheMountOverThePlaintextEnv(t *testing.T) {
	p := filepath.Join(t.TempDir(), "dsn")
	if err := os.WriteFile(p, []byte("  from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KANZ_TEST_VALUE", "from-env")
	t.Setenv("KANZ_TEST_VALUE_FILE", p)

	got, err := Read("KANZ_TEST_VALUE")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	// Trimmed: a Vault CSI mount routinely carries a trailing newline, and a DSN
	// with one appended fails to parse in a way that names the driver, not the
	// mount.
	if got != "from-file" {
		t.Errorf("Read = %q, want %q (trimmed file contents, not the env var)", got, "from-file")
	}
}

// THE TEST THIS PACKAGE EXISTS FOR.
//
// Fifteen copies of this helper answered an unreadable mount with the plaintext
// env var, and then with "". A service starting on an empty DSN because its
// Vault mount failed looks exactly like a service starting on an empty DSN
// because nobody configured one — and only one of those is an incident.
func TestReadRefusesADeclaredButMissingMount(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "never-written")
	t.Setenv("KANZ_TEST_VALUE", "from-env")
	t.Setenv("KANZ_TEST_VALUE_FILE", missing)

	got, err := Read("KANZ_TEST_VALUE")
	if err == nil {
		t.Fatalf("Read returned %q and no error for an unreadable mount — the deployment "+
			"declared a secret file and it is not there; falling back to the env var hides that", got)
	}
	if got != "" {
		t.Errorf("Read returned %q alongside an error; it must return no value at all", got)
	}
	// The error must name the variable AND the path. A Job's last log line
	// before it exits is often all anyone gets, and "no such file or directory"
	// on its own names neither what was being read nor what to fix.
	if !strings.Contains(err.Error(), "KANZ_TEST_VALUE_FILE") || !strings.Contains(err.Error(), missing) {
		t.Errorf("error %q must name both the env var and the path", err)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("error must wrap the underlying cause, got %v", err)
	}
}

// A mount that exists but cannot be read is the same class of fault as one that
// is absent, and is if anything MORE likely in production: a CSI volume mounted
// with the wrong fsGroup is readable by root and nothing else, while the
// container runs as non-root by policy.
func TestReadRefusesAnUnreadableMount(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod permission bits are not enforced the same way on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root — a 0o000 file is still readable, so this proves nothing")
	}
	p := filepath.Join(t.TempDir(), "locked")
	if err := os.WriteFile(p, []byte("secret"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KANZ_TEST_VALUE", "from-env")
	t.Setenv("KANZ_TEST_VALUE_FILE", p)

	if got, err := Read("KANZ_TEST_VALUE"); err == nil {
		t.Fatalf("Read returned %q and no error for a permission-denied mount", got)
	}
}

// An EXISTING but empty file resolves to "" with no error, and that is
// deliberate. Read cannot know whether a given value is optional; what it
// guarantees is that "" means the value was genuinely absent rather than that
// reading it failed. Callers that require a value check for emptiness
// themselves — several already do.
func TestReadTreatsAnEmptyFileAsAnEmptyValueNotAFailure(t *testing.T) {
	p := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(p, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KANZ_TEST_VALUE_FILE", p)

	got, err := Read("KANZ_TEST_VALUE")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != "" {
		t.Errorf("Read = %q, want empty", got)
	}
}

// Unset everywhere is not an error either — no mount was claimed, so there is
// nothing to have failed.
func TestReadReturnsEmptyWhenNothingIsSet(t *testing.T) {
	got, err := Read("KANZ_TEST_DEFINITELY_UNSET")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != "" {
		t.Errorf("Read = %q, want empty", got)
	}
}
