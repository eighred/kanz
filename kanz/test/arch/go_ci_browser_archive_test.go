package arch

import (
	"fmt"
	"gopkg.in/yaml.v3"
	"path/filepath"
	"strings"
	"testing"
)

func checkBrowserArchiveRecipe(data []byte) error {
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct{ Name, Run, If string }
		}
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		return err
	}
	for _, name := range []string{"go-race", "web"} {
		setup, browser := -1, -1
		for i, s := range workflow.Jobs[name].Steps {
			if s.Name == "Select official HTTPS Ubuntu archive for browser dependencies" {
				if setup != -1 || s.If != "" {
					return fmt.Errorf("archive setup cannot be omitted or duplicated")
				}
				setup = i
				if strings.TrimSpace(s.Run) != "python3 -m unittest discover -s .github/scripts -p test_ci_ubuntu_archive.py\nsudo --preserve-env=GITHUB_ACTIONS,RUNNER_OS python3 .github/scripts/ci_ubuntu_archive.py" {
					return fmt.Errorf("unreviewed archive recipe")
				}
			}
			if strings.Contains(s.Run, "playwright install") {
				if browser != -1 || s.If != "" || !strings.Contains(s.Run, "npx --no-install playwright install --with-deps chromium") {
					return fmt.Errorf("complete browser dependency installation required")
				}
				browser = i
			}
		}
		if setup < 0 || browser <= setup {
			return fmt.Errorf("archive setup must precede complete browser installation on each runner")
		}
	}
	return nil
}
func TestCIBrowserArchivePreservesCompleteDependencyInstall(t *testing.T) {
	root := filepath.Dir(moduleRoot(t))
	data := mustReadArchFile(t, filepath.Join(root, ".github/workflows/kanz-ci.yml"))
	if err := checkBrowserArchiveRecipe(data); err != nil {
		t.Fatal(err)
	}
	for _, m := range [][2]string{
		{"Select official HTTPS Ubuntu archive for browser dependencies", "Removed archive setup"},
		{"name: Select official HTTPS Ubuntu archive for browser dependencies", "name: Select official HTTPS Ubuntu archive for browser dependencies\n        if: matrix.shard == 0"},
		{"--with-deps chromium", "chromium"},
		{"python3 -m unittest discover -s .github/scripts -p test_ci_ubuntu_archive.py", "true"},
		{"sudo --preserve-env=GITHUB_ACTIONS,RUNNER_OS python3 .github/scripts/ci_ubuntu_archive.py", "true"},
	} {
		changed := strings.Replace(string(data), m[0], m[1], 1)
		if changed == string(data) {
			t.Fatal("mutation missed target")
		}
		if checkBrowserArchiveRecipe([]byte(changed)) == nil {
			t.Fatal("weakened browser fixture accepted")
		}
	}
}
