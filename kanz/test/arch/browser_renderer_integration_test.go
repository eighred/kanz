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

func TestBrowserRendererConnectedBoundary(t *testing.T) {
	root := filepath.Dir(moduleRoot(t))
	read := func(name string) string {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		return strings.ReplaceAll(string(raw), "\r\n", "\n")
	}
	pins := map[string]string{
		"web/src/api/browser-renderer-loader.test.ts":                    "eb3ccde08e576fb7d6124b217da15d48c99678f3df63f176a3b67ae1fd1e01d9",
		"web/src/api/browser-renderer.ts":                                "a9a5d220f406b0c5bb97297bdc6c117c8ae8a98e62792d705dffb949d43ca4cf",
		"web/src/api/browser-renderer-loader.ts":                         "32c98a41116e93b2e177f9c6e1f6486a0686183af25872b9bd673f42853cc613",
		"web/src/api/browser-renderer.test.ts":                           "434caa03b8b8e94a3c2fa931a73be6f3155e586945a6a3fac767bf163ee363f8",
		"web/e2e/browser-renderer.cjs":                                   "ed5e640597a250521913bf7749860ac8c3d255faba50e1ed1419c85afa0a02c7",
		"kanz/services/web-bff/internal/server/browser_renderer_test.go": "d8c94c839e404f71c8e08270e4580a192a16aa9696a993e2d9d0ce4012b1828c",
		".github/scripts/check_browser_renderer_evidence.py":             "86e289b4989ac83dfa5b2ba3678c73c6f3fe3c6818e806b35c09d3febfb1d765",
		".github/scripts/test_check_browser_renderer_evidence.py":        "b79a11e941b004a1d50c5d2d5757312567b2f18b0ac4384790807ae3b3ba101f",
	}
	for name, want := range pins {
		if fmt.Sprintf("%x", sha256.Sum256([]byte(read(name)))) != want {
			t.Fatalf("unreviewed browser integration boundary %s", name)
		}
	}
	decimal := read("web/src/api/decimal.ts")
	decimal = strings.Replace(decimal, "import { renderBrowserValue } from './browser-renderer'\n\n", "", 1)
	decimal = strings.Replace(decimal, "\n  const rendered = renderBrowserValue(1, raw, exponent)\n  if (rendered.handled) return rendered.value\n", "", 1)
	decimal = strings.Replace(decimal, "\n  const rendered = renderBrowserValue(2, s)\n  if (rendered.handled) return rendered.value\n", "", 1)
	if fmt.Sprintf("%x", sha256.Sum256([]byte(decimal))) != "a72c54727aafd9b6e9325f5631a9f1a0235d5cc66cb70d93c9f21003bdd9441b" {
		t.Fatal("legacy rendering implementation changed")
	}
	main := read("web/src/main.ts")
	main = strings.Replace(main, "import { initializeBrowserRenderer } from './api/browser-renderer-loader'\n", "", 1)
	main = strings.Replace(main, "app.mount('#app')\nvoid initializeBrowserRenderer()", "app.mount('#app')", 1)
	if fmt.Sprintf("%x", sha256.Sum256([]byte(main))) != "3686e8122eb67d16267a89a7a7ca574a18cee67bbb065afc8141b13b654904f8" {
		t.Fatal("browser auth/mount flow changed")
	}
	csp := read("kanz/services/web-bff/internal/server/browser_security.go")
	csp = strings.Replace(csp, "script-src 'self' 'wasm-unsafe-eval';", "script-src 'self';", 1)
	if fmt.Sprintf("%x", sha256.Sum256([]byte(csp))) != "dea397383bfc9b2334edc9a51b4da28f4e2d3be8f36f288ed2d033ea2e410cde" {
		t.Fatal("CSP/security boundary changed beyond WASM compilation")
	}
	for _, name := range []string{"kanz-ci.yml", "build.yml", "release.yml"} {
		raw := read(".github/workflows/" + name)
		if !browserRendererCI(raw, name) {
			t.Fatalf("browser build/actual qualification missing in %s", name)
		}
		for _, remove := range []string{"rustup toolchain install 1.90.0 --profile minimal --target wasm32-unknown-unknown"} {
			if browserRendererCI(strings.ReplaceAll(raw, remove, "true"), name) {
				t.Fatal("missing pinned compiler accepted")
			}
		}
		if name == "kanz-ci.yml" {
			for _, mutation := range [][2]string{{"  go-race:\n", "  go-race:\n    if: false\n"}, {"set -o pipefail\n          go test -p 1 ./services/web-bff/internal/server -run '^TestBrowserRendererProductionBundle$'", "true\n          go test -p 1 ./services/web-bff/internal/server -run '^TestBrowserRendererProductionBundle$'"}, {"name: Preserve actual browser renderer evidence", "name: Removed browser renderer evidence"}, {"name: browser-renderer-${{ github.run_id }}-${{ github.run_attempt }}-${{ github.sha }}", "name: browser-renderer-stale"}, {"-run '^TestBrowserRendererProductionBundle$'", "-run '^Absent$'"}, {"if: matrix.kind == 'quality'\n        working-directory: kanz\n        env:\n          TEST_RENDER_BROWSER_SCRIPT", "if: false\n        working-directory: kanz\n        env:\n          TEST_RENDER_BROWSER_SCRIPT"}, {"check_browser_renderer_evidence.py ", "absent.py "}, {"name: Qualify WASM rendering through real BFF and Chromium", "name: Qualify WASM rendering through real BFF and Chromium\n        continue-on-error: true"}} {
				if browserRendererCI(strings.Replace(raw, mutation[0], mutation[1], 1), name) {
					t.Fatal("browser proof bypass accepted")
				}
			}
		}
	}
}

type rendererStep struct {
	Name, Run, If, Uses string
	Continue            any    `yaml:"continue-on-error"`
	Directory           string `yaml:"working-directory"`
	Env                 map[string]string
	With                map[string]any
}

func browserRendererCI(raw, name string) bool {
	var w struct {
		Jobs map[string]struct {
			If       string
			Steps    []rendererStep
			Continue any `yaml:"continue-on-error"`
		}
	}
	if yaml.Unmarshal([]byte(raw), &w) != nil {
		return false
	}
	const setup = "rustup toolchain install 1.90.0 --profile minimal --target wasm32-unknown-unknown"
	if name == "kanz-ci.yml" {
		goJob, ok := w.Jobs["go-race"]
		if !ok || goJob.Continue != nil || goJob.If != "needs.scope.outputs.application == 'true'" {
			return false
		}
		install, build, proof, artifact := -1, -1, -1, -1
		names := map[string]bool{}
		for i, s := range goJob.Steps {
			if s.Name != "" && names[s.Name] {
				return false
			}
			names[s.Name] = true
			if s.Name == "Install pinned Rust WASM target for browser rendering" && s.Run == setup && s.If == "" && s.Continue == nil {
				install = i
			}
			if s.Name == "Build the real MFA browser boundary" && strings.Contains(s.Run, "npm run build") {
				build = i
			}
			if s.Name == "Qualify WASM rendering through real BFF and Chromium" {
				if s.If != "matrix.kind == 'quality'" || s.Continue != nil || s.Directory != "kanz" || s.Env["TEST_RENDER_BROWSER_SCRIPT"] != "${{ github.workspace }}/web/e2e/browser-renderer.cjs" || len(s.Env) != 4 || s.Env["TEST_MFA_STATIC_DIR"] != "${{ github.workspace }}/web/dist" || s.Env["TEST_MFA_PLAYWRIGHT"] != "${{ github.workspace }}/web/node_modules/playwright" || s.Env["TEST_MFA_NODE"] != "node" {
					return false
				}
				const proofRun = `set -o pipefail
go test -p 1 ./services/web-bff/internal/server -run '^TestBrowserRendererProductionBundle$' -count=1 -json | tee "$RUNNER_TEMP/browser-renderer-proof.jsonl"
python3 -B -m unittest discover -s ../.github/scripts -p test_check_browser_renderer_evidence.py
python3 ../.github/scripts/check_browser_renderer_evidence.py "$RUNNER_TEMP/browser-renderer-proof.jsonl" "$RUNNER_TEMP/browser-renderer-proof.json"`
				if strings.TrimSpace(s.Run) != proofRun {
					return false
				}
				proof = i
			}
			if s.Name == "Preserve actual browser renderer evidence" {
				if s.If != "always() && matrix.kind == 'quality'" || s.Continue != nil || s.Uses != "actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9" || len(s.With) != 4 || s.With["name"] != "browser-renderer-${{ github.run_id }}-${{ github.run_attempt }}-${{ github.sha }}" || s.With["if-no-files-found"] != "error" || s.With["retention-days"] != 7 || s.With["path"] != "${{ runner.temp }}/browser-renderer-proof.jsonl\n${{ runner.temp }}/browser-renderer-proof.json\nweb/src/generated/browser-render.ts\nweb/dist/assets/*.wasm\n" {
					return false
				}
				artifact = i
			}
		}
		if !(install >= 0 && build > install && proof > build && artifact > proof) {
			return false
		}
		web, ok := w.Jobs["web"]
		if !ok || web.If != "needs.scope.outputs.application == 'true'" || web.Continue != nil {
			return false
		}
		install, prepare, check := -1, -1, -1
		for i, s := range web.Steps {
			if s.Name == "Install pinned Rust WASM target for browser rendering" && s.Run == setup && s.If == "" && s.Continue == nil {
				install = i
			}
			if s.Name == "Prepare checked browser rendering asset" && s.Run == "npm run prebuild" && s.Directory == "web" && s.If == "" && s.Continue == nil {
				prepare = i
			}
			if s.Name == "Typecheck" {
				check = i
			}
		}
		return install >= 0 && prepare > install && check > prepare
	}
	for _, job := range w.Jobs {
		if job.Continue != nil {
			return false
		}
		install, build := -1, -1
		for i, s := range job.Steps {
			if s.Name == "Install pinned Rust WASM target for BFF asset" && s.Run == setup && s.If == "matrix.service == 'web-bff'" && s.Continue == nil {
				install = i
			}
			if s.Name == "Build the web client" && s.If == "matrix.service == 'web-bff'" && strings.Contains(s.Run, "npm run build") {
				build = i
			}
		}
		if install >= 0 && build > install {
			return true
		}
	}
	return false
}
