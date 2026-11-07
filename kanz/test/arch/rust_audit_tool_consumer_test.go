package arch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Source-shape guard; actual readonly registry/signature/ABI proof is separate.
func rustAuditConsumerBoundary(c, installer, workflow string) error {
	for _, token := range []string{
		`PRODUCER_SOURCE = "a2dbaf41fd77da6dc30380f133e80802488fe7c2"`,
		`CONFIG = "sha256:e523902a82786dd3dfb6aa9c5b155b9516c11fd22f86cbb7db6d9bb36f762532"`,
		`DIGEST = "sha256:0579c6075afce18b98b97864a056d0e553d0281e6d12c598ab8e3042ae88de71"`,
		`IMAGE = q.REPOSITORY + "@" + DIGEST`,
		`"--certificate-oidc-issuer", q.ISSUER`, `"--certificate-identity", q.SIGNER`,
		`"source_sha=" + PRODUCER_SOURCE`, `signature_claims(q.read_bounded(signatures, 1024 * 1024))`,
		`q.extract_saved(q.read_bounded(archive, q.MAX_PAYLOAD), CONFIG, temp, PRODUCER_SOURCE)`,
		`advisories = q.exercise(temp)`, `consumer_source = q.source()`,
		`"consumer_source": consumer_source, "producer_source": PRODUCER_SOURCE`,
	} {
		if !strings.Contains(c, token) {
			return fmt.Errorf("consumer boundary missing %q", token)
		}
	}
	previous := -1
	for _, token := range []string{`q.run(["cosign", "verify"`, `signature_claims(q.read_bounded`, `q.run(["docker", "pull"`, `image, files = q.extract_saved`, `advisories = q.exercise`} {
		at := strings.Index(c, token)
		if at <= previous {
			return fmt.Errorf("signature must precede extraction and execution")
		}
		previous = at
	}
	if strings.Contains(c, `os.environ["GITHUB_SHA"] =`) || strings.Contains(c, "cargo install") || strings.Contains(installer, "cargo ") || strings.Contains(installer, "||") {
		return fmt.Errorf("source spoof/fallback")
	}
	for _, token := range []string{"  contents: read\n  packages: read", "sigstore/cosign-installer@6f9f17788090df1f26f669e9d70d6ae9567deba6", "test_consume.py", "tools/rust-audit-tools/**", "rust-audit-tools-installed.json"} {
		if !strings.Contains(workflow, token) {
			return fmt.Errorf("readonly consumer qualification missing")
		}
	}
	if strings.Contains(workflow, "packages: write") || strings.Contains(workflow, "id-token: write") {
		return fmt.Errorf("consumer write authority")
	}
	if strings.Index(workflow, "sigstore/cosign-installer@") >= strings.Index(workflow, "name: Install pinned Rust evidence tools") {
		return fmt.Errorf("cosign setup order")
	}
	return nil
}
func TestRustAuditConsumerBoundary(t *testing.T) {
	root := filepath.Dir(moduleRoot(t))
	inputs := make([]string, 3)
	for i, n := range []string{"tools/rust-audit-tools/consume.py", ".github/scripts/install-rust-audit.sh", ".github/workflows/architecture-migration.yml"} {
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(n)))
		if err != nil {
			t.Fatal(err)
		}
		inputs[i] = strings.ReplaceAll(string(raw), "\r\n", "\n")
	}
	if err := rustAuditConsumerBoundary(inputs[0], inputs[1], inputs[2]); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		file     int
		old, new string
	}{
		{"source", 0, "a2dbaf41fd77da6dc30380f133e80802488fe7c2", "wrong"},
		{"config", 0, "e523902a82786dd3dfb6aa9c5b155b9516c11fd22f86cbb7db6d9bb36f762532", "wrong"},
		{"root", 0, "0579c6075afce18b98b97864a056d0e553d0281e6d12c598ab8e3042ae88de71", "wrong"},
		{"signer", 0, `"--certificate-identity", q.SIGNER`, `"--certificate-identity-regexp", ".*"`},
		{"issuer", 0, `"--certificate-oidc-issuer", q.ISSUER`, `"--insecure-ignore-tlog"`},
		{"claims", 0, "signature_claims(q.read_bounded", "unchecked(q.read_bounded"},
		{"archive-bound", 0, "q.read_bounded(archive, q.MAX_PAYLOAD)", "archive.read_bytes()"},
		{"runtime-controls", 0, "advisories = q.exercise(temp)", "advisories = []"},
		{"source-spoof", 0, "consumer_source = q.source()", "consumer_source = PRODUCER_SOURCE"},
		{"fallback", 1, "consume.py", "consume.py || cargo install cargo-audit"},
		{"permission", 2, "packages: read", "packages: write"},
		{"setup", 2, "sigstore/cosign-installer@", "missing/setup@"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := append([]string(nil), inputs...)
			v[tc.file] = strings.Replace(v[tc.file], tc.old, tc.new, 1)
			if v[tc.file] == inputs[tc.file] {
				t.Fatal("mutation missed")
			}
			if rustAuditConsumerBoundary(v[0], v[1], v[2]) == nil {
				t.Fatal("mutation accepted")
			}
		})
	}
}
