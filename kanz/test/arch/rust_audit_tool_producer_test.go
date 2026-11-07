package arch

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Reviewed producer recipe and host-command boundary; actual Linux qualification
// proves tool ABI/audit behavior. This is not a general shell/Python analyzer.
func auditToolProducerBoundary(recipe, script, workflow string) error {
	if fmt.Sprintf("%x", sha256.Sum256([]byte(strings.ReplaceAll(recipe, "\r\n", "\n")))) != "5b6c36ac1369b2ba5d70a6c4144bb7fcde5af4e089836ccbf1e507dcaadbebd4" {
		return fmt.Errorf("unreviewed source-free recipe")
	}
	if strings.Count(script, `"--platform", "linux/amd64"`) != 2 {
		return fmt.Errorf("both build and readback must select exact platform")
	}
	for _, token := range []string{
		`"--iidfile", str(iid), "-"], input=recipe)`,
		`os.environ.get("GITHUB_REF") != "refs/heads/main"`,
		`proof["image_tar_sha256"] != hashlib.sha256(image_bytes).hexdigest()`,
		`registry.get("config", {}).get("digest") != image`,
		`extract(pinned, check, commit) != (image, files)`,
		`"--platform", "linux/amd64"`,
		`info["Config"]["Labels"].get("org.opencontainers.image.revision") != commit`,
		`if actual != metadata:`,
		`"--deny", "warnings", "--json", "--file"`,
		`"Linux no-follow/nonblocking file reader required"`,
	} {
		if !strings.Contains(script, token) {
			return fmt.Errorf("producer control missing: %s", token)
		}
	}
	for _, token := range []string{`"--mount"`, `"--secret"`, `"--cache-to"`, `"--no-fetch"`, `"--offline"`} {
		if strings.Contains(script, token) {
			return fmt.Errorf("producer context/secret/cache/audit bypass")
		}
	}
	type step struct {
		Run      string `yaml:"run"`
		Continue bool   `yaml:"continue-on-error"`
		If       string `yaml:"if"`
		Uses     string `yaml:"uses"`
	}
	type job struct {
		If          string            `yaml:"if"`
		Needs       string            `yaml:"needs"`
		Permissions map[string]string `yaml:"permissions"`
		Steps       []step            `yaml:"steps"`
	}
	var w struct {
		Permissions map[string]string `yaml:"permissions"`
		Jobs        map[string]job    `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(workflow), &w); err != nil {
		return err
	}
	if w.Permissions["packages"] != "read" {
		return fmt.Errorf("workflow must default read-only")
	}
	q, p, m := w.Jobs["rust-audit-tools"], w.Jobs["publish-rust-audit-tools"], w.Jobs["mirror"]
	if q.Permissions["contents"] != "read" || q.Permissions["packages"] != "read" || len(q.Permissions) != 2 {
		return fmt.Errorf("PR qualifier cannot publish")
	}
	if p.If != "github.ref == 'refs/heads/main' && (github.event_name == 'push' || github.event_name == 'workflow_dispatch') && !inputs.dry_run" || p.Needs != "rust-audit-tools" || p.Permissions["packages"] != "write" {
		return fmt.Errorf("publication must follow qualification on main")
	}
	if m.If != "github.ref == 'refs/heads/main' && github.event_name != 'pull_request'" {
		return fmt.Errorf("legacy mirror cannot publish from PR")
	}
	for _, owner := range []struct {
		j    job
		mode string
	}{{q, "build"}, {p, "publish"}} {
		count := 0
		for _, s := range owner.j.Steps {
			if strings.Contains(s.Run, "qualify.py "+owner.mode) {
				if s.Run != `python3 -B tools/rust-audit-tools/qualify.py `+owner.mode+` "$RUNNER_TEMP/rust-audit-tools"` || s.Continue || s.If != "" {
					return fmt.Errorf("tool qualification cannot be skipped")
				}
				count++
			}
		}
		if count != 1 {
			return fmt.Errorf("producer command inventory")
		}
	}
	return nil
}

func TestRustAuditToolProducerBoundary(t *testing.T) {
	root := filepath.Dir(moduleRoot(t))
	paths := []string{"tools/rust-audit-tools/Dockerfile", "tools/rust-audit-tools/qualify.py", ".github/workflows/base-image-mirror.yml"}
	var text [3]string
	for i, p := range paths {
		b, err := os.ReadFile(filepath.Join(root, p))
		if err != nil {
			t.Fatal(err)
		}
		text[i] = string(b)
	}
	if err := auditToolProducerBoundary(text[0], text[1], text[2]); err != nil {
		t.Fatal(err)
	}
	for _, m := range []struct {
		name     string
		file     int
		old, new string
	}{
		{"source-copy", 0, "FROM scratch", "COPY kanz/ /private/\nFROM scratch"},
		{"version-change", 0, "--version 0.22.2", "--version 0.22.1"},
		{"source-stamp", 0, "org.opencontainers.image.revision=$SOURCE_SHA", "org.opencontainers.image.revision=unknown"},
		{"repo-context", 1, `"--iidfile", str(iid), "-"`, `"--iidfile", str(iid), "."`},
		{"mount-source", 1, `"--platform", "linux/amd64"`, `"--mount", "source=.,target=/src"`},
		{"digest-readback", 1, `registry.get("config", {}).get("digest") != image`, `False`},
		{"platform-check", 1, `"--platform", "linux/amd64"`, `"--platform", "linux/arm64"`},
		{"PR-write", 2, "packages: read", "packages: write"},
		{"publish-before-qualify", 2, "needs: rust-audit-tools", "needs: mirror"},
		{"publish-PR", 2, "github.ref == 'refs/heads/main' && (github.event_name", "(github.event_name"},
	} {
		t.Run(m.name, func(t *testing.T) {
			changed := text
			changed[m.file] = strings.Replace(changed[m.file], m.old, m.new, 1)
			if changed[m.file] == text[m.file] {
				t.Fatal("mutation did not apply")
			}
			if auditToolProducerBoundary(changed[0], changed[1], changed[2]) == nil {
				t.Fatal("mutation escaped")
			}
		})
	}
}
