package arch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Bounded workflow-shape guard for #1413. Actual Linux image extraction and
// parser mutations remain independent evidence; this is not shell dataflow analysis.
func rustReleaseEvidenceBoundary(release, build, migration, installer string) error {
	type step struct {
		Name     string            `yaml:"name"`
		Run      string            `yaml:"run"`
		If       string            `yaml:"if"`
		Continue bool              `yaml:"continue-on-error"`
		With     map[string]string `yaml:"with"`
		Env      map[string]string `yaml:"env"`
	}
	type workflow struct {
		Jobs map[string]struct {
			Steps []step `yaml:"steps"`
		} `yaml:"jobs"`
	}
	var rel workflow
	if err := yaml.Unmarshal([]byte(release), &rel); err != nil {
		return err
	}
	const condition = "matrix.service == 'binance-depth' || matrix.service == 'okx-depth'"
	gate, sign, setup, sbom := -1, -1, -1, -1
	for i, s := range rel.Jobs["release"].Steps {
		if strings.Contains(s.Run, "cosign sign") || strings.Contains(s.Run, "cosign attest") {
			if s.If != "" || s.Continue {
				return fmt.Errorf("signing must retain success-only execution")
			}
			if sign < 0 {
				sign = i
			}
		}
		if s.With["output-file"] == "sbom.spdx.json" {
			if s.With["syft-version"] != "v1.56.0" || s.With["image"] != "${{ env.IMAGE }}@${{ steps.build.outputs.digest }}" || s.If != "" || s.Continue {
				return fmt.Errorf("actual digest SBOM required")
			}
			sbom = i
		}
		if s.Name == "Install pinned Rust evidence tools" {
			if s.Run != "bash .github/scripts/install-rust-audit.sh" || s.If != condition || s.Continue {
				return fmt.Errorf("Rust tool setup cannot skip or ignore failure")
			}
			setup = i
		}
		if s.Name == "Require actual Rust image inventory and current RustSec audit" {
			const expected = "python3 -B -m unittest discover -s .github/scripts -p test_rust_release_evidence.py\npython3 .github/scripts/rust_release_evidence.py --service \"$RUST_SERVICE\" --image \"$RUST_IMAGE\" --sbom sbom.spdx.json --output rust-evidence"
			if strings.TrimSpace(s.Run) != expected || s.If != condition || s.Continue || s.Env["RUST_SERVICE"] != "${{ matrix.service }}" || s.Env["RUST_IMAGE"] != "${{ env.IMAGE }}@${{ steps.build.outputs.digest }}" {
				return fmt.Errorf("Rust release gate must fail closed on exact image digest")
			}
			gate = i
		}
	}
	if setup < 0 || sbom < 0 || gate <= setup || gate <= sbom || sign <= gate {
		return fmt.Errorf("Rust evidence must precede signing")
	}
	for _, token := range []string{"cargo +1.90.0 install --locked --version 0.22.2 --root \"$tools\" cargo-audit", "cargo +1.90.0 install --locked --version 0.5.4 --root \"$tools\" rust-audit-info", "set -euo pipefail"} {
		if !strings.Contains(installer, token) {
			return fmt.Errorf("pinned Rust audit tools required")
		}
	}
	if !strings.Contains(build, "- 'kanz-rs/**'") {
		return fmt.Errorf("Rust-only PRs must build images")
	}
	for _, service := range []string{"binance-depth", "okx-depth"} {
		entry := "- service: " + service + "\n            dockerfile: kanz-rs/services/" + service + "/Dockerfile"
		if !strings.Contains(release, entry) || !strings.Contains(build, entry) {
			return fmt.Errorf("both Rust services must be built and released")
		}
		if !strings.Contains(migration, "rust_release_evidence.py --local --service "+service) {
			return fmt.Errorf("both images need nonpublishing evidence")
		}
	}
	for _, token := range []string{"'.github/scripts/*rust_release_evidence.py'", "'.github/scripts/*rust*audit*'", "syft-version: v1.56.0", "bash .github/scripts/install-rust-audit.sh"} {
		if !strings.Contains(migration, token) {
			return fmt.Errorf("nonpublishing gate must run on changes to its inputs")
		}
	}
	return nil
}

func rustReleaseInputs(t *testing.T) []string {
	t.Helper()
	root := filepath.Dir(moduleRoot(t))
	var out []string
	for _, name := range []string{"workflows/release.yml", "workflows/build.yml", "workflows/architecture-migration.yml", "scripts/install-rust-audit.sh"} {
		b, err := os.ReadFile(filepath.Join(root, ".github", filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, strings.ReplaceAll(string(b), "\r\n", "\n"))
	}
	return out
}

func TestRustReleaseEvidenceBoundary(t *testing.T) {
	v := rustReleaseInputs(t)
	if err := rustReleaseEvidenceBoundary(v[0], v[1], v[2], v[3]); err != nil {
		t.Fatal(err)
	}
}

func TestRustReleaseEvidenceMutations(t *testing.T) {
	for _, tc := range []struct {
		name             string
		file             int
		old, replacement string
	}{
		{"early-signing", 0, "      - name: Install pinned Rust evidence tools", "      - run: cosign sign unsafe\n      - name: Install pinned Rust evidence tools"},
		{"always-signs", 0, "      - name: Sign + attest SBOM and vuln scan (keyless)", "      - name: Sign + attest SBOM and vuln scan (keyless)\n        if: always()"},
		{"continue-on-failure", 0, "      - name: Require actual Rust image inventory and current RustSec audit", "      - name: Require actual Rust image inventory and current RustSec audit\n        continue-on-error: true"},
		{"skipped-gate", 0, "name: Require actual Rust", "name: Disabled actual Rust"},
		{"only-binance", 0, "if: matrix.service == 'binance-depth' || matrix.service == 'okx-depth'", "if: matrix.service == 'binance-depth'"},
		{"local-release-bypass", 0, "rust_release_evidence.py --service", "rust_release_evidence.py --local --service"},
		{"ignored-failure", 0, "--output rust-evidence\n", "--output rust-evidence || true\n"},
		{"mutable-image", 0, "RUST_IMAGE: ${{ env.IMAGE }}@${{ steps.build.outputs.digest }}", "RUST_IMAGE: ${{ env.IMAGE }}:latest"},
		{"missing-binance", 0, "service: binance-depth", "service: absent"},
		{"missing-okx", 1, "service: okx-depth", "service: absent"},
		{"missing-rust-trigger", 1, "- 'kanz-rs/**'", "- 'other/**'"},
		{"missing-ci-okx", 2, "--local --service okx-depth", "--local --service other"},
		{"floating-syft", 0, "syft-version: v1.56.0", "syft-version: latest"},
		{"floating-auditor", 3, "--version 0.22.2", ""},
		{"unlocked-extractor", 3, "--locked --version 0.5.4", "--version 0.5.4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := rustReleaseInputs(t)
			changed := strings.Replace(v[tc.file], tc.old, tc.replacement, 1)
			if changed == v[tc.file] {
				t.Fatal("mutation missed")
			}
			v[tc.file] = changed
			if rustReleaseEvidenceBoundary(v[0], v[1], v[2], v[3]) == nil {
				t.Fatal("mutation accepted")
			}
		})
	}
}
