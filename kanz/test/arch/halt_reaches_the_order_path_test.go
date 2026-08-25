package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// EVERY SERVICE THAT CAN PUT AN ORDER IN FRONT OF AN EXCHANGE HONOURS THE
// PLATFORM KILL-SWITCH (#635).
//
// # What went wrong without it
//
// cmd/kanz-halt describes itself as "the operator's break-glass handle on the
// platform kill-switch". It publishes lifecycle.v1.ModeChanged{component:
// "system"} on platform.mode.changed, and for as long as it existed EXACTLY ONE
// process in the estate subscribed to that subject: webhook-ingest. An operator
// who ran `kanz-halt --by operator:x --reason "risk breach"` stopped TradingView
// signals, and POST /v1/orders, the OMS and both venue adapters carried on
// trading. The gate itself was well built — latching, deny-by-default, halted on
// a nil receiver. Its REACH was the defect, and nothing in this repository
// related that reach to the claim its own package doc made.
//
// A guard rather than a paragraph, because the state this replaced was not an
// unwritten rule being broken. It was a WRITTEN claim — "platform kill-switch",
// "every tenant's execution" — that no check compared against the code. The
// publish side was already guarded to the letter (nats_identity_test.go asserts
// who may PUBLISH the FACT); the subscribe side had nothing, which is why a kill
// switch with one listener passed every check in the repository.
//
// # What it checks, and why in this shape
//
// DEFAULT-DENY. Every service with an entrypoint is classified from the IMPORT
// GRAPH — not a grep, and not a hand-kept list of "the trading services". If a
// composition root can reach any package on the capital path, the service must
// honour the halt or carry a named exemption below.
//
// THREE THINGS MUST ALL HOLD, because each one alone has a silent failure:
//
//  1. It reaches internal/platform/halt. That is ONE implementation per concept:
//     a service that grew its own mode flag would satisfy any check that only
//     asked "does it react to a halt", and two brakes drift.
//  2. Its composition root CALLS halt.Arm. Importing the package proves nothing
//     — the OMS could hold a Gate nothing ever opens or closes. Arm is the one
//     place the subscription's delivery policy and failure behaviour are decided.
//  3. Its tenancy.yaml account may SUBSCRIBE platform.mode.changed. This is the
//     half a Go-only fix silently loses: the service authenticates fine, binds
//     nothing, and honours no halt. When #635 was filed exactly one account had
//     this grant.
//
// The guard does NOT assert where each service acts on the gate — that is a
// per-service decision documented at each call site, and a check that tried to
// pin it would either match prose or ossify the placement.
func TestEveryOrderPlacingServiceHonoursTheHalt(t *testing.T) {
	root := moduleRoot(t)
	pkgs := loadPackages(t)
	byPath := make(map[string]pkgInfo, len(pkgs))
	for _, p := range pkgs {
		byPath[p.ImportPath] = p
	}

	origins := deriveOrderOriginPackages(t, pkgs, root)

	subs := serviceSubscribeAllow(t, filepath.Join(root, "infra", "nats", "tenancy.yaml"))
	if len(subs) == 0 {
		t.Fatal("no __system__ subscribe permissions parsed from tenancy.yaml — has the format changed?")
	}

	var (
		orderPlacing []string
		problems     []string
	)
	for _, svc := range servicesWithEntrypoints(t, root) {
		if _, undeployed := notDeployed[svc]; undeployed {
			continue
		}
		entry := modulePath + "/services/" + svc + "/cmd/" + svc
		if _, ok := byPath[entry]; !ok {
			continue // a service whose binary is not named after it; none today
		}
		closure := transitiveImports(entry, byPath)
		origin := reachedOrderOrigin(closure, origins)
		if origin == "" {
			continue
		}
		orderPlacing = append(orderPlacing, svc)
		if _, exempt := orderPathHaltExempt[svc]; exempt {
			continue
		}

		if !closure[haltPackage] {
			problems = append(problems, svc+" reaches the capital path through "+origin+
				" but its composition root does not import "+haltPackage+
				" at any depth — it cannot hear a declared halt, and an operator stopping the "+
				"platform would not stop it")
			continue
		}
		if !callsHaltArm(t, filepath.Join(root, "services", svc, "cmd", svc)) {
			problems = append(problems, svc+" imports "+haltPackage+" but its composition root never "+
				"calls halt.Arm — a Gate nobody subscribes to the FACT stream stays in whatever state "+
				"it was constructed in forever, which is a service that reports ready and honours no halt")
			continue
		}
		svid := systemAccountSVID(svc)
		sub, ok := subs[svid]
		if !ok {
			problems = append(problems, svc+": tenancy.yaml has no `permissions` block for "+svid+
				" at all, so its subscribe set is unverifiable")
			continue
		}
		if permDenies(haltSubject, sub) || !covered(haltSubject, sub.allow) {
			problems = append(problems, svc+" subscribes "+haltSubject+" in code but tenancy.yaml's "+
				"permissions.subscribe for "+svid+" does not allow it — the service authenticates "+
				"fine, receives NOTHING, and honours no halt. NOTE: an explicitly present but EMPTY "+
				"`allow: []` is a NO-OP on this nats-server version, not a deny (see permDenies)")
		}
	}

	// NON-VACUITY. This estate has order-placing services — api-gateway, the OMS
	// and two venue adapters at minimum. A run that classified none of them would
	// pass no matter how unreachable the kill switch had become, which is exactly
	// the shape of the defect this guard exists to prevent: a check that reports
	// "fine" after checking a property nothing can violate.
	if len(orderPlacing) < 4 {
		t.Fatalf("classified only %d order-placing service(s) (%v) — this estate has at least four "+
			"(api-gateway, oms, venue-binance, venue-okx). The import scan is broken and this guard "+
			"proves nothing", len(orderPlacing), orderPlacing)
	}

	// DEAD ENTRIES: an exemption for a service that is no longer on the capital
	// path, or that now honours the halt anyway, is stale permission. It has to
	// fail, or an exemption outlives the condition that justified it and the next
	// reader treats it as a decision rather than a leftover.
	placing := map[string]bool{}
	for _, svc := range orderPlacing {
		placing[svc] = true
	}
	for svc := range orderPathHaltExempt {
		if !placing[svc] {
			problems = append(problems, "exemption for "+svc+" is DEAD: it no longer reaches the "+
				"capital path (or no longer exists), so it needs no exemption")
			continue
		}
		dir := filepath.Join(root, "services", svc, "cmd", svc)
		if callsHaltArm(t, dir) {
			problems = append(problems, "exemption for "+svc+" is DEAD: its composition root now calls "+
				"halt.Arm, so it honours the halt and the exemption only hides it from this guard")
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("%d service(s) can reach a venue without honouring the platform halt:\n\n  %s",
			len(problems), strings.Join(problems, "\n  "))
	}
}

const (
	haltPackage = modulePath + "/internal/platform/halt"
	// haltSubject is internal/platform/mode.Subject. Restated here rather than
	// imported because this guard checks a YAML permission string against the
	// wire subject, and importing the constant would make the two agree by
	// construction — which is the one thing that must not be assumed: the whole
	// failure being guarded is a Go side and a broker side that disagree.
	haltSubject = "platform.mode.changed"
)

// orderOriginArms are the three ways an order comes into existence in this
// estate, each expressed as a property of the SOURCE rather than a package name.
//
// WHY THIS IS DERIVED AND NOT A LIST (#738). It was a list — four import paths,
// hand-kept, with a comment admitting the hole:
//
//	A fifth way would be a new entry here, and until it is one this guard would
//	not see it.
//
// That admission was accurate and the fifth way had already arrived.
// services/optimization mints order commands in internal/bridge and publishes
// them from internal/publish, and because neither package was in the list the
// guard never classified the service as order-placing, never asked whether it
// honours the kill switch, and passed green. Of the transport-coupled guards in
// this package it was the only one whose failure mode was SILENCE rather than a
// loud error: every other guard fatals on a non-vacuity arm, while this one kept
// certifying a kill switch that did not reach a surface which can place orders.
//
// So membership is now a property, and the list below is a FLOOR — the four
// origins the derivation must still find — not the definition. A new origin is
// classified the day it is written, with no edit here.
//
// EACH ARM IS AST, NEVER A GREP over source text. Three guards in this package
// have already passed while the checked thing was deleted, because a regex
// matched the guard's own explanatory comment or a nearby field name. That trap
// is live for exactly this check: pkg/bus/nats.go says "order.order.submit" in a
// comment explaining consumer naming, and a text scan would classify the bus
// package — every service in the estate imports it — as an order origin, which
// would make the guard demand a halt gate in the archiver.
type orderOriginArm struct {
	what string // what the package does, phrased so a failure message reads as prose
	// hits reports whether file exhibits this arm. It is given the file's own
	// import aliases so a package that renames an import is judged on what it
	// actually references.
	hits func(file *ast.File, orderAlias, venueAlias string) bool
}

const (
	orderPBPackage = "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuePBPackage = "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	// submitSubject is the order COMMAND subject. Restated here, like
	// haltSubject above, rather than imported from either of the three packages
	// that declare it: this guard's job is to FIND those declarations, and
	// importing one would make the finder agree with the found by construction.
	submitSubject = "order.order.submit"
)

var orderOriginArms = []orderOriginArm{
	{
		// A command has to be CONSTRUCTED somewhere, and every construction in
		// this module is a composite literal. This is the arm that catches a
		// package which builds orders and hands them to someone else to publish
		// — optimization/internal/bridge is exactly that shape, and it is the
		// reason a subject-only rule is not enough.
		what: "mints an order.v1.SubmitOrder command",
		hits: func(file *ast.File, orderAlias, _ string) bool {
			if orderAlias == "" {
				return false
			}
			return anyNode(file, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok {
					return false
				}
				return isQualified(lit.Type, orderAlias, "SubmitOrder")
			})
		},
	},
	{
		// And a command has to be ADDRESSED somewhere. This is the arm that
		// catches a package which publishes orders someone else built —
		// optimization/internal/publish is that shape, and it is the reason a
		// construction-only rule is not enough either. The two arms are not
		// redundant; each one alone misses half of the service that motivated
		// this rewrite.
		//
		// A BasicLit, deliberately: the constant's NAME varies across the three
		// packages that declare it (SubjectSubmit, subjectSubmit), so matching
		// the identifier would miss a fourth spelling, and matching the text
		// would match prose.
		what: "names the order-submit subject on the wire",
		hits: func(file *ast.File, _, _ string) bool {
			return anyNode(file, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return false
				}
				v, err := strconv.Unquote(lit.Value)
				return err == nil && v == submitSubject
			})
		},
	},
	{
		// The last hop. Both halves are checked because they live in different
		// packages: internal/venueadapter/server DECLARES Execute, and each
		// venue adapter's composition root REGISTERS it. Either one on its own
		// puts the package on the capital path.
		what: "serves the venue adapter's Execute RPC",
		hits: func(file *ast.File, _, venueAlias string) bool {
			if venueAlias == "" {
				return false
			}
			return anyNode(file, func(n ast.Node) bool {
				if fn, ok := n.(*ast.FuncDecl); ok {
					return fn.Recv != nil && fn.Name.Name == "Execute" &&
						takesParam(fn, venueAlias, "ExecuteRequest")
				}
				if call, ok := n.(*ast.CallExpr); ok {
					return isQualified(call.Fun, venueAlias, "RegisterVenueAdapterServiceServer")
				}
				return false
			})
		},
	},
}

// knownOrderOrigins is the NON-VACUITY FLOOR for the derivation: the origins
// that are structural to this estate and whose disappearance from the derived
// set means the scan broke, not that the estate changed.
//
// It is not the membership — that is derived — and it must never be edited to
// silence a failure. If one of these legitimately stops being an order origin
// (the OMS aggregate renamed, the gateway's write surface moved), the fix is to
// update the entry to the package that took over the role, having first checked
// that the role still exists at all.
var knownOrderOrigins = map[string]string{
	modulePath + "/services/api-gateway/internal/orders": "the gateway's order write surface (POST /v1/orders)",
	modulePath + "/internal/signal/translate":            "the signal→order translator",
	modulePath + "/services/oms/internal/order":          "the OMS order aggregate",
	modulePath + "/internal/venueadapter/server":         "the venue adapter's execute RPC",
}

// deriveOrderOriginPackages returns every module package that exhibits at least
// one arm above, mapped to a description of what it does.
//
// It parses each package's NON-TEST files only, for the same reason
// transitiveImports excludes test edges: the question is what the shipped binary
// can do, and a fixture that builds a SubmitOrder would otherwise classify its
// whole package as an order origin.
func deriveOrderOriginPackages(t *testing.T, pkgs []pkgInfo, root string) map[string]string {
	t.Helper()
	origins := make(map[string]string)
	for _, p := range pkgs {
		if p.Standard || !strings.HasPrefix(p.ImportPath, modulePath) {
			continue
		}
		var what []string
		for _, file := range parseNonTestGoFilesIn(t, packageDir(root, p.ImportPath)) {
			for _, arm := range originArmsFor(file) {
				if !contains(what, arm) {
					what = append(what, arm)
				}
			}
		}
		if len(what) > 0 {
			sort.Strings(what)
			origins[p.ImportPath] = strings.Join(what, " and ")
		}
	}

	// THE FLOOR. A derivation that finds nothing, or that has quietly stopped
	// seeing one of the structural origins, must fail LOUDLY — the whole point of
	// this rewrite is that the previous mechanism failed quietly.
	var missing []string
	for pkg, role := range knownOrderOrigins {
		if _, ok := origins[pkg]; !ok {
			missing = append(missing, pkg+" ("+role+")")
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("the order-origin derivation no longer finds %d structural origin(s):\n\n  %s\n\n"+
			"Either the scan is broken — in which case this guard is certifying nothing — or the role "+
			"moved, in which case update knownOrderOrigins to the package that took it over. Do NOT "+
			"delete an entry to make this pass.", len(missing), strings.Join(missing, "\n  "))
	}
	return origins
}

// originArmsFor returns the arms file exhibits, in declaration order. Extracted
// from the scan so TestOrderOriginArms below can drive the SAME code the guard
// runs against synthetic sources — a detector proved on a copy is a detector
// nothing proves.
func originArmsFor(file *ast.File) []string {
	orderAlias := importAlias(file, orderPBPackage)
	venueAlias := importAlias(file, venuePBPackage)
	var out []string
	for _, arm := range orderOriginArms {
		if arm.hits(file, orderAlias, venueAlias) {
			out = append(out, arm.what)
		}
	}
	return out
}

// packageDir maps an import path in this module to its directory on disk.
func packageDir(root, importPath string) string {
	rel := strings.TrimPrefix(strings.TrimPrefix(importPath, modulePath), "/")
	if rel == "" {
		return root
	}
	return filepath.Join(root, filepath.FromSlash(rel))
}

// parseNonTestGoFilesIn is parseNonTestGoFiles without the non-vacuity fatal: a
// package of only _test.go files (test/arch itself is one) is a legitimate
// answer of "no non-test files", not a broken path. The floor above is what
// makes the aggregate non-vacuous.
func parseNonTestGoFilesIn(t *testing.T, dir string) []*ast.File {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	fset := token.NewFileSet()
	var out []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", filepath.Join(dir, name), err)
		}
		out = append(out, f)
	}
	return out
}

// importAlias returns the local name path is bound to in file, or "" if the file
// does not import it. haltImportAlias is the same question asked of one fixed
// package; this is the general form, and both exist because the halt one is
// called where the path is a constant of this file.
func importAlias(file *ast.File, path string) string {
	for _, imp := range file.Imports {
		if strings.Trim(imp.Path.Value, `"`) != path {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return lastSegment(path)
	}
	return ""
}

// lastSegment is the default package name for an import path. It is right for
// every path this guard resolves — the generated SDK's order/v1 package is named
// orderv1 and is ALWAYS imported under an explicit alias in this module, so the
// default is only ever reached for paths whose directory name is the package
// name.
func lastSegment(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

// isQualified reports whether expr is `alias.name`, unwrapping a pointer or
// parentheses so `*venuepb.ExecuteRequest` and `(orderpb.SubmitOrder)` both
// resolve.
func isQualified(expr ast.Expr, alias, name string) bool {
	for {
		switch e := expr.(type) {
		case *ast.StarExpr:
			expr = e.X
		case *ast.ParenExpr:
			expr = e.X
		case *ast.SelectorExpr:
			id, ok := e.X.(*ast.Ident)
			return ok && id.Name == alias && e.Sel.Name == name
		default:
			return false
		}
	}
}

// takesParam reports whether fn has a parameter of type alias.name.
func takesParam(fn *ast.FuncDecl, alias, name string) bool {
	if fn.Type.Params == nil {
		return false
	}
	for _, p := range fn.Type.Params.List {
		if isQualified(p.Type, alias, name) {
			return true
		}
	}
	return false
}

// anyNode reports whether any node in file satisfies pred.
func anyNode(file *ast.File, pred func(ast.Node) bool) bool {
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil || found {
			return false
		}
		if pred(n) {
			found = true
			return false
		}
		return true
	})
	return found
}

// orderPathHaltExempt names services that reach the capital path and do not
// honour the halt, with the reason. DEFAULT-DENY: an entry here is permission,
// and the dead-entry arm above removes it the moment it stops being needed.
var orderPathHaltExempt = map[string]string{
	"market-ingest": "REACHES internal/signal/translate ONLY THROUGH pkg/alpha's Runner, which " +
		"this composition root cannot make emit anything: alpha.Runner.tickLoop returns immediately " +
		"when len(Config.Engines) == 0, and market-ingest registers none. So there is no decision to " +
		"suppress here and a halt subscription would gate nothing — while making the market data " +
		"feed depend on the lifecycle stream, which is a new failure mode for no benefit. " +
		"WHAT RETIRES THIS ENTRY is not a date or a promise: " +
		"TestMarketIngestRegistersNoAlphaEngines below asserts the premise, and the day an engine " +
		"is registered that test fails and this exemption must go.",
}

// transitiveImports returns every module package reachable from root, including
// root itself. Test imports are deliberately EXCLUDED here (unlike allImports):
// the question is what the SHIPPED binary can do, and a test-only edge would
// classify a service as order-placing on the strength of a fixture.
func transitiveImports(root string, byPath map[string]pkgInfo) map[string]bool {
	seen := map[string]bool{}
	var walk func(string)
	walk = func(p string) {
		if seen[p] {
			return
		}
		seen[p] = true
		info, ok := byPath[p]
		if !ok {
			return // stdlib or another module
		}
		for _, imp := range info.Imports {
			walk(imp)
		}
	}
	walk(root)
	return seen
}

// reachedOrderOrigin returns the description of the first capital-path package
// in closure, or "" if there is none. Deterministic in sorted import-path order,
// so a service reaching two of them always reports the same one.
func reachedOrderOrigin(closure map[string]bool, origins map[string]string) string {
	var hits []string
	for pkg := range origins {
		if closure[pkg] {
			hits = append(hits, pkg)
		}
	}
	if len(hits) == 0 {
		return ""
	}
	sort.Strings(hits)
	return origins[hits[0]] + " (" + hits[0] + ")"
}

// callsHaltArm parses dir and reports whether any file calls halt.Arm.
//
// AST, NOT A GREP over the source text. Three guards in this package have
// already passed while the checked thing was deleted, because a regex matched
// the guard's own explanatory comment or a nearby field name — and this file
// says "halt.Arm" in prose a dozen times. A CallExpr whose Fun is a
// SelectorExpr cannot be satisfied by a comment.
//
// It resolves the halt package by IMPORT PATH rather than trusting the
// identifier `halt`, so a file that aliases the import — or one that defines its
// own local `halt` variable — is judged on what it actually calls.
func callsHaltArm(t *testing.T, dir string) bool {
	t.Helper()
	for _, file := range parseNonTestGoFiles(t, dir) {
		alias := haltImportAlias(file)
		if alias == "" {
			continue
		}
		found := false
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Arm" {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == alias {
				found = true
				return false
			}
			return true
		})
		if found {
			return true
		}
	}
	return false
}

// parseNonTestGoFiles parses every non-test .go file directly in dir.
//
// parser.ParseFile per entry rather than parser.ParseDir: ParseDir is deprecated
// (it ignores build tags when grouping files into packages), and grouping is not
// what either caller wants — both ask a question about the FILES a composition
// root is built from.
func parseNonTestGoFiles(t *testing.T, dir string) []*ast.File {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	var out []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", filepath.Join(dir, name), err)
		}
		out = append(out, f)
	}
	// NON-VACUITY, at the smallest scale that matters: a directory that parsed to
	// nothing would make every caller answer "no" — "it does not call halt.Arm"
	// and "it registers no engines" alike — for a directory that was moved or
	// renamed rather than for anything about the code.
	if len(out) == 0 {
		t.Fatalf("no non-test Go files in %s — this scan proves nothing about it", dir)
	}
	return out
}

// haltImportAlias returns the local name internal/platform/halt is bound to in
// file, or "" if the file does not import it.
func haltImportAlias(file *ast.File) string {
	for _, imp := range file.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		if path != haltPackage {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return "halt"
	}
	return ""
}

// TestOrderOriginArms drives the derivation's three detectors over synthetic
// sources — the regression the rewrite exists for (#738).
//
// WHAT THIS PROVES THAT THE GUARD ABOVE CANNOT. The guard runs against the
// estate as it is today, so it passes whether the detectors are sharp or blunt:
// a scan that found the four structural origins and nothing else would satisfy
// both the floor and the ≥4 count while being blind to the fifth service. The
// only way to show the derivation SEES A NEW ORIGIN is to hand it one, and the
// only way to show it is not a text search wearing an AST costume is to hand it
// prose that a text search would match.
//
// Both negative cases are real. pkg/bus/nats.go names the submit subject in a
// comment about consumer naming, and every service in the estate imports
// pkg/bus — a text scan would classify the archiver as an order origin and
// demand a halt gate in it. And internal/compliance references SubmitOrder as a
// read-only consumer, which is why the arm is a COMPOSITE LITERAL and not a
// mention: minting a command is on the capital path, reading one is not.
func TestOrderOriginArms(t *testing.T) {
	const (
		mints    = "mints an order.v1.SubmitOrder command"
		subjects = "names the order-submit subject on the wire"
		serves   = "serves the venue adapter's Execute RPC"
	)
	cases := []struct {
		name string
		src  string
		want []string
	}{
		{
			// THE ONE THAT MOTIVATED THE REWRITE: a brand-new package, in no
			// list anywhere, minting orders. It is classified on the strength of
			// what it does.
			name: "a new package that mints an order command",
			src: `package rebalancer
import orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
func build() *orderpb.SubmitOrder { return &orderpb.SubmitOrder{OrderId: "x"} }`,
			want: []string{mints},
		},
		{
			// The same, under an alias nobody else in the module uses. The arms
			// resolve the import PATH, so the local name is irrelevant — a
			// detector keyed on the identifier `orderpb` would miss this.
			name: "an aliased import is still the order package",
			src: `package rebalancer
import ord "github.com/eighred/kanz/kanz-schemas-go/order/v1"
func build() ord.SubmitOrder { return ord.SubmitOrder{} }`,
			want: []string{mints},
		},
		{
			name: "a package that only publishes what someone else built",
			src: `package publish
const subject = "order.order.submit"
func send(s string) string { return subject + s }`,
			want: []string{subjects},
		},
		{
			name: "the venue adapter's Execute RPC",
			src: `package server
import (
	"context"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
)
type S struct{}
func (s *S) Execute(ctx context.Context, req *venuepb.ExecuteRequest) (*venuepb.ExecuteResponse, error) {
	return nil, nil
}`,
			want: []string{serves},
		},
		{
			name: "a composition root that registers the adapter service",
			src: `package main
import (
	"google.golang.org/grpc"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
)
func wire(srv *grpc.Server, s venuepb.VenueAdapterServiceServer) {
	venuepb.RegisterVenueAdapterServiceServer(srv, s)
}`,
			want: []string{serves},
		},
		{
			// THE PROSE CASE. This is pkg/bus/nats.go's comment, near enough.
			// A grep classifies it; the AST does not see comments at all.
			name: "the subject named only in a comment is not an origin",
			src: `package bus
// dots become underscores: group "oms" + subject "order.order.submit" becomes
// a durable named oms_order_order_submit.
func durable(group, subject string) string { return group + subject }`,
			want: nil,
		},
		{
			// THE READ-ONLY CONSUMER CASE. internal/compliance and
			// cmd/kanz-redrive both look like this. A rule matching any mention
			// of SubmitOrder pulls in 18 packages, most of which cannot place
			// anything.
			name: "reading a SubmitOrder is not minting one",
			src: `package compliance
import orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
func qty(cmd *orderpb.SubmitOrder) string { return cmd.GetOrderId() }`,
			want: nil,
		},
		{
			// A near-miss on the subject: the prefix is shared by every order
			// subject in the estate, and an origin is the SUBMIT one only.
			name: "a different order subject is not the submit subject",
			src: `package projector
const subject = "order.order.filled"
func s() string { return subject }`,
			want: nil,
		},
		{
			// A same-named method on an unrelated type. The arm requires the
			// venue request type, not the name `Execute`, which is one of the
			// most common method names in any codebase.
			name: "an unrelated Execute method is not the venue RPC",
			src: `package db
import "context"
type Q struct{}
func (q *Q) Execute(ctx context.Context, sql string) error { return nil }`,
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), "x.go", tc.src, 0)
			if err != nil {
				t.Fatalf("parse the fixture: %v", err)
			}
			got := originArmsFor(file)
			if len(got) != len(tc.want) {
				t.Fatalf("arms = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("arms = %v, want %v", got, tc.want)
				}
			}
		})
	}

	// NON-VACUITY. Every `want` above could be satisfied by three detectors that
	// return false unconditionally if the positive cases were dropped in a
	// refactor, so assert that this table still exercises all three arms — the
	// count is the number of arms, so adding a fourth arm without a case for it
	// fails here.
	seen := map[string]bool{}
	for _, tc := range cases {
		for _, w := range tc.want {
			seen[w] = true
		}
	}
	if len(seen) != len(orderOriginArms) {
		t.Fatalf("this table exercises %d of the %d origin arms (%v) — an arm with no positive case "+
			"is an arm nothing proves works", len(seen), len(orderOriginArms), seen)
	}
}

// TestMarketIngestRegistersNoAlphaEngines is the live half of market-ingest's
// exemption above (#635).
//
// The exemption rests on a structural fact, not on a judgement: pkg/alpha's
// Runner evaluates nothing when its Config carries no Engines, so the halt gate
// it would consult has nothing to gate. An exemption resting on a fact that
// nothing checks is how a waiver outlives its reason — so the fact is checked
// here, and the day market-ingest registers an engine this fails and the
// exemption has to go with it.
func TestMarketIngestRegistersNoAlphaEngines(t *testing.T) {
	root := moduleRoot(t)
	dir := filepath.Join(root, "services", "market-ingest", "cmd", "market-ingest")

	configs := 0
	var offenders []string
	{
		for _, file := range parseNonTestGoFiles(t, dir) {
			ast.Inspect(file, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				sel, ok := lit.Type.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Config" {
					return true
				}
				id, ok := sel.X.(*ast.Ident)
				if !ok || id.Name != "alpha" {
					return true
				}
				configs++
				for _, elt := range lit.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					key, ok := kv.Key.(*ast.Ident)
					if !ok {
						continue
					}
					if key.Name == "Engines" {
						offenders = append(offenders, "alpha.Config literal")
					}
				}
				return true
			})
		}
	}

	// NON-VACUITY. If market-ingest stops constructing an alpha.Config — renamed,
	// moved, or wired through a helper this scan does not see — the loop above
	// finds nothing and reports success while proving nothing about the premise
	// the exemption rests on.
	if configs == 0 {
		t.Fatal("no alpha.Config literal found in market-ingest's composition root — the premise " +
			"behind its entry in orderPathHaltExempt can no longer be checked here, so the exemption " +
			"is unsupported: either restore the scan or wire halt.Arm and drop the exemption")
	}
	if len(offenders) > 0 {
		t.Fatalf("market-ingest now registers alpha Engines (%s) — it CAN originate an order, so its "+
			"entry in orderPathHaltExempt is void. Wire halt.Arm into its composition root, pass the "+
			"gate as alpha.Config.Gate, grant %s to its account in infra/nats/tenancy.yaml, and delete "+
			"the exemption.", strings.Join(offenders, ", "), haltSubject)
	}
}
