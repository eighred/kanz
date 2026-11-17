package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// MCP IS AN AGENT-FACING READ PLANE, AND THE IMPORT GRAPH IS WHERE THAT IS TRUE
// OR NOT (#743).
//
// AGENTS.md states three boundaries for any MCP surface, and calls each one
// worth a guard:
//
//  1. It must never receive, expose, store or proxy a venue credential.
//  2. It must never expose order placement, execution, cancel, amend, or any
//     other trading capability. Read plane only.
//  3. It is not the venue transport, and adding one must not create a second
//     answer to "how does the platform talk to an exchange".
//
// A unit test can show that today's tools are reads. It cannot show that a
// capital action is UNREACHABLE, because reachability is a property of what the
// package can import — and the day somebody adds a tool, the import is what
// arrives first. So this reads the graph.
//
// WHY DENY-BY-DEFAULT ON A LIST OF PACKAGES rather than a list of forbidden
// symbols: a symbol list is a list of the ways somebody already thought of. The
// OMS's order package, the execution router, the venue adapters and the
// credential machinery are the reachable capital surface of this estate; naming
// them and refusing the lot is the only version that survives a package being
// renamed or a new one appearing beside it.
//
// WHAT IT CANNOT CHECK: that a tool the plane DOES expose projects narrowly
// enough. "Server-side filtered and projected" is a judgement about a payload,
// and services/mcp's own tests carry it.

// mcpTree is the plane under guard.
const mcpTree = "services/mcp"

// mcpForbiddenImports are the trees an MCP read plane may not reach, each with
// the boundary it would breach.
var mcpForbiddenImports = map[string]string{
	"internal/execution":                 "the execution router and the Venue seam — boundary 3, and the shortest path to boundary 2",
	"internal/venueadapter":              "venue adapters and exchange auth — boundaries 1 and 3",
	"internal/venueadapter/exchangeauth": "exchange credential signing — boundary 1, the one that is unrecoverable",
	"services/venue_binance":             "a venue connector — boundary 3",
	"services/venue_okx":                 "a venue connector — boundary 3",
	"services/oms/internal/order":        "the OMS order aggregate: submit, amend, cancel — boundary 2",
	"internal/secret":                    "credential material — boundary 1",
	"internal/vault":                     "credential material — boundary 1",
}

// mcpForbiddenSchemas are generated SDK packages whose presence would mean the
// plane is speaking a capital or venue contract, whatever it does with it.
var mcpForbiddenSchemas = map[string]string{
	"kanz-schemas-go/venue/v1": "venue.v1 is the OMS → adapter transport — boundary 3",
	"kanz-schemas-go/order/v1": "order.v1 carries SubmitOrder, AmendOrder and CancelOrder — boundary 2",
}

// mcpImportExempt maps "<file> -> <import>" to the reason it is permitted.
//
// IT IS EMPTY. There is no argued case: a read plane that needs the order
// aggregate or a venue client is not a read plane, and an exemption here would
// be the decision to make it something else — which #743 says requires its own
// issue, not a line in a test.
var mcpImportExempt = map[string]string{}

func TestTheMCPPlaneCannotReachACapitalOrVenueCapability(t *testing.T) {
	root := moduleRoot(t)
	dir := filepath.Join(root, filepath.FromSlash(mcpTree))
	if _, err := os.Stat(dir); err != nil {
		t.Skipf("%s does not exist yet — this guard arms with the plane", mcpTree)
	}

	var breaches []string
	seenExempt := map[string]bool{}
	files := 0

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, perr := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if perr != nil {
			t.Fatalf("parse %s: %v — the guard cannot check what it cannot parse", path, perr)
		}
		rel, _ := filepath.Rel(root, path)
		slash := filepath.ToSlash(rel)
		files++
		for _, imp := range f.Imports {
			p, uerr := strconv.Unquote(imp.Path.Value)
			if uerr != nil {
				continue
			}
			if reason, bad := mcpBreach(p); bad {
				key := slash + " -> " + p
				if why, ok := mcpImportExempt[key]; ok {
					seenExempt[key] = true
					t.Logf("%s: exempt — %s", key, why)
					continue
				}
				breaches = append(breaches, key+" ("+reason+")")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", mcpTree, err)
	}

	// NON-VACUITY: the plane has source. A walk that found nothing would report a
	// clean graph having read no files at all, which is the failure mode every
	// guard in this tree is written against.
	if files < 2 {
		t.Fatalf("walked %d Go file(s) under %s — the scan is broken, not the plane", files, mcpTree)
	}

	if len(breaches) > 0 {
		sort.Strings(breaches)
		t.Errorf("%d import(s) put a capital or venue capability within reach of the MCP plane: %v.\n"+
			"AGENTS.md: MCP is read-only, deny-by-default, and NOT the venue transport — it may not "+
			"place, cancel or amend orders, may not call a venue, and may not hold a credential. An "+
			"import is how that becomes possible, and it arrives before the tool that uses it. If "+
			"this plane genuinely needs one of these, that is a decision with its own issue (#743 "+
			"out-of-scope), not an entry in mcpImportExempt.", len(breaches), breaches)
	}

	for key, why := range mcpImportExempt {
		if !seenExempt[key] {
			t.Errorf("exemption for %q (%s) matches no import — delete it", key, why)
		}
	}
}

// mcpBreach reports whether an import path crosses one of the three boundaries.
func mcpBreach(path string) (string, bool) {
	rest, ok := strings.CutPrefix(path, modulePath+"/")
	if ok {
		for tree, reason := range mcpForbiddenImports {
			if rest == tree || strings.HasPrefix(rest, tree+"/") {
				return reason, true
			}
		}
	}
	for schema, reason := range mcpForbiddenSchemas {
		if strings.Contains(path, schema) {
			return reason, true
		}
	}
	return "", false
}

// THE PLANE MUST NOT GROW A WRITE METHOD BY NAME EITHER (#743 boundary 2).
//
// The import guard above catches reaching a capital capability. This catches the
// other direction: a handler on this plane whose own name claims one. The two
// are different failures — a tool called submit_order that reads nothing is
// still a tool this plane must not offer, and its absence of imports would not
// say so.
func TestTheMCPPlaneDeclaresNoWriteSideCapability(t *testing.T) {
	root := moduleRoot(t)
	dir := filepath.Join(root, filepath.FromSlash(mcpTree))
	if _, err := os.Stat(dir); err != nil {
		t.Skipf("%s does not exist yet", mcpTree)
	}

	var offenders []string
	functions, tools := 0, 0
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}
		rel, _ := filepath.Rel(root, path)
		slash := filepath.ToSlash(rel)
		fnCount, toolCount, found := mcpReadDeclarations(f, filepath.ToSlash(filepath.Dir(rel)) == mcpTree+"/internal/server")
		functions += fnCount
		tools += toolCount
		for _, offender := range found {
			offenders = append(offenders, slash+": "+offender)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if functions < 5 || tools == 0 {
		t.Fatalf("found %d function declaration(s) and %d Tool declaration(s) under %s — the scan is broken", functions, tools, mcpTree)
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Errorf("%d declaration(s) on the MCP plane claim a capital action or lack a verifiable tool name: %v.\n"+
			"This plane is read-only (#743). Functions and declared tools must not claim to place, "+
			"cancel or amend an order. Tool names must be explicit non-empty string literals so "+
			"this guard can verify the offered capability.", len(offenders), offenders)
	}
}

// mcpReadDeclarations checks all function names and actual server.Tool literals,
// including implicit literals nested in []Tool. Comments and unrelated Name
// fields are not declarations of an MCP capability. The owner flag identifies
// the package where Tool is defined; other packages must import that exact type.
func mcpReadDeclarations(f *ast.File, owner bool) (functions, tools int, offenders []string) {
	imports := map[string]bool{}
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || path != modulePath+"/"+mcpTree+"/internal/server" {
			continue
		}
		alias := "server"
		if imp.Name != nil {
			alias = imp.Name.Name
		}
		imports[alias] = true
	}
	packageTypes := map[*ast.Object]bool{}
	for _, decl := range f.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok {
			functions++
			if mcpCapitalName(fn.Name.Name) {
				offenders = append(offenders, "function "+fn.Name.Name)
			}
		}
		if gen, ok := decl.(*ast.GenDecl); ok && gen.Tok == token.TYPE {
			for _, spec := range gen.Specs {
				if typ, ok := spec.(*ast.TypeSpec); ok {
					packageTypes[typ.Name.Obj] = true
				}
			}
		}
	}
	isTool := func(expr ast.Expr) bool {
		for {
			switch typ := expr.(type) {
			case *ast.ParenExpr:
				expr = typ.X
			case *ast.StarExpr:
				expr = typ.X
			default:
				goto resolved
			}
		}
	resolved:
		switch typ := expr.(type) {
		case *ast.Ident:
			return typ.Name == "Tool" && (owner || imports["."]) &&
				(typ.Obj == nil || owner && packageTypes[typ.Obj])
		case *ast.SelectorExpr:
			pkg, ok := typ.X.(*ast.Ident)
			return ok && typ.Sel.Name == "Tool" && imports[pkg.Name] &&
				(pkg.Obj == nil || pkg.Obj.Kind == ast.Pkg)
		}
		return false
	}
	seen := map[*ast.CompositeLit]bool{}
	var inspectLiteral func(*ast.CompositeLit, ast.Expr)
	inspectLiteral = func(lit *ast.CompositeLit, typ ast.Expr) {
		if seen[lit] {
			return
		}
		seen[lit] = true
		if lit.Type != nil {
			typ = lit.Type
		}
		if isTool(typ) {
			tools++
			name, ok := fieldValue(lit, "Name").(*ast.BasicLit)
			if !ok || name.Kind != token.STRING {
				offenders = append(offenders, "Tool.Name must be an explicit non-empty string literal")
				return
			}
			value, err := strconv.Unquote(name.Value)
			if err != nil || strings.TrimSpace(value) == "" {
				offenders = append(offenders, "Tool.Name must be an explicit non-empty string literal")
			} else if mcpCapitalName(value) {
				offenders = append(offenders, "tool "+value)
			}
		}
		var element ast.Expr
		switch container := typ.(type) {
		case *ast.ArrayType:
			element = container.Elt
		case *ast.MapType:
			element = container.Value
		}
		for _, el := range lit.Elts {
			if kv, ok := el.(*ast.KeyValueExpr); ok {
				el = kv.Value
			}
			if child, ok := el.(*ast.CompositeLit); ok {
				inspectLiteral(child, element)
			}
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		if lit, ok := n.(*ast.CompositeLit); ok {
			inspectLiteral(lit, nil)
		}
		return true
	})
	return functions, tools, offenders
}

func mcpCapitalName(name string) bool {
	normalized := strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(name))
	for _, verb := range []string{"submitorder", "placeorder", "cancelorder", "amendorder",
		"executeorder", "sendorder", "routeorder"} {
		if strings.Contains(normalized, verb) {
			return true
		}
	}
	return false
}
