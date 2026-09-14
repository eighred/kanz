package arch

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestVenueSecretRotationCommitsCompleteCredentialSetsAtomically(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the production bootstrap is POSIX shell and runs inside the Linux Vault container")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh is unavailable")
	}

	tmp := t.TempDir()
	fakeVault := filepath.Join(tmp, "vault")
	trust := filepath.Join(tmp, "bundle.pem")
	if err := os.WriteFile(trust, []byte("test trust anchor\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stub := `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$FAKE_VAULT_LOG"
case "$1 $2" in
  "token lookup") exit 0 ;;
  "kv metadata") exit 0 ;;
  "kv patch")
    path=$3
    shift 3
    fields=''
    for arg in "$@"; do
      field=${arg%%=*}
      source=${arg#*@}
      value=$(cat "$source")
      case "$path:$field:$value" in
        kv/kanz/venue-binance:api_key:binance-key|kv/kanz/venue-binance:api_secret:binance-secret|kv/kanz/venue-okx:api_key:okx-key|kv/kanz/venue-okx:api_secret:okx-secret|kv/kanz/venue-okx:api_passphrase:okx-passphrase) ;;
        *) printf 'wrong or incomplete credential field: %s:%s\n' "$path" "$field" >&2; exit 94 ;;
      esac
      fields="$fields $field"
    done
    case "$path:$fields" in
      "kv/kanz/venue-binance: api_key api_secret") : ;;
      "kv/kanz/venue-okx: api_key api_secret api_passphrase")
        if [ "${FAKE_FAIL_OKX:-0}" = 1 ]; then exit 95; fi ;;
      *) printf 'partial credential mutation: %s:%s\n' "$path" "$fields" >&2; exit 96 ;;
    esac
    exit 0 ;;
esac
printf 'unexpected fake Vault call: %s\n' "$*" >&2
exit 92
`
	if err := os.WriteFile(fakeVault, []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}

	script := filepath.Join(filepath.Dir(moduleRoot(t)), "tools", "bootstrap-testnet-venues.sh")
	run := func(t *testing.T, input string, failOKX bool) (string, string, string, error) {
		t.Helper()
		caseDir := t.TempDir()
		logPath := filepath.Join(caseDir, "vault.log")
		marker := filepath.Join(caseDir, "complete")
		cmd := exec.Command("sh", script, "rotate")
		cmd.Env = append(os.Environ(),
			"VAULT_BIN="+fakeVault,
			"VAULT_CACERT="+trust,
			"VENUE_SECRET_FIFO_ROOT="+caseDir,
			"COMPLETION_MARKER="+marker,
			"FAKE_VAULT_LOG="+logPath,
		)
		if failOKX {
			cmd.Env = append(cmd.Env, "FAKE_FAIL_OKX=1")
		}
		cmd.Stdin = strings.NewReader(input)
		output, err := cmd.CombinedOutput()
		logBody, readErr := os.ReadFile(logPath)
		if readErr != nil && !os.IsNotExist(readErr) {
			t.Fatal(readErr)
		}
		return string(output), string(logBody), marker, err
	}

	const allInput = "operator-token\nbinance-key\nbinance-secret\nokx-key\nokx-secret\nokx-passphrase\n"
	t.Run("success", func(t *testing.T) {
		output, logText, marker, err := run(t, allInput, false)
		if err != nil {
			t.Fatalf("rotation failed: %v\n%s", err, output)
		}
		if _, err := os.Stat(marker); err != nil {
			t.Fatalf("completion marker absent: %v", err)
		}
		if strings.Count(logText, "kv patch") != 2 || strings.Contains(logText, "api_key=-") {
			t.Fatalf("expected exactly one pipe-backed Vault version per venue:\n%s", logText)
		}
		for _, secret := range strings.Split(strings.TrimSpace(allInput), "\n") {
			if strings.Contains(output, secret) || strings.Contains(logText, secret) {
				t.Fatalf("secret %q entered output or command arguments", secret)
			}
		}
	})

	t.Run("empty final input causes no mutation", func(t *testing.T) {
		input := "operator-token\nbinance-key\nbinance-secret\nokx-key\nokx-secret\n\n"
		_, logText, marker, err := run(t, input, false)
		if err == nil {
			t.Fatal("empty passphrase was accepted")
		}
		if strings.Contains(logText, "kv patch") || strings.Contains(logText, "kv put") {
			t.Fatalf("Vault mutated before every credential was collected:\n%s", logText)
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("failed rotation emitted a completion marker: %v", err)
		}
	})

	t.Run("failed venue write cannot create partial field versions", func(t *testing.T) {
		_, logText, marker, err := run(t, allInput, true)
		if err == nil {
			t.Fatal("injected OKX failure was accepted")
		}
		if strings.Count(logText, "kv patch") != 2 {
			t.Fatalf("failure made per-field mutations:\n%s", logText)
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("failed rotation emitted a completion marker: %v", err)
		}
	})
}
