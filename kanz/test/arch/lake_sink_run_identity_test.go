package arch

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func lakeSinkRunIdentity(source string) error {
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", source, 0)
	if err != nil {
		return err
	}
	alias := ""
	for _, imported := range f.Imports {
		path, err := strconv.Unquote(imported.Path.Value)
		if err != nil {
			return err
		}
		if path == "github.com/eighred/kanz/services/lake-sink/internal/sink" {
			alias = "sink"
			if imported.Name != nil {
				alias = imported.Name.Name
			}
		}
	}
	if alias == "" || alias == "." || alias == "_" {
		return fmt.Errorf("actual lake sink import is unresolved")
	}
	count := 0
	var problem error
	ast.Inspect(f, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "NewFileSink" {
			return true
		}
		receiver, ok := selector.X.(*ast.Ident)
		if !ok || receiver.Name != alias {
			return true
		}
		count++
		if len(call.Args) != 2 && len(call.Args) != 3 {
			problem = fmt.Errorf("landing constructor shape changed")
			return false
		}
		// The optional snapshot mode adds only the separately guarded options
		// slice; it never supplies or replaces the per-run identity argument.
		if len(call.Args) == 3 {
			options, ok := call.Args[2].(*ast.Ident)
			if !ok || options.Name != "fileOptions" || !call.Ellipsis.IsValid() {
				problem = fmt.Errorf("unresolved landing options")
				return false
			}
		}
		literal, ok := call.Args[1].(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			problem = fmt.Errorf("lake sink must request per-run file identity")
			return false
		}
		value, err := strconv.Unquote(literal.Value)
		if err != nil || value != "" {
			problem = fmt.Errorf("fixed identity would share part files between runs")
		}
		return true
	})
	if problem != nil {
		return problem
	}
	if count != 1 {
		return fmt.Errorf("expected one actual landing constructor, found %d", count)
	}
	return nil
}

func TestLakeSinkRequestsRunIdentityForPartFiles(t *testing.T) {
	source := string(mustReadArchFile(t, filepath.Join(moduleRoot(t), "services/lake-sink/cmd/lake-sink/main.go")))
	if err := lakeSinkRunIdentity(source); err != nil {
		t.Fatal(err)
	}
	for _, argument := range []string{"cfg.Source", `"lake-sink"`} {
		mutated := strings.Replace(source, `sink.NewFileSink(cfg.OutputDir, ""`, "sink.NewFileSink(cfg.OutputDir, "+argument, 1)
		if mutated == source || lakeSinkRunIdentity(mutated) == nil {
			t.Fatal("fixed service identity escaped the executable wiring guard")
		}
	}
}
