package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

// Synthetic keys are useful for differential TLS fixtures, never for runtime
// identity. Production continues to obtain its SVID from the workload API.
func TestSyntheticIdentityCannotBecomeRuntimeAuthority(t *testing.T) {
	walkGoFilesWithComments(t, moduleRoot(t), token.NewFileSet(), func(rel string, f *ast.File) {
		if runtimeSyntheticImport(rel, f) {
			t.Errorf("%s imports synthetic identity outside tests", rel)
		}
	})
	f, err := parser.ParseFile(token.NewFileSet(), "fixture.go", `package fixture; import _ "github.com/eighred/kanz/internal/testidentity"`, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !runtimeSyntheticImport("services/example/main.go", f) || runtimeSyntheticImport("services/example/main_test.go", f) {
		t.Fatal("synthetic identity import mutation was misclassified")
	}
	for _, imported := range []string{"github.com/eighred/kanz/internal/testidentity", "github.com/eighred/kanz/services/oms/internal/conformancetestkit"} {
		for _, location := range []string{"services/oms/cmd/oms/main.go", "services/oms/internal/order/recovery.go", "cmd/example/main.go"} {
			mutated, err := parser.ParseFile(token.NewFileSet(), "fixture.go", "package fixture; import _ "+strconv.Quote(imported), 0)
			if err != nil {
				t.Fatal(err)
			}
			if !runtimeSyntheticImport(location, mutated) || runtimeSyntheticImport("services/oms/tools/recovery-conformance/recovery_test.go", mutated) {
				t.Fatal("synthetic transport production import mutation was misclassified")
			}
		}
	}
}

func runtimeSyntheticImport(rel string, f *ast.File) bool {
	if strings.HasSuffix(rel, "_test.go") {
		return false
	}
	for _, i := range f.Imports {
		path := strings.Trim(i.Path.Value, `"`)
		if path == "github.com/eighred/kanz/internal/testidentity" || path == "github.com/eighred/kanz/services/oms/internal/conformancetestkit" {
			return true
		}
	}
	return false
}
