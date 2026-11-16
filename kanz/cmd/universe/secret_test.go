package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The architecture exception is an explicit file-path API, not permission to
// introduce an environment default. Exercise the actual CLI helper, with both
// credential keys replaced by dummy values before any call.
func TestSecretFromPreservesExplicitPathAndEnvironmentKey(t *testing.T) {
	for _, key := range []string{"KANZ_TOKEN", "KANZ_SIGNING_SECRET"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv("KANZ_TOKEN", "  fixture-token-value \n")
			t.Setenv("KANZ_SIGNING_SECRET", "  fixture-signing-value \n")
			want := "fixture-token-value"
			if key == "KANZ_SIGNING_SECRET" {
				want = "fixture-signing-value"
			}
			value, err := secretFrom("", key)
			if err != nil || value != want {
				t.Fatal("environment read did not use and trim the requested key")
			}
			path := filepath.Join(t.TempDir(), "credential.fixture")
			if err := os.WriteFile(path, []byte("  fixture-file-value \n"), 0600); err != nil {
				t.Fatal(err)
			}
			value, err = secretFrom(path, key)
			if err != nil || value != "fixture-file-value" {
				t.Fatal("explicit file did not take precedence over the environment")
			}
			value, err = secretFrom(filepath.Join(t.TempDir(), "missing.fixture"), key)
			if err == nil || value != "" {
				t.Fatal("a failed explicit file read fell back to the environment")
			}
		})
	}
}
