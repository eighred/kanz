package arch

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// A PRODUCER BUILT WITHOUT A TENANT PUBLISHES NOTHING, AND SAYS SO TO NOBODY.
//
// bus.Validate requires tenant_id on every envelope. bus.Producer has exactly
// three ways to supply one (producer.go, "Tenant precedence (MT-01b)"):
//
//  1. Event.TenantID           — stamped by the caller, per event
//  2. bus.TenantIDFromContext  — stashed by Consumer from the INBOUND envelope,
//     so a handler's derived events inherit it
//  3. ProducerConfig.Tenant    — the producer-level fallback
//
// Route 2 exists only inside a delivery. A service that publishes from an HTTP
// handler, a CLI invocation, or a time.Ticker loop has no inbound envelope, so
// if it also stamps no Event.TenantID then route 3 is its ONLY source — and
// when it is empty every publish is refused "tenant_id required" before the
// bytes reach the broker.
//
// THIS HAS NOW HAPPENED FIVE TIMES, in five services:
//
//   - market-ingest — documented in its own composition root, in past tense.
//   - oms           — crash-looped on it (pkg/bus/tenantscope.go).
//   - accounting    — every subscription, redemption and fee refused; the
//     endpoint answered 400 while /readyz stayed 200 (#245, PR #297).
//   - market-data   — the WIRE-01a feed publisher. Latent only because
//     MARKET_DATA_FEED is unset in the deployed manifest.
//   - venue-binance / venue-okx — the ticker feeds that supply tv-sync's
//     MarkSource. These were LIVE, and worse than silent: the mark publisher is
//     the same HealthPublisher readiness.TrackPublisher watches, and the ticker
//     DISCARDS its error (`_ = pub.Publish(...)`). Three instruments is one
//     poll and DefaultPublishFailureThreshold is 3, so every 5-second tick
//     marked the adapter NOT READY — dropping the pod out of its Service and
//     hard-erroring the OMS router on that MIC. Order execution was fine; the
//     mark feed took it down.
//
// Every one of those survived a green suite, because a producer's config is set
// in a composition root that dials a broker before it constructs anything, and
// this repo's tests do not reach that layer. publisher_validation_test.go (#245)
// covers the adjacent half — that a publishing PACKAGE proves one envelope
// against a real Producer. It cannot cover this one: cashmove's envelope was
// already perfect, and market-data's feed package tests pass a producer the TEST
// configures with Tenant: "acme" (bussink_test.go:32) over a wired one that has
// none. A test producer healthier than the deployed one reports green for the
// path that is broken.
//
// THE RULE, and why it is narrower than the defect.
//
// The defect is "publishes outside a delivery, with no per-event tenant, and no
// ProducerConfig.Tenant". The first clause is not statically decidable here: it
// asks whether a Publish is reachable from a bus.EventHandler rather than from a
// goroutine or an HTTP route, which is whole-program reachability across
// interface dispatch — not something a fast AST guard can answer, and a guess
// either passes a real defect or fails a correct site.
//
// So this checks the one clause that IS decidable: a non-test bus.ProducerConfig
// literal names Tenant. Sites that legitimately get their tenant another way are
// exempted BY NAME, and each entry must say WHICH ROUTE supplies it. That fails
// loudly on a checkable fact instead of looking comprehensive on an uncheckable
// one — and the four current exemptions are not debt. Three of them are
// multi-tenant by design, where a fallback would be actively WRONG: it converts
// a loud refusal into a silent mis-attribution of one tenant's order or breach
// to another's book. Naming them here is what keeps that a visible decision.
//
// LIMITS, stated so a green run is not read for more than it says.
//
//   - Syntactic, by identifier: `bus.ProducerConfig{…}` in a non-test .go file.
//     A type alias or a dot-import would not be seen — but it would also break
//     the population floor below, which is why the floor is here.
//   - It proves the FIELD IS SET, not that the value is right. `Tenant: cfg.Foo`
//     satisfies this. The per-service composition-root tests carry that half
//     (TestFeedProducerTenantIsTheServiceTenant,
//     TestVenueProducerTenantIsTheServiceTenant,
//     TestCashProducerTenantMatchesTheFolderTenant); this guard is what forces a
//     new producer to write the field down at all, at which point leaving it out
//     is a visible choice in a diff rather than an omission.
//   - An empty literal (`Tenant: ""`) is rejected, since it is the same bug
//     spelled longer.
//   - For exemptions on route 1 (delivery ctx) the guard verifies NOTHING beyond
//     the reason text. That route has no syntactic signature — its evidence is
//     that the publisher is only ever called from a handler — so the entry is
//     documentation, not a check. Route-2 exemptions do better: they name the
//     file that stamps Event.TenantID and the guard confirms it still does.
func TestEveryBusProducerConfigSetsATenant(t *testing.T) {
	root := moduleRoot(t)
	sites := scanProducerConfigs(t, root)

	// NON-VACUITY. If the scan breaks, every site passes and the guard reports
	// nothing while protecting nothing.
	if len(sites) < 14 {
		t.Fatalf("found only %d non-test bus.ProducerConfig literals — the population scan is broken, "+
			"not the module (there were 16 when this guard was written)", len(sites))
	}
	// And if it breaks the other way — matching literals that set nothing — the
	// exemption list would have to swallow the module.
	withTenant := 0
	for _, s := range sites {
		if s.setsTenant {
			withTenant++
		}
	}
	if withTenant < 10 {
		t.Fatalf("only %d of %d bus.ProducerConfig literals set Tenant — the field detector is broken "+
			"(12 of 16 did when this guard was written)", withTenant, len(sites))
	}

	seenExempt := map[string]bool{}
	var problems []string
	for _, s := range sites {
		if s.setsTenant {
			if _, exempt := producersWithoutATenantFallback[s.dir]; exempt {
				problems = append(problems, fmt.Sprintf(
					"%s:%d  is exempted by producersWithoutATenantFallback but now SETS Tenant — "+
						"the repair happened, delete the entry", s.file, s.line))
			}
			continue
		}
		route, exempt := producersWithoutATenantFallback[s.dir]
		if !exempt {
			problems = append(problems, fmt.Sprintf("%s:%d  bus.ProducerConfig does not set Tenant", s.file, s.line))
			continue
		}
		seenExempt[s.dir] = true
		// A route-2 exemption claims a per-event stamp exists. Confirm it still
		// does — an exemption whose premise has been deleted is worse than none,
		// because it reads as a decision someone checked.
		if route.stampedIn == "" {
			continue
		}
		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(route.stampedIn)))
		if err != nil {
			problems = append(problems, fmt.Sprintf(
				"%s:%d  is exempted because %s stamps Event.TenantID, but that file cannot be read: %v",
				s.file, s.line, route.stampedIn, err))
			continue
		}
		if !strings.Contains(string(body), "TenantID:") {
			problems = append(problems, fmt.Sprintf(
				"%s:%d  is exempted because %s stamps Event.TenantID per event — IT NO LONGER DOES. "+
					"That was this site's only source of a tenant, so every publish it makes is now "+
					"refused \"tenant_id required\"", s.file, s.line, route.stampedIn))
		}
	}

	sort.Strings(problems)
	if len(problems) > 0 {
		t.Errorf("%d bus.ProducerConfig problem(s):\n  %s\n\n"+
			"A producer whose ProducerConfig.Tenant is empty can only publish inside an inbound delivery "+
			"or with Event.TenantID stamped on every event. Anywhere else — an HTTP handler, a CLI, a "+
			"ticker loop — bus.Validate refuses the envelope with \"tenant_id required\" and the service "+
			"emits NOTHING while continuing to report ready. Set Tenant from the service's config (the "+
			"shape in services/accounting/cmd/accounting/main.go cashProducerConfig), or add an entry to "+
			"producersWithoutATenantFallback saying WHICH of the other two routes supplies the tenant.",
			len(problems), strings.Join(problems, "\n  "))
	}

	// DEAD-ENTRY CHECK, the same shape as metricsWithoutAWriter and
	// factPublishersWithoutARealProducer. An exemption that no longer describes
	// anything protects nothing, reads as load-bearing, and silently covers the
	// next producer that moves into its path.
	var dead []string
	for dir := range producersWithoutATenantFallback {
		if !seenExempt[dir] {
			dead = append(dead, dir)
		}
	}
	if len(dead) > 0 {
		sort.Strings(dead)
		t.Fatalf("producersWithoutATenantFallback names %d package(s) that no longer build a "+
			"bus.ProducerConfig without a Tenant: %s\n\nThe repair happened — delete the entry.",
			len(dead), strings.Join(dead, ", "))
	}
}

// tenantRoute records HOW an exempted site gets its tenant, since "it does not
// need the fallback" is only true for a specific reason and the reason is the
// thing that can stop being true.
type tenantRoute struct {
	// why names the route (1 = inbound delivery ctx, 2 = per-event
	// Event.TenantID) and the reason a fallback is absent — or wrong.
	why string
	// stampedIn is the module-relative file that must still contain a
	// `TenantID:` stamp, for route-2 entries. Empty for route-1 entries, whose
	// premise has no syntactic signature; see the guard's LIMITS.
	stampedIn string
}

// producersWithoutATenantFallback is a DEFAULT-DENY allow-list, keyed by
// module-relative package directory.
//
// IT IS NOT A BACKLOG. Unlike factPublishersWithoutARealProducer, which
// catalogued eleven packages awaiting repair, every entry here is a DECISION:
// each of these producers is multi-tenant, and giving it a ProducerConfig.Tenant
// would be a downgrade, not a fix. The fallback fires exactly when the real
// tenant is missing — so on a multi-tenant publisher it converts "this event was
// refused" into "this event was silently attributed to the wrong book". A loud
// refusal at the boundary is the better failure, and that is why these four sit
// here rather than being "fixed".
var producersWithoutATenantFallback = map[string]tenantRoute{
	"cmd/kanz-halt": {
		why: "ROUTE 2. The halt/resume CLI stamps TenantID: opt.tenant on the ModeChanged FACT " +
			"(main.go:142) and REFUSES to run without --tenant (main.go:189, \"the bus rejects an " +
			"envelope with no tenant_id on the live path\"). A fallback would be unreachable code " +
			"whose only effect would be to weaken that refusal into a default.",
		stampedIn: "cmd/kanz-halt/main.go",
	},
	"services/api-gateway/cmd/api-gateway": {
		why: "ROUTE 2, and a fallback here would be a security defect. The gateway is the platform's " +
			"identity authority: every command it publishes goes through the single Handler.publish " +
			"helper (orders.go:116), which stamps TenantID: p.Tenant — the AUTHENTICATED CALLER's " +
			"tenant, from the JWT. A ProducerConfig.Tenant would let a token carrying no tenant claim " +
			"publish an order into a default tenant's book instead of being refused. " +
			"SECOND PUBLISHER ON THE SAME PRODUCER (#352): the AUTH-01d decision recorder publishes " +
			"platform.authz.decision, and it is stamped per event by pkg/authbus/recorder.go's " +
			"tenantFor — the deciding principal's tenant, falling back to authbus.WithFallbackTenant " +
			"for a decision made with no principal at all. That fallback is scoped to the recorder " +
			"PRECISELY so it cannot reach the order path above; moving it onto ProducerConfig would " +
			"reintroduce the defect this entry exists to prevent.",
		stampedIn: "services/api-gateway/internal/orders/orders.go",
	},
	"services/lineage/cmd/lineage": {
		why: "ROUTE 2. The only publisher on this producer is the AUTH-01d decision recorder " +
			"(newBusRecorder), and pkg/authbus stamps Event.TenantID on every decision: the deciding " +
			"principal's tenant, falling back to authbus.WithFallbackTenant(cfg.Tenant) when a " +
			"decision was made with no principal. It is a recorder-scoped option rather than a " +
			"ProducerConfig field so that a decision about acme's user is filed under acme instead " +
			"of under whatever this deployment was configured with — a value that would be valid, " +
			"not theirs, and undetectable downstream.",
		stampedIn: "pkg/authbus/recorder.go",
	},
	"services/compliance/cmd/compliance": {
		why: "ROUTE 1. Both publishers (audit.BusRecorder.Record, monitor.Emitter.EmitBreach) are " +
			"reachable only from monitor.Handle, a bus.EventHandler wired to consumer.SubscribeBroadcast " +
			"(main.go:179/211), so Consumer has stashed the inbound tenant on ctx (bus/consumer.go:212). " +
			"A breach FACT must carry the tenant of the position that breached, not a service default: " +
			"the monitor keys its book on env.GetTenantId() (monitor.go:112), and a fallback would " +
			"file one tenant's breach under another's mandate.",
	},
	"services/webhook-ingest/cmd/webhook-ingest": {
		why: "ROUTE 2, indirect. Every publish this service makes goes through " +
			"internal/signal/translate, shared with the native alpha runners, which resolves " +
			"tenant := TenantOf(in.FundID) (translate.go:195) and stamps it on both the StrategySignal " +
			"FACT (:339) and every SubmitOrder command (:399). It cannot be empty: Intent.validate " +
			"rejects an empty FundID (:255) and the default TenantOf returns the fund_id (:170). " +
			"One webhook endpoint serves many funds, so a per-service fallback would be the wrong " +
			"tenant for all but one of them.",
		stampedIn: "internal/signal/translate/translate.go",
	},
}

// producerConfigSite is one non-test construction of a bus.ProducerConfig.
type producerConfigSite struct {
	dir        string // module-relative package directory, forward slashes
	file       string
	line       int
	setsTenant bool
}

// scanProducerConfigs walks the module and returns every non-test
// `bus.ProducerConfig{…}` composite literal.
//
// pkg/bus itself never appears: inside package bus the literal would be
// `ProducerConfig{}`, unqualified. That is correct — bus is where the field is
// DEFINED, and producer_test.go is where its precedence rules are proven.
func scanProducerConfigs(t *testing.T, root string) []producerConfigSite {
	t.Helper()

	var out []producerConfigSite
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules", ".gotmp":
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			// A file this guard cannot parse is a hole it cannot see through, and
			// silence here is how the next untenanted producer hides.
			return fmt.Errorf("parse %s: %w", path, perr)
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		dir := filepath.ToSlash(filepath.Dir(rel))

		for _, lit := range producerConfigLiterals(f) {
			out = append(out, producerConfigSite{
				dir:        dir,
				file:       rel,
				line:       fset.Position(lit.Pos()).Line,
				setsTenant: literalSetsNonEmptyTenant(lit),
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].file != out[j].file {
			return out[i].file < out[j].file
		}
		return out[i].line < out[j].line
	})
	return out
}

// producerConfigLiterals returns every `bus.ProducerConfig{…}` composite literal in f.
func producerConfigLiterals(f *ast.File) []*ast.CompositeLit {
	var out []*ast.CompositeLit
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "ProducerConfig" {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "bus" {
			out = append(out, lit)
		}
		return true
	})
	return out
}

// literalSetsNonEmptyTenant reports whether lit names Tenant with something
// other than an empty string literal. `Tenant: ""` is the same bug spelled
// longer, so it does not count.
func literalSetsNonEmptyTenant(lit *ast.CompositeLit) bool {
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue // unkeyed literal: no field named, so nothing to credit
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || key.Name != "Tenant" {
			continue
		}
		if b, ok := kv.Value.(*ast.BasicLit); ok && b.Kind == token.STRING &&
			(b.Value == `""` || b.Value == "``") {
			return false
		}
		return true
	}
	return false
}

// TestProducerTenantGuardDetectsTheShapesItClaimsTo is the guard's own proof of
// work.
//
// The count floors above show the scan found SOMETHING. They do not show that
// the field detector distinguishes a set Tenant from an absent one, an empty
// one, or one belonging to a different package's identically-named struct —
// and those are the four ways this guard could go quietly permissive. Running
// them through the same functions the guard uses means an edit that weakens the
// analysis fails HERE, rather than turning the real guard green for free.
func TestProducerTenantGuardDetectsTheShapesItClaimsTo(t *testing.T) {
	parse := func(name, src string) *ast.File {
		f, err := parser.ParseFile(token.NewFileSet(), name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		return f
	}
	const header = "package sample\n\nimport \"github.com/eighred/kanz/pkg/bus\"\n\n"

	cases := []struct {
		name string
		src  string
		want bool // literal found AND credited with a Tenant
	}{
		{
			name: "set from config is credited",
			src:  header + "func f() { _ = bus.ProducerConfig{Source: \"s\", ProducerVersion: \"v\", Tenant: cfg.Tenant} }",
			want: true,
		},
		{
			name: "absent Tenant is the defect",
			src:  header + "func f() { _ = bus.ProducerConfig{Source: \"s\", ProducerVersion: \"v\", Metrics: m} }",
			want: false,
		},
		{
			name: "empty string Tenant is the same bug spelled longer",
			src:  header + "func f() { _ = bus.ProducerConfig{Source: \"s\", Tenant: \"\"} }",
			want: false,
		},
		{
			name: "returned from a helper is still found",
			src:  header + "func f() bus.ProducerConfig { return bus.ProducerConfig{Source: \"s\", Tenant: t} }",
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lits := producerConfigLiterals(parse("x.go", tc.src))
			if len(lits) != 1 {
				t.Fatalf("found %d bus.ProducerConfig literals, want 1 — the population scan misses "+
					"the shape the guard is about", len(lits))
			}
			if got := literalSetsNonEmptyTenant(lits[0]); got != tc.want {
				t.Errorf("literalSetsNonEmptyTenant = %v, want %v", got, tc.want)
			}
		})
	}

	// A same-named struct from another package must not be scanned at all —
	// otherwise the guard reports foreign types and the exemption list grows to
	// silence them.
	decoy := header + "import \"example.com/other/kafka\"\n\nfunc g() { _ = kafka.ProducerConfig{Source: \"s\"} }"
	if lits := producerConfigLiterals(parse("decoy.go", decoy)); len(lits) != 0 {
		t.Errorf("a same-named struct from a different package was scanned (%d literals) — the guard "+
			"matches on the selector alone and would report the wrong type", len(lits))
	}

	// An unkeyed literal names no field, so it cannot be credited. Go already
	// breaks it the day a field is added; this makes sure it does not read as
	// compliant in the meantime.
	unkeyed := header + "func h() { _ = bus.ProducerConfig{\"s\", \"v\", \"t\", nil, nil} }"
	lits := producerConfigLiterals(parse("unkeyed.go", unkeyed))
	if len(lits) != 1 {
		t.Fatalf("unkeyed literal not found (%d)", len(lits))
	}
	if literalSetsNonEmptyTenant(lits[0]) {
		t.Error("an unkeyed bus.ProducerConfig literal was credited with setting Tenant — no field is " +
			"named there, so the guard would pass a positional literal whose third element is not the tenant")
	}
}
