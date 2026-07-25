package tui

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Env is everything a PTY proof needs to reach the platform.
type Env struct {
	GatewayURL    string
	Token         string
	SigningSecret string
	Node2IP       string
	SSHKeyPath    string
	Binary        string
}

// requireEnv gathers the environment or SKIPS — loudly, naming what is missing.
//
// A skip is not a pass. The board records these workflows as verified only against a
// run in which they executed, so a silent skip that reads as green is the one outcome
// this function must make impossible.
func requireEnv(t *testing.T) Env {
	t.Helper()
	need := map[string]string{
		"KANZ_E2E_GATEWAY":  os.Getenv("KANZ_E2E_GATEWAY"),
		"KANZ_E2E_TOKEN":    os.Getenv("KANZ_E2E_TOKEN"),
		"KANZ_E2E_NODE2_IP": os.Getenv("KANZ_E2E_NODE2_IP"),
		"KANZ_E2E_SSH_KEY":  os.Getenv("KANZ_E2E_SSH_KEY"),
	}
	var missing []string
	for k, v := range need {
		if strings.TrimSpace(v) == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		t.Skipf("SKIPPING a real end-to-end proof — unset: %s. This is NOT a pass: the workflow "+
			"was not exercised. Set these against a live two-node cluster to run it.",
			strings.Join(missing, ", "))
	}
	return Env{
		GatewayURL:    need["KANZ_E2E_GATEWAY"],
		Token:         need["KANZ_E2E_TOKEN"],
		SigningSecret: os.Getenv("KANZ_E2E_SIGNING_SECRET"),
		Node2IP:       need["KANZ_E2E_NODE2_IP"],
		SSHKeyPath:    need["KANZ_E2E_SSH_KEY"],
		Binary:        buildTUI(t),
	}
}

// buildTUI compiles the real binary under test once per run.
func buildTUI(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "universe")
	cmd := exec.Command("go", "build", "-o", out, "./cmd/universe")
	cmd.Dir = moduleDir(t)
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build ./cmd/universe: %v\n%s", err, b)
	}
	return out
}

// moduleDir walks up to the directory holding go.mod.
func moduleDir(t *testing.T) string {
	t.Helper()
	d, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			t.Fatal("no go.mod found above the test directory")
		}
		d = parent
	}
}

// sessionEnv is the environment the TUI itself needs.
func (e Env) sessionEnv() []string {
	env := []string{
		"KANZ_GATEWAY_URL=" + e.GatewayURL,
		"KANZ_TOKEN=" + e.Token,
	}
	if e.SigningSecret != "" {
		env = append(env, "KANZ_SIGNING_SECRET="+e.SigningSecret)
	}
	return env
}

// mintOperatorToken builds an HS256 token carrying the baseline and operator roles —
// the dev-validator path. Production authenticates with OIDC and this helper has no
// equivalent there, which is why slice 4 exists.
func mintOperatorToken(secret string) string {
	b64 := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	header := b64([]byte(`{"alg":"HS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{
		"sub":    "e2e",
		"tenant": "fund-alpha",
		"roles":  []string{"kanz-user", "kanz-operator"},
		"exp":    time.Now().Add(time.Hour).Unix(),
	})
	payload := b64(claims)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(header + "." + payload))
	return fmt.Sprintf("%s.%s.%s", header, payload, b64(mac.Sum(nil)))
}
