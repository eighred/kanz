package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// EVERY MUTATION THAT CAN RAISE AN ORDER'S EXPOSURE PASSES THE CONTROL PLANE (#799).
//
// # What this is protecting
//
// handleAmend rewrote a resting order's size and price and called no control at
// all. s.gate.Check, halt.Refusal and s.dualControl each appeared exactly ONCE in
// the OMS and all three were inside submit; the aggregate bounded an amended
// quantity only BELOW, against filled_quantity; and the only brake in front of it
// — venueMayBeWorking — is a venue-divergence guard (#740) that says nothing
// about compliance and does not cover an order resting in PENDING_NEW or
// ACCEPTED. An order for 100 cleared the mandate, an amend raised it to
// 1,000,000, and the startup sweep routed the amended size past every control
// CLAUDE.md names as admission control.
//
// The repair is a re-entry: handleAmend builds the command the amended order
// would be and asks the same three seams submit asks. The property worth guarding
// is not the shape of that code, it is that the three seams are REACHED.
//
// # Why it walks the call graph instead of grepping
//
// Three guards in this tree have already passed while asserting nothing, because
// a regex over raw source matched the guard's own explanatory comment or a
// neighbouring field name. This resolves the package's AST and walks intra-package
// calls from each entry point, so it sees a control reached through a helper and
// does NOT see one merely named in a comment. The comment you are reading is
// itself inside the file the scan would otherwise match.
//
// # Its non-vacuity arms
//
// submit must reach all three — it is the known-good path, and if the walk stops
// finding them there the walk is broken rather than the estate. handleCancel must
// reach NONE of them: a cancel is deliberately ungated so an operator halting on
// a risk breach can still get out of the book, so a walk that "found" a control
// there would be finding it everywhere and discriminating nothing.

// amendControl is one admission control, named by the selector its call site
// wears — "the receiver's field, then the method".
type amendControl struct {
	name string
	// sel is matched against a call's rendered selector chain, e.g.
	// "s.gate.Check". Rendering the chain rather than matching an identifier is
	// what keeps `halt.Refusal` (a package function) and `s.gate.Check` (a field
	// method) in one vocabulary.
	sel  string
	what string
}

var amendControls = []amendControl{
	{"the pre-trade compliance gate", "s.gate.Check",
		"a mandate rule never sees the amended size, so an order the fund may not hold rests as if cleared"},
	{"the platform kill switch", "halt.Refusal",
		"an operator who stopped the platform on a risk breach has an order's size raised underneath them"},
	{"maker-checker", "s.dualControl.Decide",
		"an order admitted below the threshold on one signature is raised above it on the same one"},
}

func TestTheAmendPathReachesEveryAdmissionControl(t *testing.T) {
	root := moduleRoot(t)
	calls := selectorCallGraph(t, filepath.Join(root, filepath.FromSlash(omsOrderPkg)))

	// NON-VACUITY 1: the parse found the handlers at all.
	for _, fn := range []string{"handleAmend", "submit", "handleCancel"} {
		if _, ok := calls[fn]; !ok {
			t.Fatalf("no function %q was parsed out of %s — the package moved or the handler was "+
				"renamed, and every assertion below is checking an empty set", fn, omsOrderPkg)
		}
	}

	// NON-VACUITY 2: the walk finds the controls on the path that is known to
	// run them. If this stops holding, the walk has drifted and its verdict about
	// handleAmend is worthless.
	submitReach := reachableSelectors(calls, "submit")
	for _, c := range amendControls {
		if !submitReach[c.sel] {
			t.Fatalf("the call walk does not find %s (%s) from submit, which is the path that "+
				"demonstrably runs it — the walk is broken, so its answer about handleAmend "+
				"proves nothing", c.name, c.sel)
		}
	}

	// NON-VACUITY 3: the walk DISCRIMINATES. A cancel is deliberately ungated
	// (handleCancel says why: an operator halting on a risk breach must still be
	// able to get out of the book), so a walk that reported a control there would
	// be reporting one everywhere.
	cancelReach := reachableSelectors(calls, "handleCancel")
	for _, c := range amendControls {
		if cancelReach[c.sel] {
			t.Fatalf("the call walk reports %s (%s) reachable from handleCancel, which does not "+
				"run it — the walk is over-approximating and would pass for handleAmend whatever "+
				"handleAmend did", c.name, c.sel)
		}
	}

	// THE ASSERTION.
	amendReach := reachableSelectors(calls, "handleAmend")
	for _, c := range amendControls {
		if !amendReach[c.sel] {
			t.Errorf("handleAmend no longer reaches %s (%s).\n\n"+
				"An amend rewrites the size and price of a live order — it is the one mutation "+
				"that changes the terms every control was asked about, and it is reachable by any "+
				"entitled caller. Without this call, %s.\n\n"+
				"The re-entry lives in admitAmendment (services/oms/internal/order/amend.go), "+
				"which runs the controls over the command the amended order would be. If a "+
				"reduction is being let through, that is deliberate and lives inside "+
				"admitAmendment — it must not be done by removing the call.",
				c.name, c.sel, c.what)
		}
	}
}

// selectorCallGraph parses every non-test Go file in dir and returns, per
// top-level function or method name, the set of selector chains it calls
// ("s.gate.Check", "halt.Refusal", "s.admitAmendment").
//
// It is deliberately name-keyed rather than type-keyed. Everything it walks is
// one package with one receiver type, so a name is unambiguous here; resolving
// types would need go/types over a module whose generated SDK is not always
// present, and a guard that cannot run is a guard that gets deleted.
func selectorCallGraph(t *testing.T, dir string) map[string]map[string]bool {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatalf("glob %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	out := map[string]map[string]bool{}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		// Mode 0: comments are not attached, so a guard cannot match its own prose.
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			sels := map[string]bool{}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if s := renderSelector(call.Fun); s != "" {
					sels[s] = true
				}
				return true
			})
			out[fn.Name.Name] = sels
		}
	}
	if len(out) == 0 {
		t.Fatalf("parsed no functions out of %s — the scan is broken", dir)
	}
	return out
}

// renderSelector flattens a call's function expression into a dotted chain, and
// returns "" for anything that is not a plain identifier chain (an indexed call,
// a call on a call, a func literal).
func renderSelector(e ast.Expr) string {
	var parts []string
	for {
		switch v := e.(type) {
		case *ast.Ident:
			parts = append(parts, v.Name)
			for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
				parts[i], parts[j] = parts[j], parts[i]
			}
			return strings.Join(parts, ".")
		case *ast.SelectorExpr:
			parts = append(parts, v.Sel.Name)
			e = v.X
		default:
			return ""
		}
	}
}

// reachableSelectors returns every selector chain reachable from fn, following
// calls to other functions in the same package.
//
// An intra-package call is recognised by its LAST segment being a known function
// name — which covers both `s.admitAmendment(...)` and a bare `helper(...)`. The
// over-approximation that carries (a method on some OTHER type sharing a name
// with a package function) is bounded by non-vacuity arm 3: if it were widening
// the walk enough to matter, handleCancel would report controls it does not run.
func reachableSelectors(calls map[string]map[string]bool, fn string) map[string]bool {
	out := map[string]bool{}
	seen := map[string]bool{}
	var walk func(string)
	walk = func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		for sel := range calls[name] {
			out[sel] = true
			parts := strings.Split(sel, ".")
			if callee := parts[len(parts)-1]; callee != name {
				if _, ok := calls[callee]; ok {
					walk(callee)
				}
			}
		}
	}
	walk(fn)
	return out
}

// A REDUCTION IS THE ONE THING THAT MAY SKIP THE CONTROLS, AND ONLY THERE.
//
// admitAmendment lets a de-risking amend through unchecked — refusing it would
// leave the LARGER order resting, which is strictly the worse book, and it is the
// reasoning submit already gives for not re-checking a schedule's children and
// handleCancel gives for not being halt-gated.
//
// That carve-out is safe only while "reduces" is decided on BOTH factors every
// control sizes an order by. A quantity-only test would call 100 x 10 → 50 x 100
// a reduction and wave a fivefold increase through the hole this guard exists to
// close, and nothing else would notice: the amend applies, the outcome says
// EXECUTED, and the number is simply larger.
func TestTheAmendReductionCarveOutReadsBothPriceAndQuantity(t *testing.T) {
	root := moduleRoot(t)
	calls := selectorCallGraph(t, filepath.Join(root, filepath.FromSlash(omsOrderPkg)))

	sels, ok := calls["amendReducesExposure"]
	if !ok {
		t.Fatal("no amendReducesExposure in " + omsOrderPkg + " — the carve-out was renamed or " +
			"removed. If a reduction is no longer treated specially this guard should go with " +
			"it; if it is, this guard must be pointed at whatever decides it.")
	}

	// It must read the quantity AND the price. Both are compared through
	// internal/dec, which is the platform's only exact Decimal comparison — a
	// carve-out that reached for float or for raw coefficients would be wrong for
	// a different reason (1.00 is not larger than 10).
	var read []string
	for sel := range sels {
		if sel == "dec.Cmp" {
			continue
		}
		read = append(read, sel)
	}
	if !sels["dec.Cmp"] {
		sort.Strings(read)
		t.Fatalf("amendReducesExposure does not compare through dec.Cmp (it calls %v). Money and "+
			"quantities are common.v1.Decimal and only dec.Cmp compares them exactly across "+
			"exponents — a coefficient comparison makes 1.00 look larger than 10.", read)
	}

	body := funcBodyText(t, filepath.Join(root, filepath.FromSlash(omsOrderPkg), "amend.go"), "amendReducesExposure")
	for _, term := range []struct{ getter, why string }{
		{"GetOrderedQuantity", "an amend that raises the SIZE is the headline case of #799"},
		{"GetLimitPrice", "quantity x price is what every control sizes an order by, so a raised " +
			"limit is a raised commitment and 100 x 10 → 50 x 100 is not a reduction"},
	} {
		if !strings.Contains(body, term.getter) {
			t.Errorf("amendReducesExposure does not read %s — %s", term.getter, term.why)
		}
	}
}

// funcBodyText returns the source text of one top-level function's body, with
// comments stripped by the parser so a guard cannot match explanatory prose.
func funcBodyText(t *testing.T, path, name string) string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != name || fn.Body == nil {
			continue
		}
		var b strings.Builder
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok {
				b.WriteString(id.Name)
				b.WriteByte(' ')
			}
			return true
		})
		return b.String()
	}
	t.Fatalf("no func %s in %s", name, path)
	return ""
}

// ONLY TWO FUNCTIONS MAY WRITE ordered_quantity, AND BOTH ARE GATED (#799).
//
// This is the property the guards above are really protecting, stated at the
// field instead of at the handler. Today `Accept` sets it on admission (behind
// submit's controls) and `Amend` rewrites it (now behind admitAmendment's). A
// THIRD writer is how the hole reopens without either guard above noticing: a
// reconciler, a sweep, a healing path or a future replace could move an order's
// size with no control anywhere near it, and every test in this file would stay
// green because handleAmend would still be doing the right thing.
//
// DEFAULT-DENY, so the third writer has to be argued for rather than merely
// added. The exemption carries the reason and the issue that retires it, and the
// dead-entry arm keeps an exemption from outliving its repair.
var orderedQuantityWriters = map[string]string{
	"Accept": "admission: the quantity the order comes into existence with, behind submit's " +
		"halt gate, dual-control classification and pre-trade compliance check.",
	"Amend": "the amendment itself, behind admitAmendment's re-entry into the same three " +
		"controls (#799).",
}

func TestOnlyTheGatedPathsWriteOrderedQuantity(t *testing.T) {
	root := moduleRoot(t)
	dir := filepath.Join(root, filepath.FromSlash(omsOrderPkg))
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatalf("glob %s: %v", dir, err)
	}

	fset := token.NewFileSet()
	writers := map[string][]string{} // function name -> where
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}
		rel := filepath.Base(path)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch v := n.(type) {
				case *ast.AssignStmt:
					// x.OrderedQuantity = ...
					for _, lhs := range v.Lhs {
						if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "OrderedQuantity" {
							writers[fn.Name.Name] = append(writers[fn.Name.Name], rel)
						}
					}
				case *ast.KeyValueExpr:
					// OrderedQuantity: ... inside a composite literal
					if k, ok := v.Key.(*ast.Ident); ok && k.Name == "OrderedQuantity" {
						writers[fn.Name.Name] = append(writers[fn.Name.Name], rel)
					}
				}
				return true
			})
		}
	}

	// NON-VACUITY. Both known writers must be found, or the scan is looking for
	// a field spelling this tree does not use and would report an empty set as
	// a clean one.
	for want := range orderedQuantityWriters {
		if len(writers[want]) == 0 {
			t.Fatalf("the scan found no ordered_quantity write in %s — the field was renamed or "+
				"the aggregate moved, so this guard is asserting nothing", want)
		}
	}

	var unexcused []string
	for fn, where := range writers {
		if _, ok := orderedQuantityWriters[fn]; ok {
			continue
		}
		unexcused = append(unexcused, fn+" ("+strings.Join(uniq(where), ", ")+")")
	}
	if len(unexcused) > 0 {
		sort.Strings(unexcused)
		t.Errorf("these functions write ordered_quantity and are not known to be gated: %v.\n\n"+
			"An order's SIZE is the number every admission control was asked about. A path that "+
			"moves it without re-entering them is #799 reopened somewhere else — and the guards "+
			"above would stay green, because handleAmend would still be correct.\n\n"+
			"Either route the new writer through the controls (see admitAmendment) or add it to "+
			"orderedQuantityWriters with the reason it needs none and the issue that retires the "+
			"entry.", unexcused)
	}

	// DEAD-ENTRY ARM: an exemption that no longer matches a writer is a claim
	// nobody is checking any more.
	for fn, reason := range orderedQuantityWriters {
		if len(writers[fn]) == 0 {
			t.Errorf("exemption for %q (%s) matches no ordered_quantity writer — delete it", fn, reason)
		}
	}
}
