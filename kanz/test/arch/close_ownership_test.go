package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

func TestProductionCloseOwnershipUsesDurableView(t *testing.T) {
	for _, venue := range []string{"binance", "okx"} {
		path := filepath.Join(moduleRoot(t), "services", "venue-"+venue, "cmd", "venue-"+venue, "main.go")
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		bound := false
		ast.Inspect(file, func(n ast.Node) bool {
			assignment, ok := n.(*ast.AssignStmt)
			if !ok || len(assignment.Lhs) != 1 || len(assignment.Rhs) != 1 {
				return true
			}
			name, ok := assignment.Lhs[0].(*ast.Ident)
			if !ok || name.Name != "closes" {
				return true
			}
			value, ok := assignment.Rhs[0].(*ast.Ident)
			if !ok || value.Name != "view" {
				t.Errorf("%s pending closes do not share the durable adapter store", venue)
			} else {
				bound = true
			}
			return true
		})
		if !bound {
			t.Errorf("%s durable close binding missing", venue)
		}
	}
}
