package arch

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Native TS7 checks the real Vite configuration, not Vue's embedded language.
// The original full Vue/TS graph remains mandatory through vue-tsc's TS6 API.
func checkWebDualCompilerRecipe(files map[string]string) error {
	var pkg struct {
		Scripts         map[string]string `json:"scripts"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if err := json.Unmarshal([]byte(files["web/package.json"]), &pkg); err != nil {
		return fmt.Errorf("web compiler package: %w", err)
	}
	for name, expected := range map[string]string{
		"typescript":         "npm:@typescript/typescript6@6.0.2",
		"@typescript/native": "npm:typescript@7.0.2",
		"vue-tsc":            "3.3.14",
	} {
		if pkg.DevDependencies[name] != expected {
			return fmt.Errorf("web compiler dependency %s must retain the reviewed official alias/version", name)
		}
	}
	for name, expected := range map[string]string{
		"prebuild":         "node scripts/build-renderer.mjs",
		"build":            "npm run typecheck && vite build",
		"typecheck":        "npm run typecheck:vue && npm run typecheck:native",
		"typecheck:vue":    "vue-tsc --noEmit",
		"typecheck:native": "node ./node_modules/@typescript/native/bin/tsc --noEmit --project tsconfig.native.json",
		"test":             "vitest run",
	} {
		if pkg.Scripts[name] != expected {
			return fmt.Errorf("mandatory web compiler/build script %s changed", name)
		}
	}
	// Compare parsed data, so formatting is free but removing a source family,
	// adding an exclusion or overriding a strict option cannot manufacture green.
	for _, config := range []struct{ path, expected string }{
		{"web/tsconfig.json", `{
  "compilerOptions": {
    "target": "ES2022", "module": "ESNext", "moduleResolution": "bundler",
    "strict": true, "noUncheckedIndexedAccess": true,
    "noUnusedLocals": true, "noUnusedParameters": true,
    "jsx": "preserve", "lib": ["ES2022", "DOM", "DOM.Iterable"],
    "skipLibCheck": true, "isolatedModules": true,
    "verbatimModuleSyntax": true, "noEmit": true
  },
  "include": ["src/**/*.ts", "src/**/*.vue", "vite.config.ts"]
}`},
		{"web/tsconfig.native.json", `{
  "extends": "./tsconfig.json",
  "compilerOptions": {"types": ["node"]},
  "include": ["vite.config.ts"]
}`},
	} {
		var actual, expected any
		if err := json.Unmarshal([]byte(files[config.path]), &actual); err != nil {
			return fmt.Errorf("web compiler config %s: %w", config.path, err)
		}
		if err := json.Unmarshal([]byte(config.expected), &expected); err != nil {
			return err
		}
		if !reflect.DeepEqual(actual, expected) {
			return fmt.Errorf("web compiler source/strict-option boundary changed in %s", config.path)
		}
	}
	var workflow struct {
		Jobs map[string]struct {
			If       string
			Continue any `yaml:"continue-on-error"`
			Steps    []rendererStep
		}
	}
	if err := yaml.Unmarshal([]byte(files[".github/workflows/kanz-ci.yml"]), &workflow); err != nil {
		return fmt.Errorf("web compiler CI: %w", err)
	}
	job, ok := workflow.Jobs["web"]
	if !ok || job.If != "needs.scope.outputs.application == 'true'" || job.Continue != nil {
		return fmt.Errorf("mandatory web compiler CI job is absent or bypassed")
	}
	expectedSteps := map[string]string{"Typecheck": "npm run typecheck", "Test": "npm test", "Build": "npm run build"}
	seen := map[string]bool{}
	positions := map[string]int{}
	for index, step := range job.Steps {
		expected, relevant := expectedSteps[step.Name]
		if !relevant {
			continue
		}
		if seen[step.Name] || step.If != "" || step.Continue != nil || step.Directory != "web" || strings.TrimSpace(step.Run) != expected {
			return fmt.Errorf("mandatory web compiler CI step %s is changed or bypassed", step.Name)
		}
		seen[step.Name] = true
		positions[step.Name] = index
	}
	for name := range expectedSteps {
		if !seen[name] {
			return fmt.Errorf("mandatory web compiler CI step %s is absent", name)
		}
	}
	if positions["Typecheck"] >= positions["Test"] || positions["Test"] >= positions["Build"] {
		return fmt.Errorf("web compiler checks must precede tests and the bundle")
	}
	return nil
}

func TestWebDualCompilerRetainsFullVueAndNativeViteChecks(t *testing.T) {
	root := filepath.Dir(moduleRoot(t))
	files := map[string]string{}
	for _, path := range []string{"web/package.json", "web/tsconfig.json", "web/tsconfig.native.json", ".github/workflows/kanz-ci.yml"} {
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			t.Fatal(err)
		}
		files[path] = strings.ReplaceAll(string(raw), "\r\n", "\n")
	}
	if err := checkWebDualCompilerRecipe(files); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []struct{ name, path, before, after, diagnostic string }{
		{"missing Vue check", "web/package.json", "npm run typecheck:vue && npm run typecheck:native", "npm run typecheck:native", "script typecheck changed"},
		{"missing native check", "web/package.json", "npm run typecheck:vue && npm run typecheck:native", "npm run typecheck:vue", "script typecheck changed"},
		{"failure hidden by alternative", "web/package.json", "npm run typecheck:vue && npm run typecheck:native", "npm run typecheck:vue || npm run typecheck:native", "script typecheck changed"},
		{"Vue check made decorative", "web/package.json", "vue-tsc --noEmit", "vue-tsc --version", "script typecheck:vue changed"},
		{"native check made decorative", "web/package.json", "--noEmit --project tsconfig.native.json", "--version", "script typecheck:native changed"},
		{"native resolves JS compiler", "web/package.json", "node ./node_modules/@typescript/native/bin/tsc", "node ./node_modules/typescript/bin/tsc", "script typecheck:native changed"},
		{"native uses unsupported Vue project", "web/package.json", "--project tsconfig.native.json", "--project tsconfig.json", "script typecheck:native changed"},
		{"bundle bypasses both checks", "web/package.json", "npm run typecheck && vite build", "vite build", "script build changed"},
		{"renderer prebuild removed", "web/package.json", "\"prebuild\": \"node scripts/build-renderer.mjs\"", "\"prebuild\": \"true\"", "script prebuild changed"},
		{"tests removed", "web/package.json", "\"test\": \"vitest run\"", "\"test\": \"true\"", "script test changed"},
		{"TS6 alias lost", "web/package.json", "npm:@typescript/typescript6@6.0.2", "7.0.2", "dependency typescript"},
		{"native alias lost", "web/package.json", "npm:typescript@7.0.2", "npm:@typescript/typescript6@6.0.2", "dependency @typescript/native"},
		{"shim resolver downgraded", "web/package.json", "\"vue-tsc\": \"3.3.14\"", "\"vue-tsc\": \"3.0.0\"", "dependency vue-tsc"},
		{"Vue family excluded", "web/tsconfig.json", "src/**/*.vue", "src/**/*.not-vue", "boundary changed in web/tsconfig.json"},
		{"TS family excluded", "web/tsconfig.json", "src/**/*.ts", "src/**/*.not-ts", "boundary changed in web/tsconfig.json"},
		{"strict disabled", "web/tsconfig.json", "\"strict\": true", "\"strict\": false", "boundary changed in web/tsconfig.json"},
		{"indexed access widened", "web/tsconfig.json", "\"noUncheckedIndexedAccess\": true", "\"noUncheckedIndexedAccess\": false", "boundary changed in web/tsconfig.json"},
		{"unused locals widened", "web/tsconfig.json", "\"noUnusedLocals\": true", "\"noUnusedLocals\": false", "boundary changed in web/tsconfig.json"},
		{"unused parameters widened", "web/tsconfig.json", "\"noUnusedParameters\": true", "\"noUnusedParameters\": false", "boundary changed in web/tsconfig.json"},
		{"new Vue exclusion", "web/tsconfig.json", "\"include\": [", "\"exclude\": [\"src/views/**\"], \"include\": [", "boundary changed in web/tsconfig.json"},
		{"native inherits another profile", "web/tsconfig.native.json", "./tsconfig.json", "./absent.json", "boundary changed in web/tsconfig.native.json"},
		{"native omits actual Vite config", "web/tsconfig.native.json", "vite.config.ts", "src/absent.ts", "boundary changed in web/tsconfig.native.json"},
		{"native checks whole Vue graph", "web/tsconfig.native.json", "vite.config.ts", "src/**/*.vue", "boundary changed in web/tsconfig.native.json"},
		{"native strict override", "web/tsconfig.native.json", "\"types\": [", "\"strict\": false, \"types\": [", "boundary changed in web/tsconfig.native.json"},
		{"native Node types omitted", "web/tsconfig.native.json", "\"node\"", "\"absent\"", "boundary changed in web/tsconfig.native.json"},
		{"CI compiler command omitted", ".github/workflows/kanz-ci.yml", "run: npm run typecheck", "run: true", "CI step Typecheck"},
		{"CI compiler failure hidden", ".github/workflows/kanz-ci.yml", "- name: Typecheck\n        working-directory: web", "- name: Typecheck\n        continue-on-error: true\n        working-directory: web", "CI step Typecheck"},
		{"CI compiler conditional", ".github/workflows/kanz-ci.yml", "- name: Typecheck\n        working-directory: web", "- name: Typecheck\n        if: false\n        working-directory: web", "CI step Typecheck"},
		{"CI web job disabled", ".github/workflows/kanz-ci.yml", "  web:\n    needs: scope\n    if: needs.scope.outputs.application == 'true'", "  web:\n    needs: scope\n    if: false", "CI job is absent or bypassed"},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			if strings.Count(files[mutation.path], mutation.before) != 1 {
				t.Fatal("mutation target must occur exactly once")
			}
			changed := map[string]string{}
			for path, raw := range files {
				changed[path] = raw
			}
			changed[mutation.path] = strings.Replace(changed[mutation.path], mutation.before, mutation.after, 1)
			if err := checkWebDualCompilerRecipe(changed); err == nil || !strings.Contains(err.Error(), mutation.diagnostic) {
				t.Fatalf("mutation did not reach %q: %v", mutation.diagnostic, err)
			}
		})
	}
}
