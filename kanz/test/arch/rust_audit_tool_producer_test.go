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
	if strings.Contains(script, `"docker", "export"`) || strings.Contains(script, `"docker", "create"`) {
		return fmt.Errorf("inventory must read image layers, not runtime containers")
	}
	readback, sign, verify := strings.Index(script, `extract_saved(read_bounded(readback, MAX_PAYLOAD), image, check, commit)`), strings.Index(script, `run(["cosign", "sign"`), strings.Index(script, `output(["cosign", "verify"`)
	if readback < 0 || sign <= readback || verify <= sign {
		return fmt.Errorf("keyless signing must follow verified registry bytes")
	}
	main := script[strings.Index(script, "def main():"):]
	roundtrip, audit := strings.Index(main, "load_and_check_saved(image_bytes, image, files, temp, commit)"), strings.Index(main, "advisories = exercise(temp)")
	if roundtrip < 0 || audit <= roundtrip {
		return fmt.Errorf("read-only roundtrip must precede actual audit qualification")
	}
	for _, token := range []string{
		`"--provenance=false", "--sbom=false", "--metadata-file", str(build_metadata)`,
		`run(build_command(archive, build_metadata, commit), input=recipe)`,
		`compression=uncompressed,force-compression=true,oci-mediatypes=true,name=`,
		`local_tag(commit), "-"]`,
		`md["digest"] != expected_root`,
		`validate_build_metadata(metadata_bytes, graph_descriptor, image)`,
		`load_and_check_saved(image_bytes, image, files, temp, commit)`,
		`extract_saved(read_bounded(saved, MAX_PAYLOAD), image, temp, commit) != (image, files)`,
		`set(entries) != {"oci-layout", "index.json", name, config_name, layer_name}`,
		`annotations.get("io.containerd.image.name") != "docker.io/library/" + local_tag(commit)`,
		`m.get("RepoTags") != [local_tag(commit)]`,
		`os.environ.get("GITHUB_REF") != "refs/heads/main"`,
		`proof["image_tar_sha256"] != hashlib.sha256(image_bytes).hexdigest()`,
		`registry_manifest(prior, image)`,
		`registry_manifest(registry, image)`,
		`extract_saved(read_bounded(readback, MAX_PAYLOAD), image, check, commit) != (image, files)`,
		`"--platform", "linux/amd64"`,
		`config["config"]["Labels"].get("org.opencontainers.image.revision") != commit`,
		`if actual != metadata:`,
		`config["rootfs"] != {"type": "layers", "diff_ids": [diff]}`,
		`len(m["Layers"]) != 1`,
		`"sha256:" + hashlib.sha256(layer).hexdigest()`,
		`run(["cosign", "sign", "--yes", "-a", "source_sha=" + commit, pinned])`,
		`"--certificate-oidc-issuer", ISSUER, "--certificate-identity", SIGNER`,
		`SIGNER = "https://github.com/eighred/kanz/.github/workflows/base-image-mirror.yml@refs/heads/main"`,
		`ISSUER = "https://token.actions.githubusercontent.com"`,
		`"--deny", "warnings", "--json", "--file"`,
		`"Linux no-follow/nonblocking file reader required"`,
	} {
		if !strings.Contains(script, token) {
			return fmt.Errorf("producer control missing: %s", token)
		}
	}
	for _, token := range []string{`"--iidfile"`, `diagnose(probe`, `"--load"`, `"--mount"`, `"--secret"`, `"--cache-to"`, `"--no-fetch"`, `"--offline"`} {
		if strings.Contains(script, token) {
			return fmt.Errorf("producer context/secret/cache/audit bypass")
		}
	}
	type step struct {
		Run      string            `yaml:"run"`
		Continue bool              `yaml:"continue-on-error"`
		If       string            `yaml:"if"`
		Uses     string            `yaml:"uses"`
		With     map[string]string `yaml:"with"`
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
	if p.If != "github.ref == 'refs/heads/main' && (github.event_name == 'push' || github.event_name == 'workflow_dispatch') && !inputs.dry_run && github.repository == 'eighred/kanz'" || p.Needs != "rust-audit-tools" || p.Permissions["packages"] != "write" || p.Permissions["id-token"] != "write" || len(p.Permissions) != 3 {
		return fmt.Errorf("publication must follow qualification on main")
	}
	setup := 0
	for _, step := range p.Steps {
		if step.Uses == "sigstore/cosign-installer@756fe2d284c7292c4e9b62c8a28f1e5629c59532" && !step.Continue && step.If == "" && step.With["cosign-release"] == "v3.0.6" {
			setup++
		}
	}
	if setup != 1 {
		return fmt.Errorf("publisher must install pinned signing tool")
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
		{"verifier-version", 2, "cosign-release: v3.0.6", "cosign-release: v3.1.4"},
		{"source-copy", 0, "FROM scratch", "COPY kanz/ /private/\nFROM scratch"},
		{"version-change", 0, "--version 0.22.2", "--version 0.22.1"},
		{"source-stamp", 0, "org.opencontainers.image.revision=$SOURCE_SHA", "org.opencontainers.image.revision=unknown"},
		{"build-identity", 1, `validate_build_metadata(metadata_bytes, graph_descriptor, image)`, `pass`},
		{"omit-roundtrip", 1, `            load_and_check_saved(image_bytes, image, files, temp, commit)`, `            pass`},
		{"build-root", 1, `md["digest"] != expected_root`, `False`},
		{"attestation", 1, `"--provenance=false"`, `"--provenance=true"`},
		{"saved-tag", 1, `m.get("RepoTags") != [local_tag(commit)]`, `False`},
		{"repo-context", 1, `local_tag(commit), "-"]`, `local_tag(commit), "."]`},
		{"mount-source", 1, `"--platform", "linux/amd64"`, `"--mount", "source=.,target=/src"`},
		{"layer-hash", 1, `"sha256:" + hashlib.sha256(layer).hexdigest()`, `"sha256:" + "0" * 64`},
		{"multiple-layers", 1, `len(m["Layers"]) != 1`, `False`},
		{"early-sign", 1, `readback = check / "image.tar"`, `run(["cosign", "sign", "--yes", "-a", "source_sha=" + commit, pinned]); readback = check / "image.tar"`},
		{"unsigned", 1, `run(["cosign", "sign", "--yes", "-a", "source_sha=" + commit, pinned])`, `pass`},
		{"digest-readback", 1, `registry_manifest(registry, image)`, `pass`},
		{"existing-tag", 1, `registry_manifest(prior, image)`, `pass`},
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
