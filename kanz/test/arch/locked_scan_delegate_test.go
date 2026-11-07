package arch

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// A direct local decoder may own the Scan. Resolve only a uniquely declared
// same-package function with one row parameter, one direct Scan, and no writes
// to that parameter. Unknown forms stay unmatched in the literal cross-check.
// As with chained scans, this proves plain-local use, not financial semantics.
func analyseDelegatedLockedScans(fset *token.FileSet, rel string, file *ast.File, functions map[string][]*ast.FuncDecl) []lockedScan {
	var out []lockedScan
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return true
		}
		name, ok := call.Fun.(*ast.Ident)
		if !ok || len(functions[name.Name]) != 1 {
			return true
		}
		query, ok := call.Args[0].(*ast.CallExpr)
		if !ok || !mentionsForUpdate(query) {
			return true
		}
		selector, ok := query.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "QueryRow" {
			return true
		}
		fn := functions[name.Name][0]
		if fn.Recv != nil || fn.Body == nil || len(fn.Type.Params.List) != 1 || len(fn.Type.Params.List[0].Names) != 1 {
			return true
		}
		parameter := fn.Type.Params.List[0].Names[0].Name
		var scans []*ast.CallExpr
		writes := false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.AssignStmt:
				for _, lhs := range x.Lhs {
					if id, ok := lhs.(*ast.Ident); ok && id.Name == parameter {
						writes = true
					}
				}
			case *ast.ValueSpec:
				for _, id := range x.Names {
					if id.Name == parameter {
						writes = true
					}
				}
			case *ast.CallExpr:
				if sel, ok := x.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Scan" {
					if id, ok := sel.X.(*ast.Ident); ok && id.Name == parameter {
						scans = append(scans, x)
					}
				}
			}
			return true
		})
		if writes || len(scans) != 1 {
			return true
		}
		scan := scans[0]
		result := lockedScan{site: fmt.Sprintf("%s:%d", rel, fset.Position(call.Pos()).Line), fn: name.Name}
		declarations := declaredIn(fn.Body)
		for _, arg := range scan.Args {
			address, ok := arg.(*ast.UnaryExpr)
			if !ok || address.Op != token.AND {
				continue
			}
			id, ok := address.X.(*ast.Ident)
			if !ok || id.Name == "_" {
				continue
			}
			result.scanned = append(result.scanned, id.Name)
			if !readElsewhere(fn.Body, declarations, id.Name, scan.Pos(), scan.End()) {
				result.unread = append(result.unread, id.Name)
			}
		}
		if len(result.scanned) > 0 {
			out = append(out, result)
		}
		return true
	})
	return out
}

func TestLockedScanDecoderMutationCoverage(t *testing.T) {
	const source = `package fixture
func acquire(tx Tx) { decoded, err := decode(tx.QueryRow(ctx, "SELECT value FROM records FOR UPDATE")); consume(decoded,err) }
func decode(row Row) (string,error) { var value string; err := row.Scan(&value); return value,err }
`
	for _, tc := range []struct {
		name, old, next       string
		wantSites, wantUnread int
	}{
		{"control", "", "", 1, 0},
		{"scan omitted", "row.Scan(&value)", "ignored()", 0, 0},
		{"value ignored", "return value,err", "return \"\",err", 1, 1},
		{"unknown decoder", "decode(tx.QueryRow", "other(tx.QueryRow", 0, 0},
		{"row reassigned", "var value string;", "row = replacement; var value string;", 0, 0},
		{"row shadowed", "var value string;", "row := replacement; var value string;", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := source
			if tc.old != "" {
				text = strings.Replace(text, tc.old, tc.next, 1)
			}
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "fixture.go", text, 0)
			if err != nil {
				t.Fatal(err)
			}
			functions := map[string][]*ast.FuncDecl{}
			for _, decl := range file.Decls {
				if fn, ok := decl.(*ast.FuncDecl); ok {
					functions[fn.Name.Name] = append(functions[fn.Name.Name], fn)
				}
			}
			sites := analyseDelegatedLockedScans(fset, "fixture.go", file, functions)
			unread := 0
			for _, site := range sites {
				unread += len(site.unread)
			}
			if len(sites) != tc.wantSites || unread != tc.wantUnread {
				t.Fatalf("sites=%d unread=%d", len(sites), unread)
			}
		})
	}
}
