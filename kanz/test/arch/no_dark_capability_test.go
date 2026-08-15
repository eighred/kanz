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
//	      carries the grep proving nobody imports it.
//	#471  internal/validation — the SR 11-7 gate whose stated purpose is "no
//	      model serves production without recorded, current validation". It had
//	      zero importers, so every analytic served production unvalidated. Found
//	      by running this audit by hand, which is why it is now a test. RETIRED
//	      2026-08-15: internal/risk/benchmarks records the evidence and the
//	      risk-engine composition root publishes kanz_risk_analytics_validated.
//	      THE DEAD-ENTRY ARM IS WHAT CAUGHT THE STALE EXEMPTION — the repair
//	      landed and this guard refused to keep vouching for the old state.
//
// # What this checks
//
// Every package under internal/ has at least one importer somewhere in the
// module — production OR test — or an argued exemption naming the issue that
// will wire it.
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

// darkPackageExempt maps an internal package to the issue that will wire it.
//
// EVERY ENTRY NAMES AN ISSUE, and that is the whole discipline: an exemption
// that says "not yet" without saying who is tracking it is how a capability
// stays dark for a year. The dead-entry arm below deletes the entry for you when
// the package finally gets an importer.
var darkPackageExempt = map[string]string{
	"internal/prediction/registry": "#112 — the MLOPS-01a model registry. Deleting it was " +
		"proposed and REJECTED. CORRECTED 2026-08-15: it is NOT 'the last third of the AI-M1 " +
		"bridge' and what it needs is NOT a composition root. The bridge is one third wired — " +
		"the feature publisher — while the resilient inference client also has zero callers " +
		"(invisible here because this guard works at IMPORT granularity and internal/prediction " +
		"IS imported, for the publisher). CORRECTED AGAIN 2026-08-15: the transport half is no " +
		"longer missing, and the old reason here (platform.model is not a declared subject, the " +
		"topic table removed it, kanz-py's RegistryPublisher is a Protocol with no " +
		"implementation) is now false on all three counts. inference.v1.ModelRegistryEvent is " +
		"the shared wire contract, BusRegistryPublisher publishes it on " +
		"platform.model.registered, and the Kafka topic plus the broker grant are provisioned " +
		"(#112 steps 1-2). WHAT KEEPS THIS PACKAGE DARK is the Go side: Log has no binding and " +
		"nothing constructs a CoordinatedRegistry, which is #112 step 3. The correctness " +
		"property that saved it from deletion — resolve by feature_set_ref, never by name — was " +
		"asserted in three places and implemented in none; it is implemented now.",
	"internal/collateral": "#408 — the COLL-01 margin/financing plane. The maths is written and " +
		"unconsumed because an order carries no leverage and no margin mode; #408 holds the ruling " +
		"on margin semantics that decides the shape of the wiring.",
	"internal/risk/pricing/credit": "#113 — the credit calibrator carries the same Refresh seam the " +
		"scheduler drives, and is not scheduled because no live CDS quote source exists (#203). " +
		"kanz_risk_calibration_scheduled{kind=\"credit\"} reports 0 so the gap is visible.",
	"internal/risk/termsource": "#345 — the contract-terms store and TermsProvider the vol " +
		"calibration needs. Nothing joins quotes to terms yet, so nothing constructs a term source.",
	"internal/regulatory/stress": "#471 (Source section) — the REG-01c CCAR/DFAST firmwide stress " +
		"framework, in the same unwired state as the validation gate and recorded there rather " +
		"than filed separately. Worth its own issue if it is picked up independently.",
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
	internalCount := 0

	for _, p := range pkgs {
		rel, ok := strings.CutPrefix(p.ImportPath, modulePath+"/")
		if !ok || !strings.HasPrefix(rel, "internal/") {
			continue
		}
		internalCount++
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

	// NON-VACUITY: this module has a large internal tree. Finding almost none of
	// it means the package walk broke, and the guard would pass having checked
	// nothing — the exact failure mode it exists to prevent, one level up.
	if internalCount < 30 {
		t.Fatalf("found only %d packages under internal/ — the walk is broken, not the estate",
			internalCount)
	}

	if len(dark) > 0 {
		sort.Strings(dark)
		t.Errorf("%d internal package(s) have NO importer anywhere in the module — not production, "+
			"not test: %v.\n"+
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
