package arch

import (
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// These directories are discovery contracts, rather than project naming choices.
// Forge's validator binds the skill names to these entrypoint directories.
var directoryNameContracts = map[string]bool{
	".agents": true, ".github": true,
	"kanz/.githooks": true, "kanz_schemas/.github": true,
	".agents/plugins/kanz-forge":                               true,
	".agents/plugins/kanz-forge/.codex-plugin":                 true,
	".agents/plugins/kanz-forge/skills/engineering-standard":   true,
	".agents/plugins/kanz-forge/skills/financial-verification": true,
	".agents/plugins/kanz-forge/skills/issue-archaeology":      true,
	".agents/plugins/kanz-forge/skills/kanz-verify":            true,
	".agents/plugins/kanz-forge/skills/pr-delivery":            true,
	".agents/plugins/kanz-forge/skills/security-audit":         true,
	".agents/plugins/kanz-forge/skills/stack-routing":          true,
}

var snakeDirectoryName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Deployed identities remain stable when their source directory is renamed.
func sourceDirectoryName(identity string) string  { return strings.ReplaceAll(identity, "-", "_") }
func deployedServiceName(directory string) string { return strings.ReplaceAll(directory, "_", "-") }

func invalidRepositoryDirectories(files []string) []string {
	invalid := map[string]bool{}
	for _, file := range files {
		for dir := path.Dir(file); dir != "." && dir != "/"; dir = path.Dir(dir) {
			if !snakeDirectoryName.MatchString(path.Base(dir)) && !directoryNameContracts[dir] {
				invalid[dir] = true
			}
		}
	}
	result := make([]string, 0, len(invalid))
	for dir := range invalid {
		result = append(result, dir)
	}
	sort.Strings(result)
	return result
}

func TestRepositoryDirectoriesUseSnakeCase(t *testing.T) {
	root := filepath.Dir(moduleRoot(t))
	command := exec.Command("git", "-C", root, "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	raw, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	files := strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
	if len(files) < 1000 {
		t.Fatal("repository inventory is incomplete")
	}
	if invalid := invalidRepositoryDirectories(files); len(invalid) != 0 {
		t.Fatalf("project directories must use snake_case: %v", invalid)
	}
}

func TestRepositoryDirectoryNamingRejectsRegression(t *testing.T) {
	allowed := []string{"kanz/services/risk_engine/cmd/risk_engine/main.go", "web/src/App.vue",
		".github/actions/setup_buf/action.yml", ".agents/plugins/kanz-forge/skills/kanz-verify/SKILL.md"}
	if invalid := invalidRepositoryDirectories(allowed); len(invalid) != 0 {
		t.Fatal(invalid)
	}
	for _, file := range []string{"kanz-jl/Project.toml", "kanz/services/risk-engine/main.go",
		".agents/plugins/kanz-forge/skills/unregistered-skill/SKILL.md"} {
		if len(invalidRepositoryDirectories([]string{file})) == 0 {
			t.Fatalf("accepted a noncanonical directory: %s", file)
		}
	}
}
