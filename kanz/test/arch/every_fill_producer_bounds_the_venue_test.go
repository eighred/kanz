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

// NO FILL FACT IS PUBLISHED WITHOUT BOUNDING THE VENUE'S CUMULATIVE (#1045).
//
// # The path this names, which is the one the aggregate never sees
//
// A fill reaches this platform three ways. Two of them — the synchronous Execute
// return and the crash-recovery adopt — fold through the OMS aggregate, where
// ApplyFill refuses a fill larger than the open quantity as OVERFILL. The third
// is the venue user-data websocket: the adapter builds the order.v1.Fill itself
// and publishes order.order.filled, and the OMS ORDER AGGREGATE IS NOT A
// CONSUMER OF THAT SUBJECT. The position projector is, and so is the accounting
// ledger.
//
// So on that path nothing bounded the arithmetic. Leaves was ordered minus the
// venue's cumulative through a plain subtraction, which represents a negative
// result and reports success, and a venue reporting a cumulative 14 against an
// order of 10 published a FACT carrying LeavesQuantity −4. The position book
// folded it and the ledger journalled both legs: the fund's book recorded an
// execution larger than any order the controls admitted, with no refusal, no
// quarantine and no counter. The first thing that could have noticed was a
// balance reconciliation against the venue, if it ran.
//
// # Why a SIBLING guard rather than widening the one next door
//
// TestEveryRefusedFillReachesQuarantine is scoped to the OMS order package by
// its own directory constant, and correctly so — it is about what the two
// aggregate paths do with a refusal. This is the path it structurally cannot
// see, because the refusal it requires is one nothing on this path ever forms.
// Widening that guard's scope would also make an estate-wide guard change land
// beside unrelated work, which this repository has already paid for once.
//
// # What it derives, and from where
//
// The PRODUCER SET IS NOT A LIST IN THIS FILE. #806 and #803 were both "somebody
// enumerated a set by hand and missed a member", so a hand-written roster here
// would be one more copy of the thing that broke. The set is derived from the
// module: a function is a fill producer when it BUILDS a populated
// order.v1.OrderFilled or OrderPartiallyFilled payload and names a fill subject
// in the same body. Consumers do neither — they unmarshal into a zero value and
// match the subject in a switch — so the derivation separates the two without
// being told which packages are which.
//
// The BOUND is execution.LeavesRemaining, reached directly or through a function
// in the producer's own package. Reachability rather than a same-body call
// because the Binance ingester computes its healed state in a helper, and
// requiring the call to be inline would have been a rule about layout.
//
// # Why it parses the AST with comments detached
//
// Guards in this tree have passed while asserting nothing because a regex over
// raw source matched their own explanatory prose. Every paragraph above names
// the subjects, the payload types and the helper, and none of it can satisfy
// anything below.

// fillBoundHelper is the shared bound both connectors must reach. Both spellings
// are the same function: the OKX bridge aliases it exported, the Binance bridge
// unexported, beside the other decimal helpers.
var fillBoundHelper = map[string]bool{"LeavesRemaining": true, "leavesRemaining": true}

// fillPayloadTypes are the FACT payloads only a producer builds.
var fillPayloadTypes = map[string]bool{"OrderFilled": true, "OrderPartiallyFilled": true}

// fillSubjectOrigins are the canonical names of the two fill subjects in the one
// package that declares them, plus the wire strings themselves. Everything else
// that names a fill subject in this module is derived from these at package
// level, and this guard follows those derivations rather than listing them.
var fillSubjectOrigins = map[string]bool{"SubjectFilled": true, "SubjectPartiallyFilled": true}

var fillSubjectLiterals = map[string]bool{
	"order.order.filled":           true,
	"order.order.partially_filled": true,
}

// fillBoundExempt names a fill producer that may publish without reaching
// LeavesRemaining, and the bound it reaches instead.
//
// DEFAULT-DENY. An entry is a claim that the venue's cumulative was ALREADY
// bounded before the payload was built, and it must name where. It is not a
// permission to publish an unbounded number.
var fillBoundExempt = map[string]string{
	// The OMS emitter builds the FACT from an OrderState the aggregate has
	// already folded the fill into, and ApplyFill refuses a fill larger than the
	// open quantity as OVERFILL before that state exists. The non-vacuity arm
	// below asserts that refusal is still there, so this entry cannot outlive it.
	"services/oms/internal/order::fillEvent": "the OMS aggregate's ApplyFill refuses the fill as OVERFILL before the state this FACT carries is built; asserted by the omsOverfillGuardIntact arm",
}

// omsOverfillGuard is where the exemption above says the bound lives.
const (
	omsAggregateFile = "services/oms/internal/order/aggregate.go"
	omsBoundFunc     = "ApplyFill"
	omsBoundCode     = "OVERFILL"
)

// fillBoundHelperDecl is where the shared bound is declared.
const fillBoundHelperDecl = "internal/execution/exchange_common.go"

func TestEveryFillFactProducerBoundsTheVenuesCumulative(t *testing.T) {
	root := moduleRoot(t)
	pkgs := parseModulePackages(t, root)

	producers := map[string]bool{} // "pkgdir::func"
	bounded := map[string]bool{}

	for pkgDir, pkg := range pkgs {
		subjectNames := pkg.subjectAliases
		for name, fn := range pkg.funcs {
			if !buildsAFillPayload(fn) || !namesAFillSubject(fn, subjectNames) {
				continue
			}
			key := pkgDir + "::" + name
			producers[key] = true
			if pkg.reaches(name, fillBoundHelper) {
				bounded[key] = true
			}
		}
	}

	// NON-VACUITY 1: the three producers the fill-fact package's own doc counts —
	// the OMS emitter and both venue user-data ingesters. A derivation that found
	// fewer has stopped seeing a producer, which is the state this guard exists to
	// make impossible.
	if len(producers) < 3 {
		t.Fatalf("only %d fill-FACT producer(s) derived (%v) — order.order.filled has three "+
			"independent producers, so a scan seeing fewer is not looking at all of them and "+
			"this guard is asserting nothing", len(producers), sortedKeys(producers))
	}

	// NON-VACUITY 2: the bound still exists, and still bounds.
	assertLeavesRemainingStillRefuses(t, root)

	// NON-VACUITY 3: the bound the exemption defers to still exists.
	assertOMSOverfillGuardIntact(t, root)

	var unbounded []string
	for key := range producers {
		if bounded[key] {
			continue
		}
		if reason, ok := fillBoundExempt[key]; ok {
			t.Logf("%s: exempt — %s", key, reason)
			continue
		}
		unbounded = append(unbounded, key)
	}
	if len(unbounded) > 0 {
		sort.Strings(unbounded)
		t.Errorf("these functions publish a fill FACT without bounding the venue's cumulative "+
			"filled quantity against the ordered quantity: %v.\n\n"+
			"A subtraction is not a bound: it represents a negative result and reports success, so "+
			"a venue reporting more filled than was sent produces a FACT with a NEGATIVE leaves "+
			"quantity. The position book folds it and the accounting ledger journals both legs, and "+
			"on this path the OMS order aggregate never sees it — it does not consume the fill "+
			"subject — so nothing downstream refuses it (#1045).\n\n"+
			"Compute leaves through execution.LeavesRemaining and refuse the whole report on its "+
			"error, or add the function to fillBoundExempt naming the bound it reaches instead.",
			unbounded)
	}

	// DEAD-ENTRY ARM.
	for key, reason := range fillBoundExempt {
		if !producers[key] {
			t.Errorf("exemption for %q (%s) names no fill-FACT producer — delete it", key, reason)
		}
	}
}

// assertLeavesRemainingStillRefuses proves the shared bound is a bound: it
// compares the two quantities and answers the over-fill sentinel.
//
// The last arm is the one worth having. An arch guard asserting a refusal EXISTS
// passes when the condition guarding it has been switched off with a constant —
// twice in this tree — and every symbol the guard reads is still present.
func assertLeavesRemainingStillRefuses(t *testing.T, root string) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.Join(root, filepath.FromSlash(fillBoundHelperDecl)), nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", fillBoundHelperDecl, err)
	}
	var fn *ast.FuncDecl
	for _, decl := range file.Decls {
		if d, ok := decl.(*ast.FuncDecl); ok && d.Name.Name == "LeavesRemaining" && d.Body != nil {
			fn = d
		}
	}
	if fn == nil {
		t.Fatalf("no LeavesRemaining in %s — the bound this guard requires every producer to reach "+
			"no longer exists", fillBoundHelperDecl)
	}

	var comparison *ast.BinaryExpr
	var refusalReached bool
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		ifs, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}
		bin, ok := ifs.Cond.(*ast.BinaryExpr)
		if !ok || bin.Op != token.GTR {
			return true
		}
		if !selectorNamesIn(ifs.Body)["ErrVenueOverfill"] && !identNamesIn(ifs.Body)["ErrVenueOverfill"] {
			return true
		}
		comparison, refusalReached = bin, true
		return false
	})
	if !refusalReached {
		t.Fatalf("LeavesRemaining no longer refuses on a `>` comparison that answers ErrVenueOverfill — "+
			"the helper every producer is required to reach has stopped bounding anything, and the "+
			"reachability check above would still pass (%s)", fillBoundHelperDecl)
	}
	// A CONSTANT IN THE CONDITION DISABLES THE REFUSAL WHILE LEAVING EVERY SYMBOL
	// IN PLACE. `false && cumulative > ordered` reads as a live guard to anything
	// looking for the comparison.
	for _, side := range []ast.Expr{comparison.X, comparison.Y} {
		if id, ok := side.(*ast.Ident); ok && (id.Name == "true" || id.Name == "false") {
			t.Fatalf("LeavesRemaining's over-fill comparison carries the bool constant %q — the "+
				"refusal is switched off and every symbol this guard reads is still present", id.Name)
		}
	}
}

// assertOMSOverfillGuardIntact keeps the exemption honest: the OMS emitter is
// excused only for as long as the aggregate refusal it defers to is still there.
func assertOMSOverfillGuardIntact(t *testing.T, root string) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.Join(root, filepath.FromSlash(omsAggregateFile)), nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", omsAggregateFile, err)
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != omsBoundFunc || fn.Body == nil {
			continue
		}
		found := false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if ok && lit.Kind == token.STRING {
				if s, err := strconv.Unquote(lit.Value); err == nil && s == omsBoundCode {
					found = true
				}
			}
			return !found
		})
		if !found {
			t.Fatalf("%s no longer refuses with %q — the OMS emitter's exemption in fillBoundExempt "+
				"defers to that refusal, so the exemption is now excusing an unbounded producer",
				omsBoundFunc, omsBoundCode)
		}
		return
	}
	t.Fatalf("no %s in %s — the bound the OMS emitter's exemption names is gone", omsBoundFunc, omsAggregateFile)
}

// --- derivation ---

type fillPkg struct {
	funcs map[string]*ast.FuncDecl
	// subjectAliases are package-level names bound to a fill subject.
	subjectAliases map[string]bool
	// calls is the intra-package call graph, keyed by function name.
	calls map[string]map[string]bool
}

// reaches reports whether fn, or anything it calls inside its own package,
// mentions one of want.
func (p *fillPkg) reaches(fn string, want map[string]bool) bool {
	seen := map[string]bool{}
	var walk func(string) bool
	walk = func(name string) bool {
		if seen[name] {
			return false
		}
		seen[name] = true
		for callee := range p.calls[name] {
			if want[callee] {
				return true
			}
			if walk(callee) {
				return true
			}
		}
		return false
	}
	return walk(fn)
}

func parseModulePackages(t *testing.T, root string) map[string]*fillPkg {
	t.Helper()
	// .claude is the agent-worktree root: without it this walk reads another
	// agent's checkout and reports findings against source this branch does not
	// have. gen is the generated schema SDK.
	skipDir := map[string]bool{
		".git": true, ".claude": true, ".gotmp": true, "gen": true,
		"node_modules": true, "vendor": true, "testdata": true,
	}
	pkgs := map[string]*fillPkg{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDir[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// Mode 0: comments are not attached, so this guard's own prose — and the
		// paragraphs beside every producer — cannot satisfy anything.
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil // a file this walk cannot parse is not evidence of a producer
		}
		rel, rerr := filepath.Rel(root, filepath.Dir(path))
		if rerr != nil {
			return rerr
		}
		key := filepath.ToSlash(rel)
		pkg := pkgs[key]
		if pkg == nil {
			pkg = &fillPkg{
				funcs:          map[string]*ast.FuncDecl{},
				subjectAliases: map[string]bool{},
				calls:          map[string]map[string]bool{},
			}
			pkgs[key] = pkg
		}
		collectSubjectAliases(file, pkg.subjectAliases)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			pkg.funcs[fn.Name.Name] = fn
			pkg.calls[fn.Name.Name] = calleeNames(fn.Body)
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("walk %s: %v", root, err)
	}
	return pkgs
}

// collectSubjectAliases records package-level names whose value is one of the
// fill subjects — either the canonical constant or the wire string.
func collectSubjectAliases(file *ast.File, into map[string]bool) {
	for _, decl := range file.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || (gd.Tok != token.CONST && gd.Tok != token.VAR) {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if i >= len(vs.Values) {
					continue
				}
				if exprIsAFillSubject(vs.Values[i], into) {
					into[name.Name] = true
				}
			}
		}
	}
}

func exprIsAFillSubject(e ast.Expr, known map[string]bool) bool {
	switch v := e.(type) {
	case *ast.SelectorExpr:
		return fillSubjectOrigins[v.Sel.Name]
	case *ast.Ident:
		return fillSubjectOrigins[v.Name] || known[v.Name]
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return false
		}
		s, err := strconv.Unquote(v.Value)
		return err == nil && fillSubjectLiterals[s]
	}
	return false
}

// buildsAFillPayload reports whether a body constructs a POPULATED OrderFilled or
// OrderPartiallyFilled. A consumer allocates a zero value to unmarshal into; only
// a producer fills the fields in.
func buildsAFillPayload(fn *ast.FuncDecl) bool {
	found := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok || len(cl.Elts) == 0 {
			return true
		}
		switch typ := cl.Type.(type) {
		case *ast.SelectorExpr:
			found = found || fillPayloadTypes[typ.Sel.Name]
		case *ast.Ident:
			found = found || fillPayloadTypes[typ.Name]
		}
		return !found
	})
	return found
}

// namesAFillSubject reports whether a body mentions one of the fill subjects,
// through the canonical constant, a package-level alias of it, or the wire string.
func namesAFillSubject(fn *ast.FuncDecl, aliases map[string]bool) bool {
	found := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.SelectorExpr:
			found = found || fillSubjectOrigins[v.Sel.Name]
		case *ast.Ident:
			found = found || fillSubjectOrigins[v.Name] || aliases[v.Name]
		case *ast.BasicLit:
			if v.Kind == token.STRING {
				if s, err := strconv.Unquote(v.Value); err == nil && fillSubjectLiterals[s] {
					found = true
				}
			}
		}
		return !found
	})
	return found
}

// calleeNames is every function name a body calls, by its final identifier — a
// plain call and a method call both reduce to the name, which is what the
// reachability walk needs.
func calleeNames(body *ast.BlockStmt) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch f := call.Fun.(type) {
		case *ast.Ident:
			out[f.Name] = true
		case *ast.SelectorExpr:
			out[f.Sel.Name] = true
		}
		return true
	})
	return out
}

// identNamesIn is every bare identifier in a block. selectorNamesIn, its sibling
// in this package, answers the qualified half.
func identNamesIn(b *ast.BlockStmt) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(b, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			out[id.Name] = true
		}
		return true
	})
	return out
}
