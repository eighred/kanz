package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"testing"
)

// THE PRE-TRADE GATE RECORDS ON ONE EXIT, AND DECIDES ON ANOTHER (#797).
//
// # What this is protecting
//
// PreTradeGate.Evaluate had ten terminal returns and exactly ONE g.record —
// on the full-evaluation path. Nine paths recorded nothing, and TWO of those
// returned Allowed: true: the ungoverned portfolio and the mandate that
// constrains nothing. For an order admitted that way the platform could not
// answer CLAUDE.md's attributability requirement — "which mandate permitted it"
// — because nothing said a compliance decision had been made at all.
//
// The type's own doc had asserted "Every decision is recorded (COMP-01e), pass
// or reject" the whole time. It had ALREADY been corrected once, in 2026-08-24,
// for a different reason (the nil recorder), and was still false.
//
// # Why the guard is structural rather than a count
//
// Adding g.record to nine call sites fixes today's nine and says nothing about
// the tenth. The repair splits the function: decide() owns every terminal
// return, Evaluate owns the only record. A new short-circuit then cannot skip
// the audit trail without being written in the wrong function — which is a thing
// a guard can see, where "somebody forgot a call" is not.
//
// So this asserts the SHAPE:
//
//   - Evaluate contains no Decision literal of its own — every verdict comes
//     from decide();
//   - Evaluate calls g.record exactly once;
//   - decide() calls g.record NOT AT ALL, or the split has quietly collapsed
//     back into the arrangement that produced this defect;
//   - decide() still has many terminal returns, so the split is real rather
//     than a rename of a function that no longer branches.
//
// # Why it reads the AST with comments detached
//
// Three guards in this tree have already passed while asserting nothing,
// because a regex over raw source matched their own explanatory prose. The
// paragraph you are reading cannot satisfy anything below.

// gateFile is the pre-trade enforcement point.
const gateFile = "internal/compliance/gate.go"

// recordCall is the selector the audit write wears.
const recordCall = "g.record"

func TestThePreTradeGateRecordsOnItsOnlyExit(t *testing.T) {
	root := moduleRoot(t)
	path := filepath.Join(root, filepath.FromSlash(gateFile))

	fset := token.NewFileSet()
	// Mode 0: comments are not attached.
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", gateFile, err)
	}

	bodies := map[string]*ast.BlockStmt{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil || fn.Recv == nil {
			continue
		}
		bodies[fn.Name.Name] = fn.Body
	}

	// NON-VACUITY 1: both halves exist. A rename or a merge would otherwise leave
	// every assertion below checking an absent function and passing.
	for _, name := range []string{"Evaluate", "decide"} {
		if bodies[name] == nil {
			t.Fatalf("no method %s in %s — the gate was restructured and this guard is asserting "+
				"nothing. If the split was undone, #797's defect is back: nine of ten terminal "+
				"paths recorded no compliance decision, two of them while ADMITTING the order.",
				name, gateFile)
		}
	}

	// NON-VACUITY 2: decide() still branches. A "split" whose decision half has
	// one return would mean the shape moved without the property moving with it.
	if n := decisionReturnsIn(bodies["decide"]); n < 5 {
		t.Fatalf("decide() has %d Decision returns — expected the many short-circuits this guard "+
			"exists for. Either they moved somewhere this guard does not read, or the function "+
			"is no longer the one that decides.", n)
	}

	// THE ASSERTIONS.
	if n := decisionReturnsIn(bodies["Evaluate"]); n != 0 {
		t.Errorf("Evaluate builds %d Decision(s) of its own.\n\n"+
			"Every verdict must come out of decide(), because Evaluate is the only place the "+
			"decision is RECORDED — a Decision constructed here is one that returns to the caller "+
			"having skipped the audit trail, which is exactly #797.", n)
	}

	if n := callsTo(bodies["Evaluate"], recordCall); n != 1 {
		t.Errorf("Evaluate calls %s %d time(s), want exactly 1.\n\n"+
			"One call on one exit is what makes 'every terminal decision is recorded' a property "+
			"of the SHAPE rather than of somebody remembering. Zero means admissions are silent "+
			"again; more than one means a decision can be filed twice, and a duplicated record in "+
			"an audit trail is a second decision that never happened.", recordCall, n)
	}

	if n := callsTo(bodies["decide"], recordCall); n != 0 {
		t.Errorf("decide() calls %s %d time(s).\n\n"+
			"The split has collapsed back into the arrangement that produced #797: recording "+
			"beside SOME returns, which is indistinguishable at a glance from recording beside "+
			"all of them. The record belongs on Evaluate's single exit.", recordCall, n)
	}
}

// EVERY "NO RULE RAN" FLAG MUST BE NAMED BY THE REASON SWITCH (#797).
//
// A flag the switch does not name falls into the UNCLASSIFIED arm. That arm is
// deliberate — an audit trail that answers confidently and wrongly is worse than
// one that says it does not know — but a SHIPPED path reaching it means somebody
// added a short-circuit and no reason for it, and the record then says
// "UNCLASSIFIED" to the reviewer who needed the one thing it could not tell them.
//
// The unit table in internal/compliance pins each flag's code. This pins that no
// flag exists WITHOUT one, which is the half a table of known flags cannot check
// about itself.
func TestEveryNotEvaluatedFlagIsNamedByTheReasonSwitch(t *testing.T) {
	root := moduleRoot(t)
	path := filepath.Join(root, filepath.FromSlash(gateFile))

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", gateFile, err)
	}

	// The bool fields on Decision, minus the two that are not "no rule ran".
	notReasons := map[string]bool{
		// Allowed is the verdict, not a reason.
		"Allowed": true,
		// Unaccounted rides an order the mandate DID evaluate — it says the cash
		// figure was unvouched, not that nothing was checked.
		"Unaccounted": true,
	}
	var flags []string
	ast.Inspect(file, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != "Decision" {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			return false
		}
		for _, f := range st.Fields.List {
			id, ok := f.Type.(*ast.Ident)
			if !ok || id.Name != "bool" {
				continue
			}
			for _, name := range f.Names {
				if !notReasons[name.Name] {
					flags = append(flags, name.Name)
				}
			}
		}
		return false
	})

	// NON-VACUITY: the struct scan found the flags at all.
	if len(flags) < 5 {
		t.Fatalf("found %d reason flag(s) on Decision (%v) — the type moved or its fields were "+
			"renamed, and this guard is comparing an empty set", len(flags), flags)
	}

	var body *ast.BlockStmt
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "notEvaluatedCode" {
			body = fn.Body
		}
	}
	if body == nil {
		t.Fatal("no notEvaluatedCode in " + gateFile + " — the reason for a short-circuited " +
			"decision is no longer derived anywhere this guard can read")
	}
	named := selectorNamesIn(body)

	var unnamed []string
	for _, f := range flags {
		if !named[f] {
			unnamed = append(unnamed, f)
		}
	}
	if len(unnamed) > 0 {
		sort.Strings(unnamed)
		t.Errorf("these Decision flags are not named by notEvaluatedCode: %v.\n\n"+
			"A decision carrying one reaches the audit trail as %q, so the record says a rule was "+
			"not evaluated and cannot say which condition stopped it — to the reviewer who is "+
			"holding one order id and asking exactly that. Add an arm and a code.",
			unnamed, "UNCLASSIFIED")
	}
}

// decisionReturnsIn counts `return Decision{...}` literals in a body.
func decisionReturnsIn(body *ast.BlockStmt) int {
	n := 0
	ast.Inspect(body, func(node ast.Node) bool {
		ret, ok := node.(*ast.ReturnStmt)
		if !ok {
			return true
		}
		for _, r := range ret.Results {
			if lit, ok := r.(*ast.CompositeLit); ok {
				if id, ok := lit.Type.(*ast.Ident); ok && id.Name == "Decision" {
					n++
				}
			}
		}
		return true
	})
	return n
}

// callsTo counts calls whose rendered selector chain equals sel.
func callsTo(body *ast.BlockStmt, sel string) int {
	n := 0
	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if renderSelector(call.Fun) == sel {
			n++
		}
		return true
	})
	return n
}
