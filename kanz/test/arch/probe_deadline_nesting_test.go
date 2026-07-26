package arch

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
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

// tuiClientFile builds the TUI's one HTTP client; every Test Connection travels on it.
const tuiClientFile = "cmd/universe/gatewaysource.go"

// TestTUIHTTPClientCarriesNoTimeoutOfItsOwn closes the hole the test above cannot see.
//
// The ordering assertion reads four CONSTANTS. But http.Client.Timeout is enforced
// independently of the request context, so a client-level timeout is a FIFTH bound on the
// same call, and the effective limit is min(context deadline, client timeout). It won
// silently: the TUI's client carried a 30s timeout (a flag default, invisible to a test
// that parses consts) underneath the operator's 60s probe wait, so probeTimeout could
// never fire and every slow Test Connection died client-side with "Client.Timeout exceeded
// while awaiting headers" — a message about the gateway and the network, printed while
// probing a host that may have been perfectly healthy.
//
// So the rule is not "keep the client timeout above the others". It is that this call has
// exactly ONE bound: the context its caller sets. A second one cannot be ordered against
// the four consts, because it does not live with them.
func TestTUIHTTPClientCarriesNoTimeoutOfItsOwn(t *testing.T) {
	path := filepath.Join(moduleRoot(t), filepath.FromSlash(tuiClientFile))

	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	var clients int
	ast.Inspect(f, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.CompositeLit:
			if !isHTTPClientType(v.Type) {
				return true
			}
			clients++
			for _, elt := range v.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Timeout" {
					t.Errorf("%s constructs its http.Client with a Timeout field.\n\n"+
						"That bound is enforced independently of the request context, so every call "+
						"on this client is bounded by min(context, this) and the SMALLER wins "+
						"invisibly. If it lands under provision.probeTimeout (60s), Test Connection "+
						"dies client-side with a message about the gateway while probing a host that "+
						"may be healthy — and no ordering test over the four consts can see it, "+
						"because this bound does not live with them. Bound the CALL instead: pass a "+
						"context deadline (Config.CallTimeout for reads, testConnTimeout for the "+
						"probe).", tuiClientFile)
				}
			}
		case *ast.AssignStmt:
			// The same bound, spelled as a later field write rather than in the literal.
			for _, lhs := range v.Lhs {
				sel, ok := lhs.(*ast.SelectorExpr)
				if ok && sel.Sel.Name == "Timeout" {
					t.Errorf("%s assigns a .Timeout field after construction — see this test's "+
						"failure text for why the client must carry no bound of its own",
						tuiClientFile)
				}
			}
		}
		return true
	})

	if clients == 0 {
		t.Fatalf("%s no longer constructs an http.Client literal, so this guard asserted "+
			"nothing. If the client moved, point tuiClientFile at its new home; if the TUI now "+
			"uses a client built elsewhere, that client needs this same rule", tuiClientFile)
	}
}

// ingressFile publishes the gateway to the public internet; in production every Test
// Connection the TUI makes travels through it.
const ingressFile = "infra/deploy/api-gateway-ingress.yaml"

// ingressProxyTimeoutAnnotations are the two nginx bounds on a proxied request. Both
// default to 60s when absent, which is why absence — not just a small value — has to
// fail this test.
var ingressProxyTimeoutAnnotations = []string{
	"nginx.ingress.kubernetes.io/proxy-read-timeout",
	"nginx.ingress.kubernetes.io/proxy-send-timeout",
}

// ingressDoc projects the one field this guard reads from the Ingress manifest, in the
// same yaml.v3 style as operator_placement_test.go.
type ingressDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name        string            `yaml:"name"`
		Annotations map[string]string `yaml:"annotations"`
	} `yaml:"metadata"`
}

// TestIngressProxyTimeoutsExceedTheTUIBound closes the same hole as the test above, one
// layer further out — and this one is not even written in Go.
//
// The ordering test reads four constants; the client test forbids a fifth bound inside
// the TUI's HTTP client. Neither can see the SIXTH: in production the TUI does not talk
// to the gateway Service, it talks to this Ingress, and ingress-nginx bounds a proxied
// request at proxy-read-timeout / proxy-send-timeout — DEFAULT 60s. That is at or under
// three of the four Go bounds, and nginx starts its clock when IT proxies, before the
// operator's probeTimeout starts. So on a slow probe nginx answers 504 first and the TUI
// prints a gateway/network error while probing a host that may be healthy — verbatim the
// misdiagnosis the nesting exists to prevent, and invisible to every other guard here
// because the bound lives in YAML.
//
// ABSENCE MUST FAIL, NOT JUST A SMALL VALUE. Deleting the annotation does not remove the
// bound, it restores the 60s default — the bug itself. A guard that only checked the
// number when present would pass on the one edit most likely to reintroduce it.
func TestIngressProxyTimeoutsExceedTheTUIBound(t *testing.T) {
	root := moduleRoot(t)

	// The bound to beat is read from source, not copied: a hardcoded 100s here would go
	// stale the moment testConnTimeout is tuned, which is exactly when this matters.
	tuiBound := durationConst(t,
		filepath.Join(root, filepath.FromSlash("cmd/universe/poller.go")), "testConnTimeout")

	path := filepath.Join(root, filepath.FromSlash(ingressFile))
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var found bool
	dec := yaml.NewDecoder(strings.NewReader(string(body)))
	for {
		var d ingressDoc
		derr := dec.Decode(&d)
		if errors.Is(derr, io.EOF) {
			break
		}
		if derr != nil {
			t.Fatalf("parse %s: %v", path, derr)
		}
		if d.Kind != "Ingress" || d.Metadata.Name != "api-gateway" {
			continue
		}
		found = true

		for _, key := range ingressProxyTimeoutAnnotations {
			raw, ok := d.Metadata.Annotations[key]
			if !ok {
				t.Errorf("%s carries no %s annotation.\n\n"+
					"That does NOT mean the request is unbounded at the edge — it means "+
					"ingress-nginx applies its 60s DEFAULT, which is under testConnTimeout (%s) "+
					"and under the operator's own probe wait, and whose clock starts before "+
					"theirs. Every slow Test Connection then dies as a 504 from nginx and the "+
					"TUI blames the gateway or the network while probing a host that may be "+
					"perfectly healthy. Restore it above %s.", ingressFile, key, tuiBound, tuiBound)
				continue
			}
			secs, perr := strconv.Atoi(strings.TrimSpace(raw))
			if perr != nil {
				t.Errorf("%s sets %s to %q, which is not a whole number of seconds. nginx "+
					"rejects a malformed value and falls back to its 60s default, so this "+
					"reads as a bound that is set and is not.", ingressFile, key, raw)
				continue
			}
			if got := time.Duration(secs) * time.Second; got <= tuiBound {
				t.Errorf("%s sets %s = %s, which does not exceed the TUI's testConnTimeout "+
					"(%s, cmd/universe/poller.go).\n\n"+
					"nginx would cut the connection before the TUI's own bound expires, and "+
					"before that the operator's probeTimeout — so the caller gets a 504 about "+
					"the edge instead of the probe verdict only the operator can give. Raise "+
					"this annotation rather than lowering testConnTimeout: the inner bounds are "+
					"sized to real work (Job create, pod scheduling, image pull, a 10s dial).",
					ingressFile, key, got, tuiBound)
			}
		}
	}

	// Non-vacuity: a guard that matches no document passes forever once the Ingress is
	// renamed or the manifest moved.
	if !found {
		t.Fatalf("no Ingress named api-gateway found in %s — if it was renamed or split out, "+
			"point ingressFile at its new home; the edge bound it carries is the outermost "+
			"deadline on every Test Connection in production", ingressFile)
	}
}

// isHTTPClientType reports whether a composite-literal type is http.Client, including
// through the `&http.Client{...}` the code actually writes.
func isHTTPClientType(e ast.Expr) bool {
	if star, ok := e.(*ast.StarExpr); ok {
		e = star.X
	}
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Client" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "http"
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
