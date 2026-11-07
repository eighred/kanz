package arch

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const browserRenderPath = "crates/browser-render"
const browserRenderManifest = `[package]
name = "kanz-browser-render"
version = "0.1.0"
edition = "2021"
rust-version = "1.90"
publish = false
[lib]
crate-type = ["cdylib", "rlib"]`

// Closed executable inventory: no build script, dependencies, host imports,
// alternate source or frontend activation can enter this reviewed local kernel.
// Digests normalize only checkout line endings; they do not prove arithmetic.
var browserRenderSources = map[string]string{
	"tests/build-wasm.sh": "c44d5e22f1341f91fe3031ed8dcf0d0237b9eb88f7b55ea5292768263f678546",
	"src/lib.rs":          "48a1c581f891ab3df8e32d8b0218427f60ca2617714953792e504474ec0e2e37",
	"tests/kernel.rs":     "dea7cd95e81117924dfb753af7d6d7b25d5835055141d4ba87b265ca95915ae7",
	"tests/oracle.ts":     "a72c54727aafd9b6e9325f5631a9f1a0235d5cc66cb70d93c9f21003bdd9441b",
	"tests/wasm.mjs":      "1730e66681d641981081629d2e7b5ce6db56a914076cf6060e74672831ede4eb",
}

func checkBrowserRender(root string) error {
	root = filepath.Join(root, browserRenderPath)
	manifest, err := os.ReadFile(filepath.Join(root, "Cargo.toml"))
	if err != nil || rustMigrationCompact(string(manifest)) != rustMigrationCompact(browserRenderManifest) {
		return fmt.Errorf("browser renderer manifest/dependencies changed")
	}
	seen := map[string]bool{}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("browser renderer symlink")
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "Cargo.toml" || rel == "README.md" {
			return nil
		}
		expected, ok := browserRenderSources[rel]
		if !ok {
			return fmt.Errorf("unreviewed browser renderer file %s", rel)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		actual := fmt.Sprintf("%x", sha256.Sum256([]byte(strings.ReplaceAll(string(raw), "\r\n", "\n"))))
		if actual != expected {
			return fmt.Errorf("browser renderer source changed: %s", rel)
		}
		seen[rel] = true
		return nil
	})
	if err != nil {
		return err
	}
	if len(seen) != len(browserRenderSources) {
		return fmt.Errorf("browser renderer source missing")
	}
	return nil
}
func TestRustBrowserRenderBoundary(t *testing.T) {
	root := filepath.Join(filepath.Dir(moduleRoot(t)), "kanz-rs")
	if err := checkBrowserRender(root); err != nil {
		t.Fatal(err)
	}
	goSource, err := os.ReadFile(filepath.Join(moduleRoot(t), "internal/dec/dec.go"))
	if err != nil {
		t.Fatal(err)
	}
	rustSource, err := os.ReadFile(filepath.Join(root, browserRenderPath, "src/lib.rs"))
	if err != nil {
		t.Fatal(err)
	}
	goBound := decBoundPattern.FindSubmatch(goSource)
	rustBound := regexp.MustCompile(`pub const MAX_EXPONENT: i32 = ([0-9]+);`).FindSubmatch(rustSource)
	if len(goBound) != 2 || len(rustBound) != 2 || string(goBound[1]) != string(rustBound[1]) {
		t.Fatal("browser renderer Decimal bound differs from canonical Go owner")
	}

	for _, mutation := range []struct{ path, old, value string }{
		{"Cargo.toml", "[lib]", "[dependencies]\nlibc = \"0.2\"\n[lib]"},
		{"src/lib.rs", "MAX_NUMERAL: usize = 128", "MAX_NUMERAL: usize = 129"},
		{"src/lib.rs", "s.poisoned = true;", "s.poisoned = false;"},
		{"src/lib.rs", "s.output = Rendered::empty();", ""},
		{"src/lib.rs", "pub const OK:", "pub fn net() { std::net::TcpStream::connect(\"localhost:1\").unwrap(); }\npub const OK:"},
		{"tests/oracle.ts", "MAX_SAFE_EXPONENT = 64", "MAX_SAFE_EXPONENT = 65"},
		{"tests/wasm.mjs", "assert.deepEqual(WebAssembly.Module.imports(module), [])", ""},
	} {
		dst := t.TempDir()
		for _, name := range append([]string{"Cargo.toml"}, browserRenderFileNames()...) {
			raw, err := os.ReadFile(filepath.Join(root, browserRenderPath, name))
			if err != nil {
				t.Fatal(err)
			}
			if name == mutation.path {
				old := string(raw)
				raw = []byte(strings.Replace(old, mutation.old, mutation.value, 1))
				if string(raw) == old {
					t.Fatal("absent mutation", name)
				}
			}
			p := filepath.Join(dst, browserRenderPath, name)
			if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, raw, 0600); err != nil {
				t.Fatal(err)
			}
		}
		if checkBrowserRender(dst) == nil {
			t.Fatal("accepted weakened renderer", mutation.path)
		}
	}
}
func browserRenderFileNames() []string {
	var out []string
	for name := range browserRenderSources {
		out = append(out, name)
	}
	return out
}

func browserRenderWorkflow(raw []byte) bool {
	var workflow struct {
		Jobs map[string]struct {
			Continue any    `yaml:"continue-on-error"`
			If       string `yaml:"if"`
			Steps    []struct {
				Name      string            `yaml:"name"`
				Uses      string            `yaml:"uses"`
				If        string            `yaml:"if"`
				Continue  any               `yaml:"continue-on-error"`
				Directory string            `yaml:"working-directory"`
				Run       string            `yaml:"run"`
				With      map[string]string `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if yaml.Unmarshal(raw, &workflow) != nil {
		return false
	}
	job, ok := workflow.Jobs["compatibility"]
	if !ok || job.Continue != nil || job.If != "" {
		return false
	}
	var names []string
	for _, step := range job.Steps {
		switch step.Name {
		case "Select pinned Node for actual WASM qualification":
			if step.Uses != "actions/setup-node@949feb2413d6458794dcd2491c4babbbce0c15c1" || step.With["node-version-file"] != "kanz-web/.nvmrc" {
				return false
			}
		case "Qualify actual browser rendering WASM":
			if step.Directory != "kanz-rs" || strings.TrimSpace(step.Run) != "set -euo pipefail\ncargo +1.90.0 test --locked --release -p kanz-browser-render\nrustup target add wasm32-unknown-unknown --toolchain 1.90.0\nbash crates/browser-render/tests/build-wasm.sh | tee \"$RUNNER_TEMP/browser-render.json\"" {
				return false
			}
		case "Preserve actual Node WASM evidence":
			if step.Uses != "actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9" || step.With["name"] != "browser-render-wasm" || step.With["if-no-files-found"] != "error" || strings.TrimSpace(step.With["path"]) != "${{ runner.temp }}/browser-render.json\nkanz-rs/target/wasm32-unknown-unknown/release/kanz_browser_render.wasm" {
				return false
			}
		default:
			continue
		}
		if step.If != "" || step.Continue != nil {
			return false
		}
		names = append(names, step.Name)
	}
	return strings.Join(names, "|") == "Select pinned Node for actual WASM qualification|Qualify actual browser rendering WASM|Preserve actual Node WASM evidence"
}
func TestRustBrowserWASMQualificationRequired(t *testing.T) {
	path := filepath.Join(filepath.Dir(moduleRoot(t)), ".github/workflows/architecture-migration.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !browserRenderWorkflow(raw) {
		t.Fatal("actual WASM qualification missing")
	}
	for _, m := range [][2]string{
		{"  compatibility:\n", "  compatibility:\n    if: false\n"},
		{"cargo +1.90.0 test --locked --release -p kanz-browser-render", "true"},
		{"rustup target add wasm32-unknown-unknown --toolchain 1.90.0", "true"},
		{"bash crates/browser-render/tests/build-wasm.sh", "echo proof"},
		{"name: Qualify actual browser rendering WASM", "name: Qualify actual browser rendering WASM\n        if: false"},
		{"name: Qualify actual browser rendering WASM", "name: Qualify actual browser rendering WASM\n        continue-on-error: true"},
		{"name: browser-render-wasm", "name: unrelated"},
		{"node-version-file: kanz-web/.nvmrc", "node-version: latest"},
	} {
		changed := strings.Replace(string(raw), m[0], m[1], 1)
		if changed == string(raw) {
			t.Fatal("missing workflow mutation")
		}
		if browserRenderWorkflow([]byte(changed)) {
			t.Fatal("accepted missing WASM proof", m[0])
		}
	}
}
