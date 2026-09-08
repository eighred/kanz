package arch

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDataPlaneBootstrapPreservesCredentialGroupsAndGeneratesSigningMaterialInVault(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the production bootstrap is POSIX shell and runs inside the Linux Vault container")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh is unavailable")
	}

	tmp := t.TempDir()
	state := filepath.Join(tmp, "state")
	logPath := filepath.Join(tmp, "vault.log")
	fakeVault := filepath.Join(tmp, "vault")
	trust := filepath.Join(tmp, "bundle.pem")
	marker := filepath.Join(tmp, "complete")
	if err := os.WriteFile(trust, []byte("test trust anchor\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stub := `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$FAKE_VAULT_LOG"
case "$1 $2" in
  "token lookup"|"policy write") exit 0 ;;
  "audit list") printf '%s\n' '{"file/":{}}'; exit 0 ;;
  "auth list") printf '%s\n' '{"kubernetes/":{}}'; exit 0 ;;
  "secrets list")
    if [ -f "$FAKE_VAULT_STATE/transit" ]; then printf '%s\n' '{"kv/":{},"bootstrap-transit/":{}}'; else printf '%s\n' '{"kv/":{}}'; fi
    exit 0 ;;
  "secrets enable") : > "$FAKE_VAULT_STATE/transit"; exit 0 ;;
  "secrets disable") rm -f "$FAKE_VAULT_STATE/transit"; exit 0 ;;
  "kv metadata") exit 0 ;;
  "kv get")
    field=''; path=''
    for arg in "$@"; do case "$arg" in -field=*) field=${arg#-field=};; kv/*) path=$arg;; esac; done
	case "$path:$field" in
	  kv/kanz/api-gateway:signing_secret) test -f "$FAKE_VAULT_STATE/gateway" ;;
	  kv/kanz/identity:signing_key_pem) test -f "$FAKE_VAULT_STATE/identity" ;;
	  kv/kanz/testnet/postgres:accounting_app) exit 0 ;;
	  kv/kanz/testnet/postgres:accounting_migrate|kv/kanz/accounting:dsn|kv/kanz/accounting:migrate_dsn)
	    if [ "${FAKE_PARTIAL:-0}" = 1 ]; then exit 1; fi
	    exit 0 ;;
	  *) exit 0 ;;
    esac
    exit $? ;;
  "kv patch"|"kv put")
    value=$(cat)
    case "$*" in
      *"signing_secret=-"*) printf '%s' "$value" | grep -Eq '^[0-9a-f]{64}$'; : > "$FAKE_VAULT_STATE/gateway" ;;
      *"signing_key_pem=-"*) printf '%s' "$value" | grep -q '^-----BEGIN EC PRIVATE KEY-----'; printf '%s' "$value" | grep -q -- '-----END EC PRIVATE KEY-----$'; : > "$FAKE_VAULT_STATE/identity" ;;
      *) printf '%s\n' 'an existing database or Redis credential was overwritten' >&2; exit 91 ;;
    esac
    exit 0 ;;
  "read bootstrap-transit/keys/identity-token") test -f "$FAKE_VAULT_STATE/transit-key"; exit $? ;;
  "read -format=json")
    cat <<'JSON'
{
  "data": {
    "keys": {
      "1": "-----BEGIN EC PRIVATE KEY-----\nFAKEKEY\n-----END EC PRIVATE KEY-----\n"
    }
  }
}
JSON
    exit 0 ;;
  "write bootstrap-transit/keys/identity-token") : > "$FAKE_VAULT_STATE/transit-key"; exit 0 ;;
  write*) exit 0 ;;
esac
printf 'unexpected fake Vault call: %s\n' "$*" >&2
exit 92
`
	if err := os.WriteFile(fakeVault, []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}

	script := filepath.Join(filepath.Dir(moduleRoot(t)), "tools", "bootstrap-testnet-data-plane.sh")
	commonEnv := append(os.Environ(),
		"VAULT_BIN="+fakeVault,
		"VAULT_CACERT="+trust,
	)
	cmd := exec.Command("sh", script)
	cmd.Env = append(commonEnv,
		"COMPLETION_MARKER="+marker,
		"FAKE_VAULT_STATE="+state,
		"FAKE_VAULT_LOG="+logPath,
	)
	cmd.Stdin = strings.NewReader("operator-token\n")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bootstrap failed: %v\n%s", err, output)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("completion marker absent: %v", err)
	}
	for _, name := range []string{"gateway", "identity"} {
		if _, err := os.Stat(filepath.Join(state, name)); err != nil {
			t.Fatalf("%s signing material was not written: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(state, "transit")); !os.IsNotExist(err) {
		t.Fatalf("temporary Transit mount was not removed: %v", err)
	}
	logBody, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	logText := string(logBody)
	if strings.Contains(logText, "_app=-") || strings.Contains(logText, "migrate_dsn=-") || strings.Contains(logText, "redis_dsn=-") {
		t.Fatalf("existing credential group was changed:\n%s", logText)
	}
	if strings.Contains(logText, "FAKEKEY") || strings.Contains(logText, "operator-token") {
		t.Fatalf("a secret entered a command argument or log:\n%s", logText)
	}

	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	rerun := exec.Command("sh", script)
	rerun.Env = cmd.Env
	rerun.Stdin = strings.NewReader("operator-token\n")
	if output, err := rerun.CombinedOutput(); err != nil {
		t.Fatalf("idempotent rerun failed: %v\n%s", err, output)
	}
	rerunLog, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rerunLog), "kv patch") || strings.Contains(string(rerunLog), "kv put") {
		t.Fatalf("idempotent rerun rewrote a credential:\n%s", rerunLog)
	}

	partialState := filepath.Join(tmp, "partial-state")
	partialMarker := filepath.Join(tmp, "partial-complete")
	partialLog := filepath.Join(tmp, "partial-vault.log")
	if err := os.MkdirAll(partialState, 0o700); err != nil {
		t.Fatal(err)
	}
	partial := exec.Command("sh", script)
	partial.Env = append(commonEnv,
		"FAKE_PARTIAL=1",
		"FAKE_VAULT_STATE="+partialState,
		"FAKE_VAULT_LOG="+partialLog,
		"COMPLETION_MARKER="+partialMarker,
	)
	partial.Stdin = strings.NewReader("operator-token\n")
	partialOutput, err := partial.CombinedOutput()
	if err == nil {
		t.Fatalf("partial credential group was accepted:\n%s", partialOutput)
	}
	if !strings.Contains(string(partialOutput), "Credential group accounting database is partial (1/4 fields)") {
		t.Fatalf("partial credential failure was not explicit:\n%s", partialOutput)
	}
	if _, err := os.Stat(partialMarker); !os.IsNotExist(err) {
		t.Fatalf("partial bootstrap emitted a completion marker: %v", err)
	}
}
