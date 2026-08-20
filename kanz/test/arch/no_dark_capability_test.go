package arch

import (
	"sort"
	"strings"
	"testing"
)

// A CAPABILITY NOBODY CALLS MUST BE TRACKED, NOT MERELY PRESENT.
//
// This platform keeps building controls and then not connecting them. The
// pattern is consistent, and it is invisible from every signal the repository
// has: the code compiles, its own tests pass, `go vet` is clean, and nothing
// anywhere says the thing does not run.
//
//	#74   internal/risk/unwind — could size what a breached portfolio must shed.
//	      Built, tested, and never called for months. From outside, a decider
//	      nothing invokes is indistinguishable from one that does not work.
//	#112  internal/prediction/registry — the MLOPS-01a model gate. Its own doc
//	      carried the grep proving nobody imported it. RETIRED 2026-08-19: the
//	      risk-engine composition root constructs a CoordinatedRegistry over the
//	      platform.model.registered log and reports whether a model serves the
//	      feature contract it publishes on. NOTE THE LIMIT, in the same spirit as
//	      the #471 stress entry below: risk-engine FOLLOWS the registry, it does
//	      not RESOLVE a model to score against — the caller that would is
//	      internal/prediction's inference client, which still has no constructor
//	      anywhere. This guard cannot see that difference (import granularity,
//	      #509), and its sibling served_rpc_has_a_caller_test.go cannot either
//	      (call granularity). The distinction is recorded here so a deleted entry
//	      does not read as "Go scores predictions".
//	#471  internal/regulatory/stress — the REG-01c CCAR/DFAST framework. Its
//	      exemption is gone because internal/risk/benchmarks now grades it, which
//	      gives it an importer reached from the risk-engine composition root.
//	      NOTE THE LIMIT OF THAT: being BENCHMARKED is not being WIRED. Nothing
//	      runs a firmwide stress scenario; what changed is that the maths is now
//	      checked rather than merely present. This guard cannot see the
//	      difference — it is the import-granularity blind spot #509 names — and
//	      the distinction is recorded here so the deleted entry does not read as
//	      "the stress framework is in production".
//	#471  internal/validation — the SR 11-7 gate whose stated purpose is "no
//	      model serves production without recorded, current validation". It had
//	      zero importers, so every analytic served production unvalidated. Found
//	      by running this audit by hand, which is why it is now a test. RETIRED
//	      2026-08-15: internal/risk/benchmarks records the evidence and the
//	      risk-engine composition root publishes kanz_risk_analytics_validated.
//	      THE DEAD-ENTRY ARM IS WHAT CAUGHT THE STALE EXEMPTION — the repair
//	      landed and this guard refused to keep vouching for the old state.
//	#583  services/<name>/internal/ WAS NOT SCANNED AT ALL until this entry. The
//	      scope test matched a leading "internal/", so 134 packages across 27
//	      services — every service's own private tree — sat outside it. #539 had
//	      already written that limitation down in prose, as the explanation for a
//	      defect this guard missed, and it was never turned into coverage. Three
//	      packages were dark in the blind spot when it was closed: corpact (#588),
//	      posttrade (#589) and revocation (#108), all three exempted below.
//	      THE SHAPE OF THAT BUG IS THIS GUARD'S OWN, one level up: it compiled, it
//	      passed, and it said nothing, because "not looked at" and "looked at and
//	      clean" were indistinguishable from outside. That is why the non-vacuity
//	      arms below count the two trees SEPARATELY — one combined total would
//	      have stayed comfortably above any threshold while the services tree
//	      contributed zero.
//
// # What this checks
//
// Every package under internal/ — the module's root internal/ tree AND each
// service's own services/<name>/internal/ tree — has at least one importer
// somewhere in the module — production OR test — or an argued exemption naming
// the issue that will wire it.
//
// # Why both trees
//
// A service's internal/ tree is where code in this platform STARTS. CLAUDE.md's
// promotion rule is explicit: shared code lives in a service's internal/ and
// moves to kanz/internal/ or kanz/pkg/ only when a SECOND consumer appears. So a
// capability spends its dark period under services/<name>/internal/ by
// construction — it is the tree most likely to hold an unwired control, and it
// was the tree nobody was watching.
//
// # What it is not
//
// It is NOT a dead-code check, and it must not become one. The repository has
// already ruled on deletion, in internal/prediction/registry's own doc:
//
//	Removing it would freeze a documented three-part design permanently at two
//	thirds and turn "wire it" into "rebuild it".
//
// So the remedy for a failure here is a tracked exemption, not `rm -rf`. What
// the guard buys is that the NEXT unwired control is a visible decision in a
// diff, rather than something discovered years later by someone auditing
// imports by hand.
//
// # Why importers rather than callers
//
// An import is what the toolchain can see cheaply and exactly. A package can be
// imported and still have a dead function inside it, which this will not catch —
// but every case above was dark at the IMPORT level, because that is what
// "nobody wired it" actually looks like.

// darkPackageExempt maps an internal package — root or per-service — to the
// issue that will wire it. Keys are module-relative package paths, so a service
// entry reads services/<name>/internal/<pkg>.
//
// EVERY ENTRY NAMES AN ISSUE, and that is the whole discipline: an exemption
// that says "not yet" without saying who is tracking it is how a capability
// stays dark for a year. The dead-entry arm below deletes the entry for you when
// the package finally gets an importer.
var darkPackageExempt = map[string]string{
	"internal/risk/spotsource": "#509 — the production compute.SpotProvider, one of the two " +
		"seams RegisterGreeks needs. It is dark because the OTHER one cannot be built: the vol " +
		"surface needs a volsurface.QuoteProvider, which needs observed option PREMIUMS, and no " +
		"path in this repository carries them — market-ingest subscribes one websocket per " +
		"configured (instrument, venue) and the deployed maps are spot-only, the vendor Source is " +
		"an unimplemented SDK seam, and backfill writes bars rather than price_observations. " +
		"Building a QuoteProvider anyway would mean inventing premiums, which #345 rules out in " +
		"terms this file already quotes for XVA: a SUCCESSFUL calibration of invented quotes is " +
		"worse than a failed one. So this ships graded and unwired rather than being held back " +
		"until the feed exists — it is the half that is real, and holding it would mean rewriting " +
		"it later from the same evidence.",
	"internal/marketdata/termsload": "#509 — the contract-terms LOADER, and the writer the " +
		"contract_terms store has never had. It is dark for one step: what remains is a CLI to " +
		"run it (the kanz-backfill shape — pull venue reference data into a store), which is " +
		"deliberately not written blind, because it cannot be exercised here against the venue " +
		"endpoint it exists to read. Until it is wired the FI measures keep computing over an " +
		"empty table, and kanz_risk_fi_terms_missing_total is the only thing that says so.",
	"internal/collateral": "#408 — the COLL-01 margin/financing plane. The maths is written and " +
		"unconsumed because an order carries no leverage and no margin mode; #408 holds the ruling " +
		"on margin semantics that decides the shape of the wiring.",
	"internal/risk/pricing/credit": "#113 — the credit calibrator carries the same Refresh seam the " +
		"scheduler drives, and is not scheduled because no live CDS quote source exists (#203). " +
		"kanz_risk_calibration_scheduled{kind=\"credit\"} reports 0 so the gap is visible.",
	"internal/marketdata/indicator": "#416 C2 — the technical-indicator library the retired " +
		"alpha house rule depends on. It is dark for ONE STEP and the consumer is named: C2's first " +
		"alpha.Engine, which needs the score contract (P(return >= X%% within horizon H) plus a " +
		"calibration test) that C1 deliberately does not decide. WIRING IT INTO dataset.Materializer " +
		"WOULD NOT MAKE IT LIVE and is the obvious wrong fix: Materializer is the declared consumer " +
		"of the FeatureSource seam this implements, and it has zero production callers of its own " +
		"(#509's finding, one tree over) — so the import would satisfy this guard while changing " +
		"nothing about whether an indicator reaches a decision.",

	// The three below were dark in the services/*/internal blind spot #583 closed.
	"services/accounting/internal/corpact": "#588 — the IBOR-01c corporate-action processor. It " +
		"builds the journal entry ledger.foldCorpAct already knows how to apply, and ledger.go:109 " +
		"names this package as that builder: the FOLD is live and reachable, the BUILDER is not. It " +
		"is dark because NOTHING ANNOUNCES A CORPORATE ACTION — accounting.v1.CorporateAction has no " +
		"publisher anywhere in the module, no NATS subject and no Kafka topic, and the accounting " +
		"composition root subscribes fills, cash and FX only. Wiring it over a fabricated " +
		"announcement stream is the obvious wrong fix, on the #345 ground this file already quotes " +
		"for XVA: a SUCCESSFUL fold of invented corporate actions is worse than no fold, because the " +
		"book is then confidently wrong rather than visibly untouched. So today no split, dividend, " +
		"merger or coupon adjusts the book — and the ABSENCE IS NOW STATED rather than silent: the " +
		"accounting composition root seeds kanz_accounting_entry_source_wired{type=\"corporate_action\"} " +
		"at 0 with a startup WARN naming the missing feed (services/accounting/cmd/accounting/" +
		"entry_source_posture.go). That makes 'this deployment does not process corporate actions' " +
		"distinguishable from 'it does, and none occurred', which nav.go's corporate_action " +
		"attribution component cannot do — it is a RESIDUAL, so a 0 there means neither. The " +
		"posture is not the wiring, so this entry stays until a feed exists.",
	"services/oms/internal/posttrade": "#589 — the POST-01/PARITY-04 post-trade plane: confirmation " +
		"matching, settlement instruction generation, T+N tracking and business-day fail aging. THE " +
		"ESTATE FOR IT IS ALREADY PROVISIONED and that is the sharp part — infra/nats/" +
		"bootstrap-job.yaml creates the SETTLEMENT stream and infra/nats/tenancy.yaml grants publish " +
		"on settlement.instruction.fail, naming posttrade.BusFailSink by symbol — while no publisher " +
		"was ever constructed, so that stream is permanently empty and reads as 'nothing failed'. It " +
		"is blocked on a COUNTERPARTY confirmation feed: nothing here receives a confirmation from a " +
		"broker, custodian or CSD, and SimSettlementVenue simulates the settlement-venue half only. " +
		"NOTE WHAT WIRING THE CALENDAR ALONE WOULD NOT FIX, since #583 raised it as the small-change " +
		"candidate: DetectFailsWithCalendar already consumes it, and there is no settlement dating " +
		"anywhere else in the module to give it a second caller — what is dark is the plane, not the " +
		"calendar inside it.",
	"services/operator/internal/revocation": "#108 — the SOV-04 revocation ORDER (halt, then scale " +
		"to zero, then purge Vault-CSI secrets) and the abort rule that stops on a failed halt. It " +
		"has no production caller BY DESIGN, and this entry is the record of that decision rather " +
		"than an apology for it: none of the three verbs is implemented here, no revoke RPC exists " +
		"to reach it, and the Estate seam is cluster-shaped — its correctness is only observable " +
		"against a live cluster this repository has never had reachable from a test (#90, #92). What " +
		"IS decidable with no cluster is the safety property, and that is what ships: scaling a " +
		"tenant's OMS to zero while orders are live at a venue abandons them mid-flight, so halt " +
		"strictly precedes everything and nothing rolls back. Settled and tested BEFORE the " +
		"mechanical half exists to be misused, not after.",
}

// isRootInternalTree reports whether rel — a module-relative package path — is
// the module's root internal package or anything beneath it.
func isRootInternalTree(rel string) bool {
	return rel == "internal" || strings.HasPrefix(rel, "internal/")
}

// isServiceInternalTree reports whether rel is in some service's own private
// tree: services/<name>/internal, or anything beneath it.
//
// It matches on the THIRD path element rather than a prefix, for two reasons
// that a HasPrefix("services/") test gets wrong in opposite directions. It must
// include services/accounting/internal, which is a package AT the tree root
// rather than under it — the trailing-slash form silently drops it. And it must
// exclude services/<name>/cmd/..., which is composition-root wiring: a main
// package has no importer by definition, so sweeping it in would make every
// service binary look dark and force 27 meaningless exemptions.
func isServiceInternalTree(rel string) bool {
	parts := strings.Split(rel, "/")
	return len(parts) >= 3 && parts[0] == "services" && parts[2] == "internal"
}

func TestNoInternalCapabilityIsDarkAndUntracked(t *testing.T) {
	pkgs := loadPackages(t)

	// Every import edge in the module, from production AND test files. A package
	// used only by tests is still reachable and still exercised — dark means
	// nothing at all refers to it.
	imported := map[string]bool{}
	for _, p := range pkgs {
		for _, group := range [][]string{p.Imports, p.TestImports, p.XTestImports} {
			for _, dep := range group {
				// Self-imports do not count: a package's own external test
				// package imports it, which would make every package look live.
				if dep != p.ImportPath {
					imported[dep] = true
				}
			}
		}
	}

	var dark []string
	seenExempt := map[string]bool{}
	rootCount, serviceCount := 0, 0

	for _, p := range pkgs {
		rel, ok := strings.CutPrefix(p.ImportPath, modulePath+"/")
		if !ok {
			continue
		}
		switch {
		case isRootInternalTree(rel):
			rootCount++
		case isServiceInternalTree(rel):
			serviceCount++
		default:
			continue
		}
		if imported[p.ImportPath] {
			continue
		}
		if reason, ok := darkPackageExempt[rel]; ok {
			seenExempt[rel] = true
			t.Logf("%s: dark, tracked — %s", rel, reason)
			continue
		}
		dark = append(dark, rel)
	}

	// NON-VACUITY, COUNTED PER TREE. This module has a large internal tree and a
	// large per-service one. Finding almost none of either means the package walk
	// broke, and the guard would pass having checked nothing — the exact failure
	// mode it exists to prevent, one level up.
	//
	// The two counts are deliberately NOT summed. A single total is what let the
	// services blind spot survive: the root tree alone clears any sane threshold,
	// so a scope test that matched zero service packages would have passed a
	// combined check while checking 27 services' worth of nothing. Separate arms
	// are the only way "we did not look" and "we looked and it was clean" stay
	// distinguishable here.
	if rootCount < 30 {
		t.Fatalf("found only %d packages under internal/ — the walk is broken, not the estate",
			rootCount)
	}
	if serviceCount < 50 {
		t.Fatalf("found only %d packages under services/*/internal/ — the walk is broken, not the "+
			"estate. Every one of this module's services carries a private internal tree (134 "+
			"packages across 27 services when #583 widened this guard), so a scan finding almost "+
			"none of them has stopped matching and would otherwise pass having checked nothing.",
			serviceCount)
	}

	if len(dark) > 0 {
		sort.Strings(dark)
		t.Errorf("%d internal package(s) — root internal/ or services/<name>/internal/ — have NO "+
			"importer anywhere in the module, not production and not test: %v.\n"+
			"A capability nobody calls is invisible to every signal this repository has: it "+
			"compiles, its own tests pass, vet is clean, and nothing says it does not run. That is "+
			"how internal/risk/unwind could size a breached portfolio's reduction for months "+
			"without ever being asked (#74), and how the SR 11-7 validation gate came to sit "+
			"unreachable while every analytic served production unvalidated (#471).\n"+
			"Wire it, or add an entry to darkPackageExempt NAMING THE ISSUE that will. Do not "+
			"delete it to make this pass — that ruling has already been made and rejected.",
			len(dark), dark)
	}

	// DEAD-ENTRY ARM, and this one is the happy path: an exemption for a package
	// that now HAS an importer means somebody wired it. Delete the entry.
	for rel, reason := range darkPackageExempt {
		if !seenExempt[rel] {
			t.Errorf("exemption for %q is stale — the package now has an importer, or it no longer "+
				"exists. Delete the entry (%s)", rel, reason)
		}
	}
}
