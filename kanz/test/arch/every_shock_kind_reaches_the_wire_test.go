package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"
)

// EVERY SHOCK KIND THE ENGINE CAN APPLY MUST BE ASKABLE FOR (#1004).
//
// # The gap this closes, which shipped and made a whole capability unreachable
//
// internal/risk/api/v1 declared four ScenarioShock implementors. The scenario
// engine applied all four. The factor classifier a SectorShock resolves against
// was constructed at the risk-engine composition root (#640/#695). A curated
// catalog of historical crises (GFC 2008, COVID 2020) and NGFS climate
// scenarios — every one of them a GICS SECTOR CURVE, because a crisis is not
// uniform: financials −55% against staples −15% in 2008 — was written and unit
// tested.
//
// And `query.v1.ScenarioShock` was a two-member oneof. There was no SectorShock
// and no VolShock on the wire and no case for either in the gRPC decode, whose
// default arm returned INVALID_ARGUMENT. So the richest stress an external
// caller could request was "drop everything N%", which cannot express dispersion
// and therefore tells a PM nothing gross exposure did not already say. Sector
// stress, climate stress and vol stress were all unaskable, and the catalog had
// zero production callers.
//
// NOTHING WAS BROKEN — every unit test passed, the engine was correct, the
// classifier was wired. Two hand-maintained copies of one taxonomy (the api/v1
// implementor set and the proto oneof) had simply drifted apart, and no signal
// on this estate compares them. That is why the repair is a guard and not a
// paragraph: the next shock type added to api/v1 fails the build until it can be
// requested, instead of being quietly inert.
//
// # What it compares, and why each leg is load-bearing
//
// The implementor set is DERIVED, not listed: the guard reads the ScenarioShock
// interface's method set out of the api/v1 AST and keeps every named type in
// that package whose methods cover it. A list written here would be a third copy
// of the taxonomy that drifted, which is the defect rather than the fix.
//
// For each implementor it requires three things, one per hop of the request:
//
//   - a member of the query.v1.ScenarioShock oneof, read from the compiled
//     PROTO DESCRIPTOR rather than the .proto text, so a member that exists in
//     the file but not in the generated SDK cannot satisfy it;
//   - a case in grpcsrv.apiShocks that CONSTRUCTS the api/v1 type — the decode.
//     A oneof member with no case here is refused by that switch's default arm,
//     which is the same unreachability with a different error message;
//   - a case in scenario.applyShock — the apply. A case here makes the shock
//     RECOGNIZED rather than dropped by the switch's silent default.
//
// THIS GUARD ONLY PROVES THE CASE EXISTS. It used to record that VolShock's arm
// was "deliberately empty" and that an empty case "is exactly the point", and
// that reading is what let a recognized-but-inert shock ship: recognized is not
// answered, and the caller could not tell the two apart (#1035). Whether an arm
// DOES anything is the sibling guard's question —
// every_shock_kind_answers_or_refuses_test.go.
//
// It also checks the reverse direction. A oneof member with no api/v1
// implementor is a field a caller can populate and the engine can never act on
// — a request that looks accepted and shocks nothing, which is the failure this
// estate treats as the worst one.
//
// # Empty exemption list, deliberately
//
// Surveyed when written: four implementors, four oneof members, four decode
// cases, four apply cases — full parity, with nothing to grandfather. The first
// entry anyone needs to add is a conversation about why a shock type the engine
// can apply must stay unaskable, not a formality.
func TestEveryShockKindReachesTheWire(t *testing.T) {
	root := moduleRoot(t)

	implementors := scenarioShockImplementors(t, root)
	// NON-VACUITY (1/4): a scanner that finds no implementors passes every
	// assertion below while proving nothing. Four exist today.
	if len(implementors) < 4 {
		t.Fatalf("found %d v1.ScenarioShock implementors in internal/risk/api/v1 (%v), want at "+
			"least 4 — the extractor is broken or the types moved, and this guard is comparing "+
			"against nothing", len(implementors), implementors)
	}

	onWire := scenarioShockOneofMembers(t)
	// NON-VACUITY (2/4): an empty oneof would make the wire leg vacuously
	// unsatisfiable rather than vacuously satisfied, but a nil map from a renamed
	// oneof would report every implementor as missing and bury the real cause.
	if len(onWire) == 0 {
		t.Fatal("query.v1.ScenarioShock declares no oneof named \"shock\" — the message was " +
			"renamed or restructured, and this guard cannot see the wire it is checking")
	}

	decoded := shockTypesConstructedIn(t,
		filepath.Join(root, "services", "risk-engine", "internal", "grpcsrv", "server.go"),
		"apiShocks")
	// NON-VACUITY (3/4): the decode switch must be found. A renamed or moved
	// apiShocks yields an empty set, which would fail loudly here rather than
	// report four spurious gaps.
	if len(decoded) == 0 {
		t.Fatal("grpcsrv.apiShocks constructs no api/v1 shock types — the decode moved or was " +
			"renamed, and this guard is not reading the decode it names")
	}

	applied := shockTypesSwitchedOn(t,
		filepath.Join(root, "internal", "risk", "scenario", "scenario.go"),
		"applyShock")
	// NON-VACUITY (4/4): same, for the apply switch.
	if len(applied) == 0 {
		t.Fatal("scenario.applyShock switches on no api/v1 shock types — the apply dispatch " +
			"moved or was renamed, and this guard is not reading the apply it names")
	}

	for _, name := range implementors {
		if reason, ok := shockKindExemptions[name]; ok {
			t.Logf("exempt: %s — %s", name, reason)
			continue
		}
		if !onWire[name] {
			t.Errorf("v1.%s has no query.v1.ScenarioShock oneof member.\n"+
				"The engine can apply it and NO CALLER CAN ASK FOR IT — the decode's default arm "+
				"returns INVALID_ARGUMENT for a shock the engine was fully able to evaluate. That "+
				"is how sector and vol stress, and with them every named historical and climate "+
				"scenario, were unreachable for as long as they existed (#1004).\n"+
				"Add `%s <snake_case> = <next>;` to the oneof in "+
				"kanz-schemas/proto/query/v1/risk_query.proto (additive, so buf breaking passes) "+
				"and regenerate the SDK.", name, name)
		}
		if !decoded[name] {
			t.Errorf("grpcsrv.apiShocks has no case constructing v1.%s.\n"+
				"A oneof member the decode does not handle falls to the default arm and is "+
				"refused, so putting it on the wire changed the error message and nothing else.\n"+
				"Add the case in services/risk-engine/internal/grpcsrv/server.go.", name)
		}
		if !applied[name] {
			t.Errorf("scenario.applyShock has no case for v1.%s.\n"+
				"applyShock's switch has no default, so an unhandled shock is silently dropped: "+
				"the request succeeds, the projection comes back, and the shock the caller asked "+
				"for never moved a number.\n"+
				"Add the case in internal/risk/scenario/scenario.go — and note that an EMPTY case "+
				"satisfies this guard and not the sibling one: an arm must move the book or record "+
				"on the coverage (every_shock_kind_answers_or_refuses_test.go).", name)
		}
	}

	// THE OTHER DIRECTION. A oneof member no api/v1 type implements is a field a
	// caller can set on a request the server accepts and the engine can never act
	// on — accepted, forwarded, and neither honoured nor refused.
	declared := map[string]bool{}
	for _, name := range implementors {
		declared[name] = true
	}
	var orphaned []string
	for name := range onWire {
		if !declared[name] {
			orphaned = append(orphaned, name)
		}
	}
	if len(orphaned) > 0 {
		sort.Strings(orphaned)
		t.Errorf("query.v1.ScenarioShock oneof member(s) %v have no v1.ScenarioShock implementor "+
			"— a caller can populate them and the engine has nothing to apply, so the request "+
			"looks accepted and shocks nothing", orphaned)
	}

	// DEAD-ENTRY ARM: an exemption naming a type that no longer implements
	// v1.ScenarioShock outlives its repair and silently widens the next one.
	for name, reason := range shockKindExemptions {
		if !declared[name] {
			t.Errorf("exemption for %s (%s) names no v1.ScenarioShock implementor — remove it; "+
				"a stale exemption is a hole waiting for a type of the same name", name, reason)
		}
	}
}

// shockKindExemptions maps an api/v1 shock type name to the issue that will
// retire its exemption. EMPTY, and it lands empty: at the time this guard was
// written every implementor had a oneof member, a decode case and an apply case.
var shockKindExemptions = map[string]string{}

// scenarioShockImplementors returns the names of the types in
// internal/risk/api/v1 that satisfy the v1.ScenarioShock interface, derived from
// the interface's own method set rather than from a list kept here.
func scenarioShockImplementors(t *testing.T, root string) []string {
	t.Helper()
	dir := filepath.Join(root, "internal", "risk", "api", "v1")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	fset := token.NewFileSet()

	var want []string
	methods := map[string]map[string]bool{}
	for _, entry := range entries {
		fileName := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(fileName, ".go") || strings.HasSuffix(fileName, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, fileName), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", fileName, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.TypeSpec:
				if node.Name.Name != "ScenarioShock" {
					return true
				}
				iface, ok := node.Type.(*ast.InterfaceType)
				if !ok {
					return true
				}
				for _, m := range iface.Methods.List {
					for _, method := range m.Names {
						want = append(want, method.Name)
					}
				}
			case *ast.FuncDecl:
				if node.Recv == nil || len(node.Recv.List) == 0 {
					return true
				}
				// receiverTypeName is the shared helper in decimal_grpc_domain_test.go —
				// same question, one implementation.
				recv := receiverTypeName(node.Recv.List[0].Type)
				if recv == "" {
					return true
				}
				if methods[recv] == nil {
					methods[recv] = map[string]bool{}
				}
				methods[recv][node.Name.Name] = true
			}
			return true
		})
	}
	if len(want) == 0 {
		t.Fatal("no ScenarioShock interface with methods found in internal/risk/api/v1 — the " +
			"contract moved, and every type would look like an implementor")
	}

	var out []string
	for typ, have := range methods {
		covers := true
		for _, m := range want {
			if !have[m] {
				covers = false
				break
			}
		}
		if covers {
			out = append(out, typ)
		}
	}
	sort.Strings(out)
	return out
}

// scenarioShockOneofMembers returns the message names in the query.v1
// ScenarioShock "shock" oneof, read from the COMPILED DESCRIPTOR. Reading the
// .proto text instead would accept a member that exists in the file but not in
// the SDK every caller is generated against.
func scenarioShockOneofMembers(t *testing.T) map[string]bool {
	t.Helper()
	oneof := (&querypb.ScenarioShock{}).ProtoReflect().Descriptor().Oneofs().ByName("shock")
	if oneof == nil {
		return nil
	}
	out := map[string]bool{}
	fields := oneof.Fields()
	for i := 0; i < fields.Len(); i++ {
		if msg := fields.Get(i).Message(); msg != nil {
			out[string(msg.Name())] = true
		}
	}
	return out
}

// shockTypesConstructedIn returns the api/v1 type names built as composite
// literals inside the named function — the decode's own record of which shocks
// it can produce.
func shockTypesConstructedIn(t *testing.T, path, fn string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	forEachNodeIn(t, path, fn, func(n ast.Node) {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return
		}
		if name := apiV1TypeName(lit.Type); name != "" {
			out[name] = true
		}
	})
	return out
}

// shockTypesSwitchedOn returns the api/v1 type names named by the type-switch
// cases inside the named function — the apply dispatch's own record.
func shockTypesSwitchedOn(t *testing.T, path, fn string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	forEachNodeIn(t, path, fn, func(n ast.Node) {
		clause, ok := n.(*ast.CaseClause)
		if !ok {
			return
		}
		for _, expr := range clause.List {
			if name := apiV1TypeName(expr); name != "" {
				out[name] = true
			}
		}
	})
	return out
}

// forEachNodeIn parses one file, locates the named top-level function, and walks
// its body. A missing function is fatal: a guard that silently reads nothing is
// the shape this whole file exists to prevent.
func forEachNodeIn(t *testing.T, path, fn string, visit func(ast.Node)) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	for _, decl := range file.Decls {
		f, ok := decl.(*ast.FuncDecl)
		if !ok || f.Name.Name != fn || f.Body == nil {
			continue
		}
		ast.Inspect(f.Body, func(n ast.Node) bool {
			if n != nil {
				visit(n)
			}
			return true
		})
		return
	}
	t.Fatalf("no func %s in %s — it was renamed or moved, and this guard reads nothing", fn, path)
}

// apiV1TypeName returns the selector name of a `v1.X` expression, or "" for
// anything else. The AST is the source, so a shock type named only in a comment
// or an error string cannot satisfy any leg of this guard.
func apiV1TypeName(expr ast.Expr) string {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "v1" {
		return ""
	}
	return sel.Sel.Name
}
