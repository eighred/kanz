package arch

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// The dedicated proof's literal Go input is outside the Dockerfile walk. Bind
// its current declaration/pull forms to the same canonical builder and module;
// this is pin agreement, not an advisory or image-qualification verdict.
func runtimeCredentialProofGoPins(proof, workflow, builder, toolchain string) error {
	proof = strings.ReplaceAll(proof, "\r\n", "\n")
	workflow = strings.ReplaceAll(workflow, "\r\n", "\n")
	builder = strings.ReplaceAll(builder, "\r\n", "\n")
	froms := fromGolangLine.FindAllStringSubmatch(builder, -1)
	if len(froms) != 1 || !regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+@sha256:[0-9a-f]{64}$`).MatchString(froms[0][1]) {
		return fmt.Errorf("canonical Go builder pin missing or ambiguous")
	}
	version, _, _ := strings.Cut(froms[0][1], "@")
	if normalizeVersion(version) != normalizeVersion(toolchain) {
		return fmt.Errorf("proof builder disagrees with module toolchain")
	}
	expected := mirrorPrefix + "golang:" + froms[0][1]
	images := regexp.MustCompile(`(?m)^FROM\s+((?:\S+/)?golang:\S+)`).FindAllStringSubmatch(builder, -1)
	if len(images) != 1 || images[0][1] != expected {
		return fmt.Errorf("canonical Go builder registry differs")
	}
	declarations := regexp.MustCompile(`(?m)^GO = "([^"\n]+)"$`).FindAllStringSubmatch(proof, -1)
	pulls := regexp.MustCompile(`(?m)^[ \t]*docker pull (ghcr\.io/eighred/base/golang:[^\s]+)[ \t]*$`).FindAllStringSubmatch(workflow, -1)
	if len(declarations) != 1 || len(pulls) != 1 || declarations[0][1] != expected || pulls[0][1] != expected {
		return fmt.Errorf("dedicated proof Go image disagrees with canonical builder")
	}
	if strings.Count(workflow, "- uses: actions/setup-go@") != 1 || strings.Count(workflow, "go-version-file: kanz/go.mod") != 1 || strings.Contains(workflow, "go-version:") {
		return fmt.Errorf("proof provisioning must use the canonical Go module version")
	}
	return nil
}

func TestRuntimeCredentialProofUsesCanonicalGoInputs(t *testing.T) {
	root := moduleRoot(t)
	repo := filepath.Dir(root)
	proof := string(mustReadArchFile(t, filepath.Join(repo, "tools", "runtime_credentials", "proof.py")))
	workflow := string(mustReadArchFile(t, filepath.Join(repo, ".github", "workflows", "runtime-credentials-proof.yml")))
	builder := string(mustReadArchFile(t, filepath.Join(root, "services", "identity", "Dockerfile")))
	toolchain := goModToolchainVersion(t, root)
	if err := runtimeCredentialProofGoPins(proof, workflow, builder, toolchain); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(string, string, string, string) (string, string, string, string){
		"missing-declaration": func(p, w, b, v string) (string, string, string, string) {
			return strings.Replace(p, "GO = ", "REMOVED = ", 1), w, b, v
		},
		"duplicate-declaration": func(p, w, b, v string) (string, string, string, string) {
			return p + "\nGO = \"unreviewed\"\n", w, b, v
		},
		"different-image": func(p, w, b, v string) (string, string, string, string) {
			return strings.Replace(p, "GO = \"", "GO = \"different/", 1), w, b, v
		},
		"missing-pull": func(p, w, b, v string) (string, string, string, string) {
			return p, strings.ReplaceAll(w, "docker pull ghcr.io/eighred/base/golang:", "removed pull ghcr.io/eighred/base/golang:"), b, v
		},
		"duplicate-pull": func(p, w, b, v string) (string, string, string, string) {
			return p, w + "\ndocker pull ghcr.io/eighred/base/golang:unreviewed\n", b, v
		},
		"different-pull": func(p, w, b, v string) (string, string, string, string) {
			return p, strings.ReplaceAll(w, "docker pull ghcr.io/eighred/base/golang:", "docker pull ghcr.io/eighred/base/golang:different-"), b, v
		},
		"different-module": func(p, w, b, v string) (string, string, string, string) {
			return p, strings.ReplaceAll(w, "go-version-file: kanz/go.mod", "go-version-file: other/go.mod"), b, v
		},
		"independent-host-version": func(p, w, b, v string) (string, string, string, string) {
			return p, strings.ReplaceAll(w, "go-version-file: kanz/go.mod", "go-version: '0.0.0'"), b, v
		},
		"different-toolchain": func(p, w, b, v string) (string, string, string, string) {
			return p, w, b, "0.0.0"
		},
		"different-builder-registry": func(p, w, b, v string) (string, string, string, string) {
			return p, w, strings.ReplaceAll(b, mirrorPrefix+"golang:", "different.example/golang:"), v
		},
	} {
		t.Run(name, func(t *testing.T) {
			p, w, b, v := mutate(proof, workflow, builder, toolchain)
			if runtimeCredentialProofGoPins(p, w, b, v) == nil {
				t.Fatal("changed proof input was accepted")
			}
		})
	}
}

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
  "token lookup") exit 0 ;;
  "read auth/kubernetes/role/postgres-provisioner") exit 0 ;;
  "policy write") cat > "$FAKE_VAULT_STATE/$3.policy"; exit 0 ;;
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
	  kv/kanz/accounting-runtime:credential_isolation_state|kv/kanz/audit-runtime:credential_isolation_state|kv/kanz/identity-runtime:credential_isolation_state)
	    printf 'Field "%s" not present in secret\n' "$field" >&2; exit 1 ;;
	  kv/kanz/api-gateway:signing_secret) test -f "$FAKE_VAULT_STATE/gateway" || { printf 'Field "%s" not present in secret\n' "$field" >&2; exit 1; } ;;
	  kv/kanz/identity:signing_key_pem) test -f "$FAKE_VAULT_STATE/identity" || { printf 'Field "%s" not present in secret\n' "$field" >&2; exit 1; } ;;
	  kv/kanz/testnet/postgres:accounting_app) exit 0 ;;
	  kv/kanz/testnet/postgres:accounting_migrate|kv/kanz/accounting:dsn|kv/kanz/accounting:migrate_dsn)
	    if [ "${FAKE_PARTIAL:-0}" = 1 ]; then printf 'Field "%s" not present in secret\n' "$field" >&2; exit 1; fi
	    exit 0 ;;
      kv/kanz/tv-sync-runtime:migrate_dsn)
        if [ "${FAKE_LOOKUP_FAILURE:-0}" = 1 ]; then printf '%s\n' 'permission denied' >&2; exit 2; fi
        if [ "${FAKE_TV_MIGRATE_DSN:-0}" = 1 ]; then exit 0; fi
        printf 'Field "%s" not present in secret\n' "$field" >&2; exit 1 ;;
      kv/kanz/testnet/postgres:tv_sync_app) exit 0 ;;
      kv/kanz/testnet/postgres:tv_sync_migrate|kv/kanz/tv-sync-runtime:dsn)
        if [ "${FAKE_PARTIAL_TV:-0}" = 1 ]; then printf 'Field "%s" not present in secret\n' "$field" >&2; exit 1; fi
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
	cmd := exec.Command("sh", script, "tv-sync")
	cmd.Env = append(commonEnv,
		"COMPLETION_MARKER="+marker,
		"FAKE_VAULT_STATE="+state,
		"FAKE_VAULT_LOG="+logPath,
	)
	cmd.Stdin = strings.NewReader("operator-token\n")
	base := exec.Command("sh", script)
	base.Env = cmd.Env
	base.Stdin = strings.NewReader("operator-token\n")
	if output, err := base.CombinedOutput(); err != nil {
		t.Fatalf("ordinary bootstrap: %v\n%s", err, output)
	}
	baseLog, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(baseLog), "tv-sync") || strings.Contains(string(baseLog), "tv_sync") {
		t.Fatal("ordinary bootstrap depends on unactivated tv-sync credentials")
	}
	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
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
	for _, unrelated := range []string{"kv get -field=accounting", "kv get -field=signing", "write auth/kubernetes/config", "policy write accounting-read"} {
		if strings.Contains(logText, unrelated) {
			t.Fatalf("tv preparation reconciled unrelated bootstrap state: %s", unrelated)
		}
	}
	for _, required := range []string{"write auth/kubernetes/role/tv-sync bound_service_account_names=tv-sync bound_service_account_namespaces=kanz-services", "kv get -field=tv_sync_app kv/kanz/testnet/postgres", "kv get -field=dsn kv/kanz/tv-sync-runtime", "token_no_default_policy=true"} {
		if !strings.Contains(logText, required) {
			t.Fatalf("tv-sync bootstrap boundary missing: %s", required)
		}
	}
	policy, err := os.ReadFile(filepath.Join(state, "tv-sync-read.policy"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(policy), `path "kv/data/kanz/tv-sync-runtime"`) || strings.Contains(string(policy), `path "kv/data/kanz/tv-sync"`) || strings.Contains(string(policy), "kanz/testnet/postgres") || strings.Contains(string(policy), "*") {
		t.Fatal("runtime policy can expose migration secret records")
	}
	if strings.Contains(logText, "_app=-") || strings.Contains(logText, "migrate_dsn=-") || strings.Contains(logText, "redis_dsn=-") {
		t.Fatalf("existing credential group was changed:\n%s", logText)
	}
	if strings.Contains(logText, "FAKEKEY") || strings.Contains(logText, "operator-token") {
		t.Fatalf("a secret entered a command argument or log:\n%s", logText)
	}

	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	rerun := exec.Command("sh", script, "tv-sync")
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
	partialTV := exec.Command("sh", script, "tv-sync")
	partialTV.Env = append(commonEnv, "FAKE_PARTIAL_TV=1", "FAKE_VAULT_STATE="+partialState, "FAKE_VAULT_LOG="+partialLog, "COMPLETION_MARKER="+partialMarker)
	partialTV.Stdin = strings.NewReader("operator-token\n")
	partialTVOutput, err := partialTV.CombinedOutput()
	if err == nil || !strings.Contains(string(partialTVOutput), "Credential group tv-sync database is partial (1/3 fields)") {
		t.Fatalf("partial tv-sync group not refused explicitly: %v %s", err, partialTVOutput)
	}
	if _, err := os.Stat(partialMarker); !os.IsNotExist(err) {
		t.Fatal("partial tv-sync bootstrap emitted completion")
	}
	leaked := exec.Command("sh", script, "tv-sync")
	leaked.Env = append(commonEnv, "FAKE_TV_MIGRATE_DSN=1", "FAKE_VAULT_STATE="+partialState, "FAKE_VAULT_LOG="+partialLog, "COMPLETION_MARKER="+partialMarker)
	leaked.Stdin = strings.NewReader("operator-token\n")
	leakedOutput, err := leaked.CombinedOutput()
	if err == nil || !strings.Contains(string(leakedOutput), "runtime KV contains a migration credential") {
		t.Fatalf("runtime migration credential not refused: %v %s", err, leakedOutput)
	}
	if _, err := os.Stat(filepath.Join(partialState, "tv-sync-read.policy")); !os.IsNotExist(err) {
		t.Fatal("runtime policy published before credential qualification")
	}
	unknown := exec.Command("sh", script, "tv-sync")
	unknown.Env = append(commonEnv, "FAKE_LOOKUP_FAILURE=1", "FAKE_VAULT_STATE="+partialState, "FAKE_VAULT_LOG="+partialLog, "COMPLETION_MARKER="+partialMarker)
	unknown.Stdin = strings.NewReader("operator-token\n")
	unknownOutput, err := unknown.CombinedOutput()
	if err == nil || !strings.Contains(string(unknownOutput), "absence is unverified") {
		t.Fatalf("unknown Vault result accepted: %v %s", err, unknownOutput)
	}
	if _, err := os.Stat(filepath.Join(partialState, "tv-sync-read.policy")); !os.IsNotExist(err) {
		t.Fatal("runtime policy published after unknown Vault read")
	}

}
