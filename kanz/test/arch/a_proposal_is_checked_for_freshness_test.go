package arch

import (
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// A REBALANCE PROPOSAL CANNOT BECOME ORDERS WITHOUT A FRESHNESS BOUND (#970).
//
// # What this protects
//
// bridge.ToOrders is the one function every materialization path passes through:
// bridge.Materialize calls it, and server.materialize — the HTTP route — calls it
// DIRECTLY. That second caller is the reason the freshness check lives in
// ToOrders rather than in Materialize, and it is the reason this guard exists:
// a check placed one level up would have been skipped by the route, silently,
// and the route is where the capital actually leaves.
//
// The failure being prevented is not "somebody deletes the check". It is
// "somebody adds a THIRD materialization path" — a batch job, a bus consumer, a
// second HTTP route — and reaches order construction by a way that never took a
// Freshness. That path would compile, pass its own tests, and materialize
// proposals of any age.
//
// # Why the signature is what is checked
//
// The guard asserts that ToOrders REQUIRES a bridge.Freshness parameter, and that
// its body consults it before constructing anything. A new caller then cannot
// avoid the question: it must supply a Freshness, and the zero value refuses
// (bridge.ErrFreshnessUnbounded), so "forgot to configure it" fails closed rather
// than passing everything.
//
// That is deliberately a check on the SHAPE rather than a list of approved
// callers. A call-site allow-list is a second copy of the set of materialization
// paths, and the defect class here is exactly "somebody added a member and the
// second copy did not learn about it" (#806, #803).
//
// # What it does NOT claim
//
// It does not prove any deployment configures a sensible bound — that is
// TestNoCompositionRootMaterializesWithoutABound below, and the composition
// root's own ERROR log. It proves the question cannot be skipped.

const bridgePkgDir = "../../services/optimization/internal/bridge"

// parseBridgePkg parses a package directory's non-test Go files.
func parseBridgePkg(t *testing.T, dir string) (*token.FileSet, []*ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatalf("glob %s: %v", dir, err)
	}
	sort.Strings(paths)
	var files []*ast.File
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		t.Fatalf("parsed no files in %s — the guard would pass vacuously", dir)
	}
	return fset, files
}

// findBridgeFunc returns the named top-level function.
func findBridgeFunc(files []*ast.File, name string) *ast.FuncDecl {
	for _, f := range files {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if ok && fn.Recv == nil && fn.Name.Name == name {
				return fn
			}
		}
	}
	return nil
}

// paramTypeNames renders a function's parameter types as source-ish strings.
func paramTypeNames(fset *token.FileSet, fn *ast.FuncDecl) []string {
	var out []string
	for _, field := range fn.Type.Params.List {
		var b strings.Builder
		if err := format.Node(&b, fset, field.Type); err != nil {
			continue
		}
		n := 1
		if len(field.Names) > 0 {
			n = len(field.Names)
		}
		for i := 0; i < n; i++ {
			out = append(out, b.String())
		}
	}
	return out
}

// TestToOrdersRequiresAFreshnessBound holds the signature and the ordering.
func TestToOrdersRequiresAFreshnessBound(t *testing.T) {
	fset, files := parseBridgePkg(t, bridgePkgDir)

	toOrders := findBridgeFunc(files, "ToOrders")
	if toOrders == nil {
		t.Fatal("bridge.ToOrders not found — it is the choke point every materialization path " +
			"passes through, and this guard is written against it. If it was renamed, update the " +
			"guard rather than letting it silently check nothing")
	}
	params := paramTypeNames(fset, toOrders)
	if !anyContains(params, "Freshness") {
		t.Fatalf("bridge.ToOrders takes %v and no Freshness.\n\n"+
			"Every path that turns a rebalance proposal into orders goes through this function, "+
			"including server.materialize which calls it DIRECTLY — so a freshness check anywhere "+
			"else is skippable by the HTTP route. The parameter is what forces a new caller to "+
			"answer the question; the zero value then refuses, so forgetting to configure a bound "+
			"fails closed rather than materializing proposals of any age (#970).", params)
	}

	// The check must run BEFORE anything is constructed. A refusal that happens
	// after the commands are built is still a refusal, but it means a future edit
	// that returns partial results on error would leak orders built from a stale
	// proposal.
	body := renderBody(t, fset, toOrders.Body)
	checkIdx := strings.Index(body, ".Check(")
	if checkIdx < 0 {
		t.Fatal("bridge.ToOrders takes a Freshness and never calls Check on it — the parameter is " +
			"decoration, and a stale proposal would materialize exactly as before (#970)")
	}
	buildIdx := strings.Index(body, "orderpb.SubmitOrder{")
	if buildIdx >= 0 && buildIdx < checkIdx {
		t.Fatal("bridge.ToOrders constructs a SubmitOrder before checking freshness — the refusal " +
			"must precede construction so no command built from a stale proposal can escape")
	}
}

// TestMaterializeCannotBypassToOrders holds the choke point itself.
//
// If Materialize ever built commands directly instead of delegating, the
// signature guard above would still pass while the second path went unchecked.
func TestMaterializeCannotBypassToOrders(t *testing.T) {
	fset, files := parseBridgePkg(t, bridgePkgDir)
	mat := findBridgeFunc(files, "Materialize")
	if mat == nil {
		t.Fatal("bridge.Materialize not found — update this guard rather than letting it check nothing")
	}
	body := renderBody(t, fset, mat.Body)
	if !strings.Contains(body, "ToOrders(") {
		t.Fatal("bridge.Materialize no longer delegates to ToOrders — it is building commands by " +
			"another route, which bypasses the freshness and mandate gates ToOrders holds (#970, #646)")
	}
	if strings.Contains(body, "orderpb.SubmitOrder{") {
		t.Fatal("bridge.Materialize constructs a SubmitOrder itself — every command must come from " +
			"ToOrders, which is where both proposal-level gates live")
	}
}

// TestTheFreshnessZeroValueRefuses is the property the whole design rests on: an
// unconfigured bound must be UNKNOWN and refuse, never "unlimited".
//
// It is checked here, in the estate-wide guard, and not only in the package's own
// tests, because it is the assumption every OTHER guard above leans on — the
// signature check is only load-bearing if forgetting to fill the parameter is
// safe.
func TestTheFreshnessZeroValueRefuses(t *testing.T) {
	fset, files := parseBridgePkg(t, filepath.Clean(bridgePkgDir))
	check := findBridgeMethod(files, "Freshness", "Check")
	if check == nil {
		t.Fatal("Freshness.Check not found — update this guard rather than letting it check nothing")
	}
	body := renderBody(t, fset, check.Body)
	if !strings.Contains(body, "MaxAge <= 0") {
		t.Fatal("Freshness.Check no longer refuses a non-positive MaxAge.\n\n" +
			"The zero Freshness is what a composition root that forgot the option leaves behind, " +
			"and what every caller gets by default. If it admits, then 'nobody set a bound' and " +
			"'the bound is unlimited' share an encoding on the path where a proposal becomes " +
			"capital — which is the pre-#970 behaviour restored silently (#970)")
	}
	if !strings.Contains(body, "ErrFreshnessUnbounded") {
		t.Fatal("Freshness.Check refuses an unset bound with something other than " +
			"ErrFreshnessUnbounded — the refusal CLASS is what the route renders and what a " +
			"dashboard counts")
	}
}

// findBridgeMethod returns a method on the named receiver type.
func findBridgeMethod(files []*ast.File, recv, name string) *ast.FuncDecl {
	for _, f := range files {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Name.Name != name {
				continue
			}
			for _, r := range fn.Recv.List {
				if t, ok := r.Type.(*ast.Ident); ok && t.Name == recv {
					return fn
				}
				if st, ok := r.Type.(*ast.StarExpr); ok {
					if t, ok := st.X.(*ast.Ident); ok && t.Name == recv {
						return fn
					}
				}
			}
		}
	}
	return nil
}

// renderBody renders a function body back to source so the guard can reason about
// ORDER — which call comes before which construction — rather than only presence.
func renderBody(t *testing.T, fset *token.FileSet, n ast.Node) string {
	t.Helper()
	var b strings.Builder
	if err := format.Node(&b, fset, n); err != nil {
		t.Fatalf("render: %v", err)
	}
	return b.String()
}

func anyContains(hay []string, needle string) bool {
	for _, h := range hay {
		if strings.Contains(h, needle) {
			return true
		}
	}
	return false
}
