package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// DEADLINES ON A TEST CONNECTION MUST NEST OUTWARD. This test is that rule, executable.
//
// One Add Node "Test Connection" is bounded four times over, by four constants living in
// four packages that do not import each other:
//
//	universe TUI          testConnTimeout        cmd/universe/poller.go
//	api-gateway route     testConnectionTimeout  services/api-gateway/internal/control
//	operator probe wait   probeTimeout           services/operator/internal/provision
//	provisioner dial      probeDialTimeout       cmd/kanz-provisioner/probe.go
//
// Each must strictly exceed the one beneath it, so the INNERMOST bound is the one that
// fires. That ordering is not tidiness — it decides what a human is told. Only the
// operator can say "probe job did not complete in time"; only the provisioner's dial can
// distinguish "refused" from "silently dropped", which is the entire reason the probe
// exists. Invert the order and every verdict degrades into the outermost layer's generic
// deadline error, identical for a black-holed host and a healthy one.
//
// This is not hypothetical. Before OPS-M2e the probe was a sub-millisecond in-process
// dial and the TUI's 5s bound was ample; moving it into an ephemeral Kubernetes Job (Job
// create + pod schedule + possible image pull + a 10s dial) made the innermost bound the
// largest, and EVERY Test Connection failed at the outermost layer — including against a
// healthy host.
//
// WHY THIS LIVES IN test/arch. No single package can see all four constants, and none
// should: the TUI must not import the operator's internals. A comment in each file
// claiming the ordering is not a guard — four unexported consts in four packages drift
// the first time one of them is tuned in isolation.
//
// WHY IT PARSES SOURCE. The constants are unexported, so they cannot be read by import,
// and go/ast keeps this test from being a fifth place the values are written down — a
// hardcoded copy here would go stale exactly as silently as the comments it replaces.

// deadlineLayer is one bound in the chain, outermost first.
type deadlineLayer struct {
	name  string // the const's identifier
	file  string // repo-relative file that declares it
	label string // what it bounds, for the failure message
}

var testConnDeadlineChain = []deadlineLayer{
	{"testConnTimeout", "cmd/universe/poller.go", "the TUI's wait on the gateway"},
	{"testConnectionTimeout", "services/api-gateway/internal/control/control.go",
		"the gateway's wait on the operator"},
	{"probeTimeout", "services/operator/internal/provision/provision.go",
		"the operator's wait on the probe Job"},
	{"probeDialTimeout", "cmd/kanz-provisioner/probe.go", "the probe's own TCP dial"},
}

func TestTestConnectionDeadlinesNestOutward(t *testing.T) {
	root := moduleRoot(t)

	got := make([]time.Duration, len(testConnDeadlineChain))
	for i, layer := range testConnDeadlineChain {
		got[i] = durationConst(t, filepath.Join(root, filepath.FromSlash(layer.file)), layer.name)
	}

	for i := 0; i+1 < len(testConnDeadlineChain); i++ {
		outer, inner := testConnDeadlineChain[i], testConnDeadlineChain[i+1]
		if got[i] <= got[i+1] {
			t.Errorf("%s (%s, %s) = %s does NOT exceed %s (%s, %s) = %s.\n\n"+
				"The outer bound fires first, so %s can never report what it alone knows — the "+
				"caller gets a generic upstream deadline instead, and cannot tell a firewalled "+
				"host from a healthy one. Raise %s rather than lowering %s: the inner bounds are "+
				"sized to real work (Job create, pod scheduling, image pull, a 10s dial).",
				outer.name, outer.file, outer.label, got[i],
				inner.name, inner.file, inner.label, got[i+1],
				inner.label, outer.name, inner.name)
		}
	}
}

// durationConst reads one `const name = N * time.Unit` declaration out of a file.
//
// It FAILS rather than returning zero when the const is missing or is not a literal
// duration product: a renamed or restructured const must break this test loudly, since a
// silent zero would make the ordering assertions above pass while asserting nothing.
func durationConst(t *testing.T, path, name string) time.Duration {
	t.Helper()

	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	var found time.Duration
	var seen bool
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) != 1 || len(vs.Values) != 1 || vs.Names[0].Name != name {
				continue
			}
			d, derr := evalDurationExpr(vs.Values[0])
			if derr != "" {
				t.Fatalf("%s declares %s but this test cannot read it: %s — keep it a literal "+
					"`N * time.Unit` product, or teach evalDurationExpr the new shape",
					path, name, derr)
			}
			found, seen = d, true
		}
	}
	if !seen {
		t.Fatalf("%s no longer declares a const %q — it is one of the four bounds on a Test "+
			"Connection (see this file's header). If it was renamed, rename it in "+
			"testConnDeadlineChain too; if it was deleted, that layer is now unbounded",
			path, name)
	}
	return found
}

// evalDurationExpr evaluates `N * time.Unit` (in either order). It returns a reason
// instead of an error value because the only caller turns it straight into a t.Fatalf.
func evalDurationExpr(e ast.Expr) (time.Duration, string) {
	be, ok := e.(*ast.BinaryExpr)
	if !ok || be.Op != token.MUL {
		return 0, "not a multiplication"
	}
	var n int64
	var unit time.Duration
	for _, side := range []ast.Expr{be.X, be.Y} {
		switch v := side.(type) {
		case *ast.BasicLit:
			if v.Kind != token.INT {
				return 0, "non-integer literal " + v.Value
			}
			parsed, err := strconv.ParseInt(v.Value, 0, 64)
			if err != nil {
				return 0, "unparseable literal " + v.Value
			}
			n = parsed
		case *ast.SelectorExpr:
			pkg, ok := v.X.(*ast.Ident)
			if !ok || pkg.Name != "time" {
				return 0, "selector is not from package time"
			}
			u, ok := timeUnits[v.Sel.Name]
			if !ok {
				return 0, "unknown time unit " + v.Sel.Name
			}
			unit = u
		default:
			return 0, "unsupported operand"
		}
	}
	if n == 0 || unit == 0 {
		return 0, "not a literal-times-unit product"
	}
	return time.Duration(n) * unit, ""
}

var timeUnits = map[string]time.Duration{
	"Nanosecond":  time.Nanosecond,
	"Microsecond": time.Microsecond,
	"Millisecond": time.Millisecond,
	"Second":      time.Second,
	"Minute":      time.Minute,
	"Hour":        time.Hour,
}
