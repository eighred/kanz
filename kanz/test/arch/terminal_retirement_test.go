package arch

import (
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The owner explicitly retired terminal interfaces. Preserve independent
// administrative commands and their security boundaries without restoring a UI.
func retiredTerminalPath(name string) bool {
	name = strings.ToLower(filepath.ToSlash(name))
	for _, prefix := range []string{
		"kanz/internal/tui/", "kanz/cmd/universe/", "kanz/cmd/kanz_monitor/", "kanz/test/e2e/tui/",
	} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func TestRetiredTerminalInterfacesStayRemoved(t *testing.T) {
	root := filepath.Dir(moduleRoot(t))
	raw, err := exec.Command("git", "-C", root, "ls-files", "--cached", "--others", "--exclude-standard", "-z").Output()
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, name := range strings.Split(string(raw), "\x00") {
		if name == "" {
			continue
		}
		absolute := filepath.Join(root, name)
		if _, err := os.Stat(absolute); os.IsNotExist(err) {
			continue // staged/unstaged deletions are not effective source
		} else if err != nil {
			t.Fatal(err)
		}
		checked++
		if retiredTerminalPath(name) {
			t.Errorf("retired terminal source returned: %s", name)
		}
		if !strings.HasSuffix(name, ".go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), absolute, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range file.Imports {
			imported, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(imported, "github.com/charmbracelet/") ||
				strings.HasPrefix(imported, modulePath+"/internal/tui") {
				t.Errorf("retired terminal dependency in %s: %s", name, imported)
			}
		}
	}
	if checked < 1000 {
		t.Fatal("effective repository inventory is incomplete")
	}
	for name, forbidden := range map[string]string{
		"kanz/go.mod":                  "github.com/charmbracelet/",
		"kanz/infra/nats/tenancy.yaml": "spiffe://kanz.internal/ns/kanz-operator/sa/kanz-monitor",
	} {
		body, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), forbidden) {
			t.Errorf("retired terminal dependency or broker grant in %s", name)
		}
	}
}

func TestTerminalRetirementRejectsReintroduction(t *testing.T) {
	for _, name := range []string{"kanz/internal/tui/new/client.go", "kanz/cmd/universe/main.go",
		"kanz/cmd/kanz_monitor/new.go", "kanz/test/e2e/tui/flow_test.go", "kanz/cmd/Universe/main.go"} {
		if !retiredTerminalPath(name) {
			t.Errorf("accepted retired path %s", name)
		}
	}
	if retiredTerminalPath("kanz/cmd/kanz_lake_snapshot/internal/gatewayclient/client.go") {
		t.Fatal("independent snapshot command was classified as a terminal interface")
	}
}
