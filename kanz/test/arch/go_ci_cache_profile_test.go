package arch

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const goCacheAction = "55cc8345863c7cc4c66a329aec7e433d2d1c52a9"
const goCachePrefix = "kanz-go-complete-v3-${{ runner.os }}-${{ runner.arch }}-${{ steps.compiler_cache_meta.outputs.version }}-${{ steps.compiler_cache_meta.outputs.image }}-${{ steps.compiler_cache_meta.outputs.fingerprint }}-${{ matrix.kind }}-${{ matrix.shard }}-"
const goCacheInputs = "${{ hashFiles('kanz/.gotmp/go-ci-cache-profile', 'kanz/go.mod', 'kanz/go.sum', '.github/go-ci-cache-profile', 'kanz-schemas/buf.gen.yaml', 'kanz-schemas/proto/**/*.proto', '.github/scripts/go_shard_evidence.py') }}"
const goCacheWriter = "success() && github.event_name == 'push' && github.ref == 'refs/heads/main' && steps.compiler_cache_meta.outputs.enabled == 'true'"

type goCacheStep struct {
	Name    string            `yaml:"name"`
	ID      string            `yaml:"id"`
	Uses    string            `yaml:"uses"`
	If      string            `yaml:"if"`
	Run     string            `yaml:"run"`
	WorkDir string            `yaml:"working-directory"`
	With    map[string]string `yaml:"with"`
	Env     map[string]string `yaml:"env"`
}

// Pins cache authority and partition, not action internals or compiler
// correctness. Compilation reuse never counts as fresh test evidence.
func checkGoCacheProfile(data []byte) error {
	var workflow struct {
		Jobs map[string]struct{ Steps []goCacheStep }
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		return err
	}
	steps := workflow.Jobs["go-race"].Steps
	meta, restore, final, save, setup, native, setupAt := -1, -1, -1, -1, 0, -1, -1
	for i, step := range steps {
		switch {
		case strings.HasPrefix(step.Uses, "actions/setup-go@"):
			setup++
			setupAt = i
			if step.With["cache"] != "false" || step.With["cache-dependency-path"] != "" {
				return fmt.Errorf("implicit compiler cache writer enabled")
			}
		case step.Name == "Prepare complete MFA browser dependencies before compiler cache":
			if native != -1 || step.If != "" || strings.TrimSpace(step.Run) != "cd kanz-web\nnpm ci --no-audit --no-fund\nnpx --no-install playwright install --with-deps chromium" {
				return fmt.Errorf("native dependencies no longer fully prepared")
			}
			native = i
		case step.ID == "compiler_cache_meta":
			if meta != -1 || step.WorkDir != "kanz" || strings.TrimSpace(step.Run) != "python3 -B -m unittest discover -s ../.github/scripts -p test_go_ci_cache.py\npython3 ../.github/scripts/go_ci_cache.py" || step.If != "" {
				return fmt.Errorf("compiler metadata no longer checked")
			}
			meta = i
		case step.ID == "compiler_cache":
			if restore != -1 || step.Uses != "actions/cache/restore@"+goCacheAction || step.If != "steps.compiler_cache_meta.outputs.enabled == 'true'" || len(step.With) != 3 || step.With["path"] != "${{ steps.compiler_cache_meta.outputs.paths }}" || step.With["key"] != goCachePrefix+goCacheInputs || strings.TrimSpace(step.With["restore-keys"]) != goCachePrefix {
				return fmt.Errorf("compiler restore lost isolated complete profile")
			}
			restore = i
		case step.ID == "compiler_cache_final":
			if final != -1 || step.If != "success() && steps.compiler_cache_meta.outputs.enabled == 'true'" || step.WorkDir != "kanz" || step.Run != "python3 ../.github/scripts/go_ci_cache.py check" || step.Env["EXPECTED_FINGERPRINT"] != "${{ steps.compiler_cache_meta.outputs.fingerprint }}" {
				return fmt.Errorf("native compiler changes no longer disable saving")
			}
			final = i
		case strings.HasPrefix(step.Uses, "actions/cache/save@"):
			if save != -1 || step.Uses != "actions/cache/save@"+goCacheAction || step.If != goCacheWriter+" && steps.compiler_cache_final.outputs.stable == 'true' && steps.compiler_cache.outputs.cache-hit != 'true'" || len(step.With) != 2 || step.With["path"] != "${{ steps.compiler_cache_meta.outputs.paths }}" || step.With["key"] != "${{ steps.compiler_cache.outputs.cache-primary-key }}" {
				return fmt.Errorf("incomplete or non-main run can seed compiler cache")
			}
			save = i
		case strings.HasPrefix(step.Uses, "actions/cache"):
			return fmt.Errorf("unexpected compiler cache action")
		}
	}
	if setup != 1 || native < 0 || setupAt <= native || meta <= setupAt || restore <= meta || final <= restore || save != len(steps)-1 || final != save-1 {
		return fmt.Errorf("compiler cache authority/order incomplete")
	}
	for job, config := range workflow.Jobs {
		if job == "go-race" {
			continue
		}
		for _, step := range config.Steps {
			if strings.Contains(step.With["key"], "kanz-go-complete-") || strings.Contains(step.With["restore-keys"], "kanz-go-complete-") {
				return fmt.Errorf("another job can seed complete compiler profile")
			}
		}
	}
	return nil
}

func TestGoCICompilerCacheProfileIsExclusive(t *testing.T) {
	root := filepath.Dir(moduleRoot(t))
	if strings.TrimSpace(string(mustReadArchFile(t, filepath.Join(root, ".github/go-ci-cache-profile")))) != "kanz-go-race-v3" {
		t.Fatal("reviewed compiler profile version changed")
	}
	data := mustReadArchFile(t, filepath.Join(root, ".github/workflows/kanz-ci.yml"))
	if err := checkGoCacheProfile(data); err != nil {
		t.Fatal(err)
	}
	mutations := [][2]string{
		{"cache: false", "cache: true"},
		{"actions/cache/restore@" + goCacheAction, "actions/cache@" + goCacheAction},
		{"actions/cache/save@" + goCacheAction, "actions/cache/save@main"},
		{goCacheWriter, "always()"},
		{"steps.compiler_cache_final.outputs.stable == 'true'", "true"},
		{"steps.compiler_cache.outputs.cache-hit != 'true'", "true"},
		{"${{ steps.compiler_cache_meta.outputs.paths }}", "${{ github.workspace }}"},
		{"${{ steps.compiler_cache.outputs.cache-primary-key }}", "shared"},
		{"python3 ../.github/scripts/go_ci_cache.py check", "true"},
		{"name: Prepare complete MFA browser dependencies before compiler cache", "name: Omitted native preparation"},
		{"npx --no-install playwright install --with-deps chromium", "npx --no-install playwright install chromium"},
		{goCachePrefix + "\n", strings.TrimSuffix(goCachePrefix, "-") + "\n"},
	}
	for _, input := range []string{"runner.os", "runner.arch", "steps.compiler_cache_meta.outputs.version", "steps.compiler_cache_meta.outputs.image", "steps.compiler_cache_meta.outputs.fingerprint", "matrix.kind", "matrix.shard"} {
		mutations = append(mutations, [2]string{"${{ " + input + " }}", "shared"})
	}
	for _, input := range []string{"kanz/.gotmp/go-ci-cache-profile", "kanz/go.mod", "kanz/go.sum", ".github/go-ci-cache-profile", "kanz-schemas/buf.gen.yaml", "kanz-schemas/proto/**/*.proto", ".github/scripts/go_shard_evidence.py"} {
		mutations = append(mutations, [2]string{"'" + input + "'", "'removed'"})
	}
	for _, mutation := range mutations {
		t.Run(mutation[0], func(t *testing.T) {
			changed := strings.ReplaceAll(string(data), mutation[0], mutation[1])
			if changed == string(data) {
				t.Fatal("mutation did not apply")
			}
			if checkGoCacheProfile([]byte(changed)) == nil {
				t.Fatal("unsafe cache mutation accepted")
			}
		})
	}
	for _, path := range workflowFiles(t, root) {
		if filepath.Base(path) != "kanz-ci.yml" && strings.Contains(string(mustReadArchFile(t, path)), "kanz-go-complete-") {
			t.Fatalf("competing compiler namespace in %s", path)
		}
	}
}
