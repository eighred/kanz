package arch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Source-shape guard; actual readonly registry/signature/ABI proof is separate.
func rustAuditConsumerBoundary(c, installer, workflow string) error {
	for _, token := range []string{
		`PRODUCER_SOURCE = "a2dbaf41fd77da6dc30380f133e80802488fe7c2"`,
		`CONFIG = "sha256:e523902a82786dd3dfb6aa9c5b155b9516c11fd22f86cbb7db6d9bb36f762532"`,
		`DIGEST = "sha256:0579c6075afce18b98b97864a056d0e553d0281e6d12c598ab8e3042ae88de71"`,
		`IMAGE = q.REPOSITORY + "@" + DIGEST`,
		`claim.get("critical", {}).get("type") != "https://sigstore.dev/cosign/sign/v1"`,
		`.get("docker-reference") != IMAGE`,
		`.get("docker-manifest-digest") != DIGEST`,
		`.get("source_sha") != PRODUCER_SOURCE`,
		`json.loads(raw, object_pairs_hook=q.strict)`,
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
	var parsed struct {
		Jobs map[string]struct {
			If       any `yaml:"if"`
			Continue any `yaml:"continue-on-error"`
			Steps    []struct {
				Name     string `yaml:"name"`
				Uses     string `yaml:"uses"`
				Run      string `yaml:"run"`
				If       any    `yaml:"if"`
				Continue any    `yaml:"continue-on-error"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(workflow), &parsed); err != nil {
		return err
	}
	job, ok := parsed.Jobs["compatibility"]
	if !ok || job.If != nil || job.Continue != nil {
		return fmt.Errorf("compatibility must be mandatory")
	}
	setup, consumer := -1, -1
	for i, step := range job.Steps {
		if strings.HasPrefix(step.Uses, "sigstore/cosign-installer@") {
			if setup != -1 || step.Uses != "sigstore/cosign-installer@6f9f17788090df1f26f669e9d70d6ae9567deba6" || step.If != nil || step.Continue != nil || step.Run != "" {
				return fmt.Errorf("compatibility cosign setup differs")
			}
			setup = i
		}
		if step.Name == "Install pinned Rust evidence tools" {
			if consumer != -1 || step.If != nil || step.Continue != nil || step.Uses != "" || step.Run != "bash .github/scripts/install-rust-audit.sh" {
				return fmt.Errorf("compatibility consumer differs")
			}
			consumer = i
		}
	}
	if setup < 0 || consumer <= setup {
		return fmt.Errorf("compatibility cosign must precede consumer")
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
		{"claim-type", 0, `"https://sigstore.dev/cosign/sign/v1"`, `"cosign container image signature"`},
		{"claim-reference", 0, `.get("docker-reference") != IMAGE`, `.get("docker-reference") != q.REPOSITORY`},
		{"claim-digest", 0, `.get("docker-manifest-digest") != DIGEST`, `False`},
		{"claim-source", 0, `.get("source_sha") != PRODUCER_SOURCE`, `False`},
		{"claim-duplicates", 0, `json.loads(raw, object_pairs_hook=q.strict)`, `json.loads(raw)`},
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
			if tc.name == "setup" {
				// Mutate the owning job, retaining the unrelated owner installer.
				start := strings.Index(v[2], "\n  compatibility:")
				if start < 0 {
					t.Fatal("compatibility missing")
				}
				v[2] = v[2][:start] + strings.Replace(v[2][start:], tc.old, tc.new, 1)
			} else {
				v[tc.file] = strings.Replace(v[tc.file], tc.old, tc.new, 1)
			}
			if v[tc.file] == inputs[tc.file] {
				t.Fatal("mutation missed")
			}
			if rustAuditConsumerBoundary(v[0], v[1], v[2]) == nil {
				t.Fatal("mutation accepted")
			}
		})
	}
}

func TestRustAuditConsumerJobComposition(t *testing.T) {
	root := filepath.Dir(moduleRoot(t))
	read := func(path string) string {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			t.Fatal(err)
		}
		return strings.ReplaceAll(string(b), "\r\n", "\n")
	}
	c, installer, workflow := read("tools/rust-audit-tools/consume.py"), read(".github/scripts/install-rust-audit.sh"), read(".github/workflows/architecture-migration.yml")
	start := strings.Index(workflow, "\n  compatibility:")
	if start < 0 {
		t.Fatal("compatibility missing")
	}
	prefix, job := workflow[:start], workflow[start:]
	setup := "      - uses: sigstore/cosign-installer@6f9f17788090df1f26f669e9d70d6ae9567deba6 # v4.1.2\n"
	consumer := "      - name: Install pinned Rust evidence tools\n        run: bash .github/scripts/install-rust-audit.sh\n"
	if strings.Count(job, setup) != 1 || strings.Count(job, consumer) != 1 {
		t.Fatal("mutation anchors differ")
	}
	// A second unrelated installer is valid but cannot satisfy this job's prerequisite.
	unrelated := "\n  unrelated:\n    steps:\n" + setup
	if err := rustAuditConsumerBoundary(c, installer, workflow+unrelated); err != nil {
		t.Fatal(err)
	}
	mutations := map[string]string{
		"removed-with-second-installer": strings.Replace(job, setup, "", 1),
		"reordered":                     strings.Replace(strings.Replace(job, setup, "", 1), consumer, consumer+setup, 1),
		"setup-if":                      strings.Replace(job, setup, setup+"        if: false\n", 1),
		"setup-continue":                strings.Replace(job, setup, setup+"        continue-on-error: true\n", 1),
		"consumer-if":                   strings.Replace(job, consumer, consumer+"        if: false\n", 1),
		"consumer-continue":             strings.Replace(job, consumer, consumer+"        continue-on-error: true\n", 1),
		"consumer-command":              strings.Replace(job, "run: bash .github/scripts/install-rust-audit.sh", "run: echo skipped", 1),
		"job-if":                        strings.Replace(job, "  compatibility:\n", "  compatibility:\n    if: false\n", 1),
		"job-continue":                  strings.Replace(job, "  compatibility:\n", "  compatibility:\n    continue-on-error: true\n", 1),
	}
	for name, mutated := range mutations {
		t.Run(name, func(t *testing.T) {
			if mutated == job {
				t.Fatal("mutation missed")
			}
			if rustAuditConsumerBoundary(c, installer, prefix+mutated+unrelated) == nil {
				t.Fatal("mutation accepted")
			}
		})
	}
}
