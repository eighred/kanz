package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

// THE GATEWAY'S CAPABILITY MUX IS BUILT WITH A REAL RECORDER (AUTH-01d, #352).
//
// authz.NewMux takes the recorder as a REQUIRED parameter, so a composition root
// cannot forget it — that much the compiler holds. What the compiler cannot hold
// is the value: `authz.NewMux(grants, nil)` compiles perfectly, serves every
// request correctly, and records nothing. nil is the shape the package's own
// tests use, so it is not exotic — it is one autocomplete away.
//
// WHY THAT SPECIFIC MISTAKE MATTERS HERE. The gateway is the platform's sole
// identity authority. Before #352 it recorded its allows and denies NOWHERE, not
// even to a log, and the symptom of that was nothing at all: every request
// behaved exactly as it should. There is no failing probe, no error, no 500 —
// the only observable difference is an audit trail that is silently empty, and
// nobody looks at an audit trail until they need it, by which time the decisions
// are months gone.
//
// So this asserts the one thing the type system leaves open: that the argument
// at the composition root is not the zero value.
func TestTheGatewayMuxIsBuiltWithADecisionRecorder(t *testing.T) {
	path := filepath.Join(moduleRoot(t), filepath.FromSlash(
		"services/api-gateway/cmd/api-gateway/main.go"))

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse api-gateway main.go: %v", err)
	}

	var calls []*ast.CallExpr
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "NewMux" {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "authz" {
			calls = append(calls, call)
		}
		return true
	})

	// NON-VACUITY: no call means the guard matches nothing and would stay green
	// through a rename or a move of the composition root.
	if len(calls) == 0 {
		t.Fatal("no authz.NewMux call in the api-gateway composition root.\n\n" +
			"Either the gateway no longer builds its capability mux here, or it was renamed. " +
			"This guard is now asserting nothing — repoint it before assuming the property holds.")
	}

	for _, call := range calls {
		pos := fset.Position(call.Pos())
		if len(call.Args) < 2 {
			t.Errorf("authz.NewMux at %s takes %d argument(s) — the recorder is missing",
				pos, len(call.Args))
			continue
		}
		if id, ok := call.Args[1].(*ast.Ident); ok && id.Name == "nil" {
			t.Errorf("authz.NewMux at %s is built with a NIL decision recorder.\n\n"+
				"The gateway is the platform's sole identity authority. With nil it serves every "+
				"request correctly and records no authorization decision anywhere — the exact "+
				"state #352 repaired, and one with no symptom until somebody needs the trail.\n\n"+
				"Pass the recorder from buildDecisionRecorder, which falls back to slog when "+
				"there is no bus and is therefore never a no-op.", pos)
		}
	}
}
