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
		origin := reachedOrderOrigin(closure)
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
			"(api-gateway, oms, venue-binance, venue-okx). The import scan or orderOriginPackages is "+
			"broken and this guard proves nothing", len(orderPlacing), orderPlacing)
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

// orderOriginPackages are the packages from which an order can reach an
// exchange. A composition root that can reach ANY of them is on the capital
// path.
//
// The four are the complete set of ways an order exists in this estate, which is
// why the set is small enough to name:
//
//   - api-gateway/internal/orders mints an order COMMAND from an authenticated
//     human request (POST /v1/orders).
//   - internal/signal/translate mints them from a strategy signal — the
//     TradingView webhook and the native alpha runner both fan out through it.
//   - oms/internal/order admits an order to the book and routes it to a venue.
//   - internal/venueadapter/server is the RPC that hands one to the exchange.
//
// A fifth way would be a new entry here, and until it is one this guard would
// not see it — which is why the membership is by IMPORT GRAPH and the reason for
// each entry is written down: the failure mode of a list is that it stops
// matching reality quietly.
var orderOriginPackages = map[string]string{
	modulePath + "/services/api-gateway/internal/orders": "the gateway's order write surface",
	modulePath + "/internal/signal/translate":            "the signal→order translator",
	modulePath + "/services/oms/internal/order":          "the OMS order aggregate",
	modulePath + "/internal/venueadapter/server":         "the venue adapter's execute RPC",
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
// in closure, or "" if there is none. Deterministic in the order the map is
// sorted, so a service reaching two of them always reports the same one.
func reachedOrderOrigin(closure map[string]bool) string {
	var hits []string
	for pkg := range orderOriginPackages {
		if closure[pkg] {
			hits = append(hits, pkg)
		}
	}
	if len(hits) == 0 {
		return ""
	}
	sort.Strings(hits)
	return orderOriginPackages[hits[0]] + " (" + hits[0] + ")"
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
