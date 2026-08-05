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
)

// EVERY SERVER IN THIS ESTATE IS BOUND ON ALL FOUR SIDES, AND THERE IS ONE PLACE
// THAT DECIDES WHAT THAT MEANS.
//
// Twenty-six composition roots each wrote the same partial literal — Addr, Handler,
// ReadHeaderTimeout, nothing else — and all twenty-six were wrong the same way
// (#235). ReadHeaderTimeout stops timing at the last header byte, so past that point
// nothing bounded the connection at all: a caller that stopped reading, or a wedged
// intermediary, pinned a goroutine and a file descriptor until the peer closed. With
// IdleTimeout and ReadTimeout both unset net/http applies NO idle bound, so under a
// pooling proxy every keep-alive connection ever opened is held — the steady state,
// not a slow leak. The api-gateway is the sole ingress for orders, so enough held
// descriptors on that path and no order reaches the spine.
//
// WHY A GUARD AND NOT A REVIEW HABIT. The partial literal was not written once and
// copied by accident; it was the house shape, reproduced by the scaffold, and it
// survived twenty-six reviews. pkg/secret records the same lesson with a sharper
// ending: fifteen roots kept an error-discarding copy for weeks while two had the
// repair, because nothing connected them. The constructor is the fix; this file is
// what stops the copies coming back.
//
// WHY IT IS A GUARD AND NOT A TEST OF THE LEAK. The failure is resource exhaustion
// under sustained load against a wedged upstream. Nothing local reproduces it, and
// -race does not run on the usual box. The constructor being unavoidable is the
// evidence — so the scanner's own non-vacuity matters more than usual here.

// httpServerHome is the ONE package allowed to construct an http.Server.
const httpServerHome = "internal/platform/httpserver"

// bareHTTPServerExemptions maps a module-relative file to the issue that retires
// the exemption. Empty, and it should stay that way: an http.Server built anywhere
// else is a server whose four bounds nobody has to justify.
//
// A new entry is a claim that some server needs bounds the estate default cannot
// express. Say which bound and why, and file the issue — Timeouts is four fields
// and overriding one (as cmd/api-gateway does for Write and Idle) does not need an
// exemption.
var bareHTTPServerExemptions = map[string]string{}

func TestOnlyThePlatformConstructorBuildsAnHTTPServer(t *testing.T) {
	root := moduleRoot(t)

	// TESTS ARE OUT OF SCOPE, DELIBERATELY. A server built by a test lives and dies
	// with the test binary, so it leaks nothing an operator can be paged for, and
	// httptest already owns that shape. Production code is the whole surface here.
	files := nonTestGoFiles(t, root)
	if len(files) == 0 {
		t.Fatal("found zero non-test Go files — the scanner is broken, not the estate")
	}

	var offenders, home []string
	for _, f := range files {
		n := countCompositeLits(t, filepath.Join(root, filepath.FromSlash(f.rel)), isHTTPServerType)
		if n == 0 {
			continue
		}
		if strings.HasPrefix(f.rel, httpServerHome+"/") {
			home = append(home, f.rel)
			continue
		}
		if _, ok := bareHTTPServerExemptions[f.rel]; ok {
			continue
		}
		offenders = append(offenders, f.rel)
	}

	// NON-VACUITY. If the constructor's own literal has moved or been renamed, this
	// scan is looking for a shape that no longer exists and would pass over an
	// estate full of bare literals.
	if len(home) == 0 {
		t.Fatalf("%s constructs no http.Server literal, so this scan matched nothing and "+
			"proved nothing. Either the constructor moved (point httpServerHome at it) or "+
			"it stopped building the server itself, in which case the four bounds are now "+
			"set somewhere this guard is not looking", httpServerHome)
	}

	// DEAD-ENTRY CHECK: an exemption for a file that no longer builds a server
	// reads as a reviewed decision while protecting nothing, and the next reader
	// takes it as precedent.
	var dead []string
	for rel := range bareHTTPServerExemptions {
		if countCompositeLits(t, filepath.Join(root, filepath.FromSlash(rel)), isHTTPServerType) == 0 {
			dead = append(dead, rel)
		}
	}
	sort.Strings(dead)
	for _, rel := range dead {
		t.Errorf("bareHTTPServerExemptions still names %s (%s), which builds no http.Server. "+
			"Delete the entry — a stale exemption cannot outlive the thing it excused.",
			rel, bareHTTPServerExemptions[rel])
	}

	sort.Strings(offenders)
	for _, rel := range offenders {
		t.Errorf("%s builds an http.Server directly.\n\n"+
			"WHAT IT COSTS: a hand-written literal is one that can omit a timeout, and the "+
			"omission does not look like anything — a server with no WriteTimeout serves "+
			"traffic perfectly until a caller stops reading, then pins a goroutine and a "+
			"file descriptor until the process restarts; with IdleTimeout absent net/http "+
			"falls back to ReadTimeout and, if that is absent too, applies no idle bound at "+
			"all. That exact literal shipped in twenty-six composition roots (#235).\n\n"+
			"Use %s.New(addr, handler, %s.Standard()), which refuses to build a server with "+
			"any of the four unset. To change one bound for this server, override that field "+
			"of Standard() — cmd/api-gateway does it for Write and Idle — rather than "+
			"reaching for the raw struct.",
			rel, filepath.Base(httpServerHome), filepath.Base(httpServerHome))
	}
}

// scaffoldTemplateFile mints new composition roots, and its server is a STRING, so
// the AST scan above cannot see it.
//
// This is not a hypothetical gap. The partial literal reached twenty-six services
// because it was the scaffold's output: fixing the twenty-six without fixing the
// generator would have left the twenty-seventh to be born broken, and it would have
// been born reviewed, because it came out of the tool.
const scaffoldTemplateFile = "tools/scaffold/templates.go"

func TestTheScaffoldGeneratesABoundedServer(t *testing.T) {
	body := readModuleFile(t, scaffoldTemplateFile)

	if strings.Contains(body, "http.Server{") {
		t.Errorf("%s emits an http.Server literal.\n\n"+
			"Every service generated from this template would then carry its own copy of "+
			"the four bounds — which is exactly how twenty-six roots came to set only "+
			"ReadHeaderTimeout (#235). Emit httpserver.New(...) instead.", scaffoldTemplateFile)
	}
	if !strings.Contains(body, "httpserver.New(") {
		t.Errorf("%s no longer emits httpserver.New(...).\n\n"+
			"The check above would then pass on a template that builds no server at all, or "+
			"one that builds it some third way. A generated service must come out bounded.",
			scaffoldTemplateFile)
	}
}

// defaultClientExemptions maps a module-relative file to the issue retiring it.
//
// Empty. http.DefaultClient is a package-level variable shared with every other
// package linked into the binary: anything that sets DefaultClient.Timeout or
// swaps http.DefaultTransport silently retunes calls made from files that never
// mention it. It also carries DefaultTransport's MaxIdleConnsPerHost of 2, a limit
// nobody in this estate chose. Both were live on the api-gateway's proxy path, the
// one every wealth/datamaster/tv-sync/copilot read takes (#235).
var defaultClientExemptions = map[string]string{}

func TestNoPackageRunsOnTheSharedDefaultClient(t *testing.T) {
	root := moduleRoot(t)

	// Tests excluded for the same reason as above, and one more: a test asserting
	// against its own httptest server over http.DefaultClient shares that global
	// with nothing that outlives the run.
	files := nonTestGoFiles(t, root)
	if len(files) == 0 {
		t.Fatal("found zero non-test Go files — the scanner is broken, not the estate")
	}

	// AST, NOT GREP. Four files explain in comments why they do NOT use
	// http.DefaultClient, and a text scan would fail every one of them — a guard
	// that fires on the documentation of its own rule gets deleted.
	uses := func(rel string) bool {
		return countSelectorUses(t, filepath.Join(root, filepath.FromSlash(rel)), "http", "DefaultClient") > 0
	}

	var offenders []string
	for _, f := range files {
		if uses(f.rel) {
			if _, ok := defaultClientExemptions[f.rel]; ok {
				continue
			}
			offenders = append(offenders, f.rel)
		}
	}

	var dead []string
	for rel := range defaultClientExemptions {
		if !uses(rel) {
			dead = append(dead, rel)
		}
	}
	sort.Strings(dead)
	for _, rel := range dead {
		t.Errorf("defaultClientExemptions still names %s (%s), which no longer touches "+
			"http.DefaultClient. Delete the entry — a stale exemption cannot outlive the "+
			"thing it excused.", rel, defaultClientExemptions[rel])
	}

	sort.Strings(offenders)
	for _, rel := range offenders {
		t.Errorf("%s makes requests on http.DefaultClient.\n\n"+
			"That is a SHARED MUTABLE GLOBAL. Any package in this binary can set "+
			"DefaultClient.Timeout or replace http.DefaultTransport and thereby retune every "+
			"call made here, from somewhere no reader of this file would look — and a client "+
			"timeout is enforced independently of the request context, so it would win "+
			"invisibly over the per-call budget that is supposed to be the only bound. It "+
			"also carries DefaultTransport's MaxIdleConnsPerHost of 2, which is connection "+
			"churn on any fan-out path.\n\n"+
			"Build a client this file owns: &http.Client{Transport: &http.Transport{...}}, "+
			"with NO Timeout field — bound the call with a context deadline, as "+
			"proxy.forwardBudget does.", rel)
	}
}

// THE ESTATE DEFAULT IS ITSELF IN THE NESTING.
//
// Timeouts.Write is a bound on EVERY route of every service that takes Standard(),
// and it fires the worst way any bound here fires: the connection is severed, with
// no status, no body and nothing naming what was slow (measured — a client gets a
// bare EOF). So it must clear the largest deliberate budget a caller applies to
// those services, or it silently becomes the real limit and takes the diagnosis
// away from the layer that has it. Same argument as gatewayWriteTimeout one hop out,
// which is why that one is guarded in probe_deadline_nesting_test.go.
var standardWriteMustExceed = []deadlineLayer{
	{"copilotForwardTimeout", "services/api-gateway/internal/proxy/proxy.go",
		"the gateway's wait on a proxied POST /v1/ask"},
	{"readForwardTimeout", "services/api-gateway/internal/proxy/proxy.go",
		"the gateway's wait on a proxied wealth/datamaster/tv-sync read"},
}

const httpServerFile = httpServerHome + "/httpserver.go"

func TestStandardTimeoutsClearTheBudgetsBeneathThem(t *testing.T) {
	root := moduleRoot(t)
	at := func(rel string) string { return filepath.Join(root, filepath.FromSlash(rel)) }

	write := durationConst(t, at(httpServerFile), "standardWriteTimeout")
	idle := durationConst(t, at(httpServerFile), "standardIdleTimeout")
	read := durationConst(t, at(httpServerFile), "standardReadTimeout")
	readHeader := durationConst(t, at(httpServerFile), "standardReadHeaderTimeout")

	for _, budget := range standardWriteMustExceed {
		inner := durationConst(t, at(budget.file), budget.name)
		if write <= inner {
			t.Errorf("standardWriteTimeout (%s, %s) does not exceed %s (%s, %s) = %s, the "+
				"budget for %s.\n\n"+
				"The upstream then severs the connection before the gateway's own deadline "+
				"expires, so the caller gets an anonymous EOF instead of the gateway's 502 "+
				"NAMING the upstream that was slow — and the gateway's log line, which is the "+
				"only record of which service wedged, is never written. Raise "+
				"standardWriteTimeout rather than cutting %s, which is sized to real work.",
				write, httpServerFile, budget.name, budget.file, budget.label, inner,
				budget.label, budget.name)
		}
	}

	// The pooling side. proxyIdleConnTimeout is when the GATEWAY retires an idle
	// connection to these services; standardIdleTimeout is when THEY retire it.
	// The gateway must be the one that gives it up.
	pool := durationConst(t,
		at("services/api-gateway/cmd/api-gateway/main.go"), "proxyIdleConnTimeout")
	if idle <= pool {
		t.Errorf("standardIdleTimeout (%s) does not exceed proxyIdleConnTimeout (%s, "+
			"services/api-gateway/cmd/api-gateway/main.go).\n\n"+
			"The upstream then closes idle connections the gateway's pool still believes it "+
			"may reuse. net/http retries an idempotent request over a fresh connection, so "+
			"this does not usually surface as an error — it surfaces as latency nobody can "+
			"attribute, and on a non-idempotent request as a 502 under no load at all. Too "+
			"SHORT is its own fault here, not the safe direction.", idle, pool)
	}

	// Read has to leave room for ReadHeader or the header bound is unreachable and
	// its failure message never appears.
	if read <= readHeader {
		t.Errorf("standardReadTimeout (%s) does not exceed standardReadHeaderTimeout (%s) — "+
			"the whole-request bound would fire before the header bound it contains, so a "+
			"slowloris would be reported as a slow body", read, readHeader)
	}
}

// STREAMING IS THE ONE HANDLER SHAPE Timeouts.Write BREAKS RATHER THAN BOUNDS.
//
// WriteTimeout bounds the WHOLE response, so it severs a Server-Sent-Events stream
// mid-flight — measured in internal/platform/httpserver's own tests: 3 of 10 frames
// delivered, then an unexpected EOF with no status and nothing logged. A Trading
// Terminal on the other end shows a book that has stopped updating, which looks
// exactly like a quiet market.
//
// The answer is NOT to leave WriteTimeout off the servers that stream — that would
// put the unbounded server back and hide it behind a legitimate reason, while every
// ordinary buffered read on the same server lost its bound too. It is
// http.NewResponseController(w).SetWriteDeadline(time.Time{}), which lifts the bound
// for that ONE connection.
//
// So: a handler that opens a stream must lift its own write deadline, and this
// guard is the only thing that will tell the author of the SECOND streaming endpoint
// in this estate — who will have no reason to know.
const sseContentType = "text/event-stream"

func TestStreamingHandlersLiftTheirWriteDeadline(t *testing.T) {
	root := moduleRoot(t)

	var streamers, missing []string
	for _, f := range nonTestGoFiles(t, root) {
		if !strings.Contains(f.body, sseContentType) {
			continue
		}
		if strings.HasPrefix(f.rel, "test/arch/") {
			continue // this scanner names the content type in its own prose
		}
		streamers = append(streamers, f.rel)
		// AST, NOT GREP — and this one was caught by its own mutation test. The first
		// version asked whether the file CONTAINED "SetWriteDeadline"; deleting the
		// actual call left the paragraph above it explaining why the call is there,
		// and the guard went green over a stream that was once again being severed.
		// A comment about a rule must never satisfy the rule.
		if countMethodCalls(t, filepath.Join(root, filepath.FromSlash(f.rel)), "SetWriteDeadline") == 0 {
			missing = append(missing, f.rel)
		}
	}

	// NON-VACUITY. tv-sync's broker stream is the estate's one SSE endpoint. Zero
	// means it was deleted, renamed its content type, or moved somewhere this walk
	// does not reach — and in the last two cases the endpoint is still there, now
	// unguarded and being severed at WriteTimeout.
	if len(streamers) == 0 {
		t.Fatalf("found no handler serving %q, so this guard asserted nothing. tv-sync's "+
			"/broker/accounts/{id}/stream is the estate's Server-Sent-Events endpoint — if "+
			"it moved, this walk must follow it; if it is gone, delete this guard "+
			"deliberately rather than leaving it passing on an empty set.", sseContentType)
	}

	sort.Strings(missing)
	for _, rel := range missing {
		t.Errorf("%s serves %q but never calls SetWriteDeadline.\n\n"+
			"Every server in this estate is built by %s and carries a WriteTimeout, which "+
			"bounds the WHOLE response — so this stream will be severed mid-session with no "+
			"status, no body and nothing logged, and the client will see a feed that simply "+
			"stopped rather than an error. Clear the bound for this one connection:\n\n"+
			"\thttp.NewResponseController(w).SetWriteDeadline(time.Time{})\n\n"+
			"Do NOT drop WriteTimeout from the server instead: that restores the unbounded "+
			"server of #235 for every ordinary route it also serves.",
			rel, sseContentType, httpServerHome)
	}
}

// --- scanning mechanics ---
//
// These carry no failure text. Each caller says what its own violation costs on
// its own path, which is the whole value of these guards; a shared message would
// be true of all of them and useful for none.

// nonTestGoFiles is every Go source file in the module that is not a test.
func nonTestGoFiles(t *testing.T, root string) []goFile {
	t.Helper()

	var out []goFile
	for _, f := range goFilesUnder(t, root) {
		if strings.HasSuffix(f.rel, "_test.go") {
			continue
		}
		out = append(out, f)
	}
	return out
}

// countCompositeLits counts composite literals in path whose type satisfies match.
func countCompositeLits(t *testing.T, path string, match func(ast.Expr) bool) int {
	t.Helper()

	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	n := 0
	ast.Inspect(f, func(node ast.Node) bool {
		if lit, ok := node.(*ast.CompositeLit); ok && match(lit.Type) {
			n++
		}
		return true
	})
	return n
}

// countSelectorUses counts `pkg.name` selector expressions in path.
//
// It reads the AST rather than the text so that a comment EXPLAINING why a file
// avoids something is not counted as using it — four files document exactly that
// about http.DefaultClient, and a grep-based check failed all four.
func countSelectorUses(t *testing.T, path, pkg, name string) int {
	t.Helper()

	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	n := 0
	ast.Inspect(f, func(node ast.Node) bool {
		sel, ok := node.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != name {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == pkg {
			n++
		}
		return true
	})
	return n
}

// countMethodCalls counts calls to a method named name on ANY receiver.
//
// The receiver is deliberately unconstrained: the call this exists for is
// http.NewResponseController(w).SetWriteDeadline(...), where the receiver is a call
// expression rather than an identifier, and a future handler might just as well
// hold the controller in a local.
func countMethodCalls(t *testing.T, path, name string) int {
	t.Helper()

	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	n := 0
	ast.Inspect(f, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == name {
			n++
		}
		return true
	})
	return n
}

// readModuleFile returns the contents of a module-relative file, failing if it is
// gone — a guard whose subject has moved must say so rather than pass.
func readModuleFile(t *testing.T, rel string) string {
	t.Helper()

	path := filepath.Join(moduleRoot(t), filepath.FromSlash(rel))
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v — if this file moved, point the guard that reads it at the "+
			"new path; it is not asserting anything from here", rel, err)
	}
	return string(b)
}
