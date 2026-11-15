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

// deadlineLayer is one bound in the chain, outermost first.
type deadlineLayer struct {
	name  string // the const's identifier
	file  string // repo-relative file that declares it
	label string // what it bounds, for the failure message

	// consequence and remedy override the default wording of a nesting failure,
	// which describes a CALLER's budget being outlived by the server it calls. Not
	// every nested bound is a caller's: serviceStatementTimeout (#228) is a bound
	// INSIDE the handler, and for that one the default advice — "raise the outer
	// bound, the inner is sized to real work" — is backwards. A guard that fires
	// with the wrong instruction sends the next reader to widen the thing that was
	// protecting them. Empty means the default.
	consequence string
	remedy      string
}

// Read all three backend bounds from their owners; outer deadlines must exceed inner work.
var testConnDeadlineChain = []deadlineLayer{
	{name: "testConnectionTimeout", file: "services/api_gateway/internal/control/control.go",
		label: "the gateway's wait on the operator"},
	{name: "probeTimeout", file: "services/operator/internal/provision/provision.go",
		label: "the operator's wait on the probe Job"},
	{name: "probeDialTimeout", file: "cmd/kanz_provisioner/probe.go",
		label: "the probe's own TCP dial"},
}

func TestTestConnectionDeadlinesNestOutward(t *testing.T) {
	root := moduleRoot(t)
	if len(testConnDeadlineChain) != 3 {
		t.Fatal("gateway, operator and probe must all be covered")
	}

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

const gatewayClientFile = "cmd/kanz_lake_snapshot/internal/gatewayclient/client.go"

func TestLakeSnapshotHTTPClientUsesCallerDeadline(t *testing.T) {
	sites := httpClientTimeoutSites(t, filepath.Join(moduleRoot(t), filepath.FromSlash(gatewayClientFile)))
	if sites.inLiteral > 0 || sites.byAssignment > 0 {
		t.Fatal("gateway client must preserve its caller's context deadline")
	}
	if sites.clients == 0 {
		t.Fatal("gateway client construction not found; update the guard's source path")
	}
}

// gatewayProxyClientFile builds the client every PROXIED request travels on: the
// api-gateway's forwarder to wealth, datamaster, tv-sync and copilot.
const gatewayProxyClientFile = "services/api_gateway/cmd/api_gateway/main.go"

func TestGatewayProxyClientCarriesNoTimeoutOfItsOwn(t *testing.T) {
	sites := httpClientTimeoutSites(t, filepath.Join(moduleRoot(t), filepath.FromSlash(gatewayProxyClientFile)))

	if sites.inLiteral > 0 || sites.byAssignment > 0 {
		t.Errorf("%s gives its proxy http.Client a Timeout.\n\n"+
			"That bound is enforced independently of the request context, so every proxied "+
			"call becomes min(proxy.forwardBudget, this) with the smaller winning invisibly "+
			"— and it cannot be ordered against the per-upstream budgets because it does not "+
			"live with them. It is also unreachable from the plaintext branch, so it bounds "+
			"production and not dev. This is the 30s that capped /v1/ask under the copilot's "+
			"own five-minute budget. Change proxy.forwardBudget instead: it is per-upstream, "+
			"it is read by this file's other guards, and it dies with the caller.",
			gatewayProxyClientFile)
	}

	if sites.clients == 0 {
		t.Fatalf("%s no longer constructs an http.Client literal, so this guard asserted "+
			"nothing. The proxy transport did not stop existing — find where it moved and "+
			"point gatewayProxyClientFile there, or the no-client-timeout rule is now "+
			"unenforced on the path every wealth/datamaster/tv-sync/copilot read takes",
			gatewayProxyClientFile)
	}
}

// clientTimeoutSites counts what httpClientTimeoutSites found in one file.
type clientTimeoutSites struct {
	clients      int // http.Client composite literals
	inLiteral    int // ... of which set Timeout in the literal
	byAssignment int // `x.Timeout = ...` writes, which the literal scan cannot see
}

func httpClientTimeoutSites(t *testing.T, path string) clientTimeoutSites {
	t.Helper()

	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	var out clientTimeoutSites
	ast.Inspect(f, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.CompositeLit:
			if !isHTTPClientType(v.Type) {
				return true
			}
			out.clients++
			for _, elt := range v.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Timeout" {
					out.inLiteral++
				}
			}
		case *ast.AssignStmt:
			// The same bound, spelled as a later field write rather than in the literal.
			for _, lhs := range v.Lhs {
				sel, ok := lhs.(*ast.SelectorExpr)
				if ok && sel.Sel.Name == "Timeout" {
					out.byAssignment++
				}
			}
		}
		return true
	})
	return out
}

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

func TestIngressProxyTimeoutsExceedTheGatewayBound(t *testing.T) {
	root := moduleRoot(t)

	// The bound to beat is read from source, not copied: a hardcoded deadline here would go
	// stale the moment testConnectionTimeout is tuned, which is exactly when this matters.
	gatewayBound := durationConst(t,
		filepath.Join(root, "services/api_gateway/internal/control/control.go"), "testConnectionTimeout")

	annotations := apiGatewayIngressAnnotations(t, root)

	for _, key := range ingressProxyTimeoutAnnotations {
		raw, ok := annotations[key]
		if !ok {
			t.Errorf("%s carries no %s annotation.\n\n"+
				"That does NOT mean the request is unbounded at the edge — it means "+
				"ingress-nginx applies its 60s DEFAULT, which is under testConnectionTimeout (%s) "+
				"and under the operator's own probe wait, and whose clock starts before "+
				"theirs. Every slow Test Connection then dies as a 504 from nginx and the "+
				"caller blames the gateway or the network while probing a host that may be "+
				"perfectly healthy. Restore it above %s.", ingressFile, key, gatewayBound, gatewayBound)
			continue
		}
		secs, perr := strconv.Atoi(strings.TrimSpace(raw))
		if perr != nil {
			t.Errorf("%s sets %s to %q, which is not a whole number of seconds. nginx "+
				"rejects a malformed value and falls back to its 60s default, so this "+
				"reads as a bound that is set and is not.", ingressFile, key, raw)
			continue
		}
		if got := time.Duration(secs) * time.Second; got <= gatewayBound {
			t.Errorf("%s sets %s = %s, which does not exceed the gateway's testConnectionTimeout "+
				"(%s, services/api_gateway/internal/control/control.go).\n\n"+
				"nginx would cut the connection before the caller's own bound expires, and "+
				"before that the operator's probeTimeout — so the caller gets a 504 about "+
				"the edge instead of the probe verdict only the operator can give. Raise "+
				"this annotation rather than lowering testConnectionTimeout: the inner bounds are "+
				"sized to real work (Job create, pod scheduling, image pull, a 10s dial).",
				ingressFile, key, got, gatewayBound)
		}
	}
}

// apiGatewayIngressAnnotations returns the annotations on the api-gateway Ingress.
//
// It FAILS rather than returning an empty map when the document is absent: a guard that
// matches no document passes forever once the Ingress is renamed or the manifest moved,
// and every bound read out of this file is the outermost one on a real call.
func apiGatewayIngressAnnotations(t *testing.T, root string) map[string]string {
	t.Helper()

	path := filepath.Join(root, filepath.FromSlash(ingressFile))
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

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
		if d.Kind == "Ingress" && d.Metadata.Name == "api-gateway" {
			return d.Metadata.Annotations
		}
	}

	t.Fatalf("no Ingress named api-gateway found in %s — if it was renamed or split out, "+
		"point ingressFile at its new home; the edge bound it carries is the outermost "+
		"deadline on every request this gateway serves", ingressFile)
	return nil
}

// gatewayServerFile declares equal bounds for the ordinary gateway listener and
// the optional paired BFF mTLS listener.
//
// It no longer writes an http.Server literal — every server in the estate is built
// by internal/platform/httpserver, which cannot be called with a timeout unset
// (#235). What this file still owns is the two OVERRIDES: the gateway is the only
// server with an ingress in front of it and per-route budgets of its own, so
// Write and Idle are its own consts and the ordering below is still its problem.
const gatewayServerFile = "services/api_gateway/cmd/api_gateway/main.go"

// gatewayHandlerBudgets are every per-request deadline a handler on that server can set.
// WriteTimeout is a bound on ALL of them at once, so it has to clear the largest.
var gatewayHandlerBudgets = []deadlineLayer{
	{name: "testConnectionTimeout", file: "services/api_gateway/internal/control/control.go",
		label: "POST /v1/control/test-connection"},
	{name: "callTimeout", file: "services/api_gateway/internal/control/control.go",
		label: "every other control-plane RPC"},
	{name: "copilotForwardTimeout", file: "services/api_gateway/internal/proxy/proxy.go",
		label: "a proxied POST /v1/ask"},
	{name: "readForwardTimeout", file: "services/api_gateway/internal/proxy/proxy.go",
		label: "a proxied wealth/datamaster/tv-sync read"},
}

// nginxUpstreamKeepaliveDefault is ingress-nginx's `upstream-keepalive-timeout` default:
// how long IT keeps an idle connection to this gateway pooled for reuse. The gateway's
// IdleTimeout has to outlast it — see the test below.
const nginxUpstreamKeepaliveDefault = 60 * time.Second

// TestGatewayServerBoundsTheConnectionItself covers the two leaks NO request deadline can
// reach, and the ordering that decides whether the request deadline is ever heard from.
//
// Every other bound in this file is on an outbound CALL. None of them bounds the inbound
// connection, and until #235 this server set only ReadHeaderTimeout — which stops timing
// at the last header byte. Two consequences, both on the gateway that is the sole ingress
// for ORDERS:
//
//   - no WriteTimeout: a client that never finishes reading the response, or an
//     intermediary that stops reading, pins a goroutine and an fd indefinitely;
//   - no IdleTimeout: net/http falls back to ReadTimeout, which is ALSO unset, so there is
//     NO idle bound and every keep-alive connection is held until the peer closes it.
//
// ABSENCE MUST FAIL, NOT JUST A BAD VALUE — for IdleTimeout especially, since deleting it
// does not loosen the bound, it removes the bound. That is the bug, and a guard that only
// checked the number when present would pass on the one edit most likely to restore it.
//
// AND WriteTimeout IS ITSELF IN THE NESTING. It applies to every route, so if it falls
// below the longest handler budget it silently becomes the real limit — and it is the
// worst-behaved limit on the box: the connection is severed with no status, no body and
// nothing said about which upstream was slow. It must exceed all of them and stay under
// the edge's proxy-read-timeout, so the gateway gives up before nginx does and is the
// layer that reports it.
func TestGatewayServerBoundsTheConnectionItself(t *testing.T) {
	root := moduleRoot(t)
	if err := checkPairedHTTPRecipes(pairedHTTPFiles(t, root)); err != nil {
		t.Fatal(err)
	}
	fields := gatewayTimeoutFields(t, filepath.Join(root, filepath.FromSlash(gatewayServerFile)))

	consequences := map[string]string{
		"Write": "a caller that stops reading the response — or a wedged intermediary " +
			"— pins a goroutine and a file descriptor for as long as it likes, and this " +
			"gateway is the only way an order reaches the spine",
		"Idle": "net/http then falls back to ReadTimeout, and if that is unset too there " +
			"is NO idle bound at all and every keep-alive connection is held until the peer " +
			"closes it. Under a pooling proxy that is the steady state, not a slow leak",
	}

	got := map[string]time.Duration{}
	for _, field := range []string{"Write", "Idle"} {
		ident, present := fields[field]
		if !present {
			t.Errorf("%s builds its httpserver.Timeouts without %s.\n\n"+
				"Absence is not a looser bound, it is NO bound: %s.\n\n"+
				"(httpserver.New would refuse a zero at startup, so this is the softer "+
				"failure of the two — but it means the gateway silently took the estate "+
				"default for a bound that has to be ordered against ITS handler budgets and "+
				"ITS ingress, which no other server has.)",
				gatewayServerFile, field, consequences[field])
			continue
		}
		if ident == "" {
			t.Errorf("%s sets %s to something other than a named const, so no test can read "+
				"it and order it against the handler budgets it has to clear. Give it a const "+
				"in this file, as gatewayWriteTimeout/gatewayIdleTimeout were.",
				gatewayServerFile, field)
			continue
		}
		got[field] = durationConst(t, filepath.Join(root, filepath.FromSlash(gatewayServerFile)), ident)
	}
	if len(got) != 2 {
		return // the failures above say everything; the orderings below would only repeat them
	}

	// WriteTimeout against every handler budget on this server.
	for _, budget := range gatewayHandlerBudgets {
		inner := durationConst(t, filepath.Join(root, filepath.FromSlash(budget.file)), budget.name)
		if got["Write"] <= inner {
			t.Errorf("gatewayWriteTimeout (%s, %s) does not exceed %s (%s, %s) = %s, the budget "+
				"for %s.\n\n"+
				"WriteTimeout applies to EVERY route, so it becomes the real limit on that one — "+
				"and it fires by severing the connection: no status, no body, nothing naming the "+
				"upstream that was slow, where the handler's own deadline produces a 502/504 and "+
				"a log line. Raise gatewayWriteTimeout rather than cutting %s, which is sized to "+
				"real work.",
				got["Write"], gatewayServerFile, budget.name, budget.file, budget.label,
				inner, budget.label, budget.name)
		}
	}

	// WriteTimeout against the edge. Both nginx bounds are read, because either one
	// firing first takes the diagnosis out of the gateway's hands.
	annotations := apiGatewayIngressAnnotations(t, root)
	for _, key := range ingressProxyTimeoutAnnotations {
		raw, ok := annotations[key]
		if !ok {
			continue // TestIngressProxyTimeoutsExceedTheGatewayBound owns the absence case
		}
		secs, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil {
			continue // and the malformed case
		}
		if edge := time.Duration(secs) * time.Second; got["Write"] >= edge {
			t.Errorf("gatewayWriteTimeout (%s) is not below the edge's %s = %s (%s).\n\n"+
				"nginx would cut first, and the caller would get its anonymous 504 instead of "+
				"anything this gateway could tell them — while the gateway went on holding the "+
				"goroutine and the socket, which is the leak WriteTimeout exists to stop. The "+
				"gateway must be the layer that gives up first.",
				got["Write"], key, edge, ingressFile)
		}
	}

	// IdleTimeout against the proxy that pools connections to this server.
	if got["Idle"] <= nginxUpstreamKeepaliveDefault {
		t.Errorf("gatewayIdleTimeout (%s) does not exceed ingress-nginx's "+
			"upstream-keepalive-timeout default (%s).\n\n"+
			"Too SHORT is its own fault, not a safer one: the gateway closes idle connections "+
			"nginx still believes it may reuse, and the race surfaces as intermittent 502s "+
			"under no load at all — harder to read than the leak this field closes. If the "+
			"edge's keepalive is retuned, retune this above it.",
			got["Idle"], nginxUpstreamKeepaliveDefault)
	}
}

// gatewayTimeoutFields returns the common keyed fields of both httpserver.Timeouts literals
// in path, mapped to the identifier it is set to — or "" when it is set to anything
// other than a bare identifier (an inline `105 * time.Second` cannot be ordered against
// anything, because durationConst has no const to read).
//
// IT READS THE OVERRIDE, NOT THE SERVER. Until #235 this read the fields of the
// `&http.Server{...}` literal here; that literal is gone, because a struct literal
// cannot require a field and twenty-six roots proved it. httpserver.New now refuses a
// zero outright, so absence of a bound is no longer the failure this can see — what it
// still has to see is that the gateway's two OVERRIDES are named consts, because they
// are the only bounds in the estate that must be ordered against per-route budgets and
// an ingress annotation.
//
// It requires exactly two equal literals. The paired-boundary guard identifies their
// constructor sites; neither listener can silently acquire a different deadline.
func gatewayTimeoutFields(t *testing.T, path string) map[string]string {
	t.Helper()

	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	var literals int
	var firstLiteral string
	out := map[string]string{}
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok || !isTimeoutsType(lit.Type) {
			return true
		}
		literals++
		normalized := map[string]string{}
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				t.Fatal("unkeyed timeout")
			}
			normalized[bondCloneNode(kv.Key)] = bondCloneNode(kv.Value)
		}
		// Compare semantic keyed fields, independent of Go formatting/order.
		signature := ""
		for _, key := range []string{"ReadHeader", "Read", "Write", "Idle"} {
			value, ok := normalized[key]
			if !ok {
				t.Fatalf("missing timeout %s", key)
			}
			signature += key + "=" + value + ";"
		}
		if len(normalized) != 4 {
			t.Fatal("unexpected timeout field")
		}
		if literals == 1 {
			firstLiteral = signature
		} else if signature != firstLiteral {
			t.Fatal("paired gateway listeners have different bounds")
		}
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok {
				continue
			}
			name := ""
			if id, ok := kv.Value.(*ast.Ident); ok {
				name = id.Name
			}
			out[key.Name] = name
		}
		return true
	})

	if literals != 2 {
		t.Fatalf("%s constructs %d httpserver.Timeouts literals, want exactly 2 equal reviewed listeners.\n\n"+
			"With none, the gateway has stopped overriding the estate defaults and its "+
			"WriteTimeout is no longer sized against its own handler budgets or kept under "+
			"the ingress's proxy-read-timeout — the orderings below would assert nothing. "+
			"With several, this reads an arbitrary one while another server may be bounded "+
			"by numbers nobody ordered.", path, literals)
	}
	return out
}

// isTimeoutsType reports whether a composite-literal type is httpserver.Timeouts.
func isTimeoutsType(e ast.Expr) bool {
	if star, ok := e.(*ast.StarExpr); ok {
		e = star.X
	}
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Timeouts" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "httpserver"
}

// isHTTPServerType reports whether a composite-literal type is http.Server, including
// through the `&http.Server{...}` the code actually writes.
func isHTTPServerType(e ast.Expr) bool {
	if star, ok := e.(*ast.StarExpr); ok {
		e = star.X
	}
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Server" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "http"
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

// delegatingClientFiles must construct NO http.Client of their own.
//
// TestcallerHTTPClientCarriesNoTimeoutOfItsOwn pins the ONE client a terminal
// surface is allowed to build. That rule was scoped to a single file path, and a
// second client sat outside it: the kanz shell's own gateway package carried
// http.Client{Timeout: 60 * time.Second}, so its calls were bounded by
// min(context deadline, 60s) with the smaller winning invisibly — exactly the
// pattern the guard above exists to forbid. It also sent no request signature,
// so a gateway enforcing them 401'd every call (#198).
//
// THE FILE THAT DEFECT LIVED IN IS GONE WITH THE TERMINAL CLIENT, and this list
// was repointed rather than emptied. An empty list here would pass forever while
// asserting nothing — and the property is not about that file, it is about any
// surface that says it delegates to the signed gateway transport. The lake
// snapshot command is the remaining consumer; a direct request there would
// duplicate its signature and deadline responsibilities.
var delegatingClientFiles = []string{
	"cmd/kanz_lake_snapshot/main.go",
}

func TestDelegatingClientsBuildNoHTTPClientOfTheirOwn(t *testing.T) {
	root := moduleRoot(t)
	for _, rel := range delegatingClientFiles {
		path := filepath.Join(root, filepath.FromSlash(rel))
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		found := 0
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if ok && isHTTPClientType(lit.Type) {
				found++
			}
			return true
		})
		if found > 0 {
			t.Errorf("%s constructs %d http.Client literal(s). It must delegate to "+
				"cmd/kanz_lake_snapshot/internal/gatewayclient instead: a second client is a second place for the "+
				"request signature and the deadline rule to drift, and both have already "+
				"drifted here once (#198) — the signature was absent entirely, and a 60s "+
				"client timeout silently won over the caller's context.", rel, found)
		}
	}
}
