package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// THE BOOK OF RECORD CLAIMS TO FOLD CORPORATE ACTIONS, AND HAS NEVER SEEN ONE
// (#588).
//
// services/accounting/internal/corpact builds the journal entry
// ledger.foldCorpAct already applies. Nothing constructs it:
// accounting.v1.CorporateAction has no publisher, no NATS subject and no Kafka
// topic anywhere in this module. That much is tracked by
// darkPackageExempt["services/accounting/internal/corpact"].
//
// What that exemption does NOT reach is the sentence the rest of the platform
// was built on. In five places — one manifest and four Go docs — this repository
// says some form of:
//
//	accounting's ledger is the only component that folds trade legs, cash
//	movements and corporate actions bitemporally, so it is the only one that may
//	compute a balance
//
// and uses it to FORBID two other components from computing their own: the
// pre-trade buying-power gate (#415, via internal/cashview) and
// exchange balance reconciliation (#418, via internal/venueadapter/balancerecon).
// The ruling is right and must stand — a second computation of one number drifts,
// and the drift surfaces as a trading control that refuses or admits wrongly.
// But one of its three named inputs has never arrived, and neither consumer was
// told. ledger.foldCorpAct pays dividend and coupon cash and merger
// consideration onto the held quantity, so every announced balance is short by
// every such payment the fund has received.
//
// That is a DIFFERENT defect from the dark package, and it is the more dangerous
// half: a dark package does nothing, whereas this one is actively relied upon.
// A gate reading the short balance refuses orders a portfolio can afford and
// reports it as a spending limit; a reconciliation reading it will report an
// exchange break that is really an unfed feed.
//
// # What this binds together
//
//	darkPackageExempt["services/accounting/internal/corpact"]  nothing feeds it
//	services/accounting/cmd/accounting/main.go                 the process says so
//	the five claim sites                                       the docs say so
//
// While the exemption stands, every claim site must carry the marker and the
// accounting composition root must state the posture. When a feed finally exists
// the exemption goes — and then this guard FAILS THE OTHER WAY, because five
// files telling a reader that corporate actions are missing from a balance that
// now contains them is the same defect with the sign flipped. Nothing else in
// the toolchain sees a stale disclaimer in a comment or in YAML.
//
// # Why a guard and not a comment
//
// Because the comment IS the thing being protected, and a comment is undated
// evidence. This repository has already been bitten by a comment that justified a
// trade-off whose premise had expired. Modelled on
// provisioned_stream_claims_a_producer_test.go, which does the same job for #589.

// corpactDarkPackage is the exemption key every assertion below is bound to.
const corpactDarkPackage = "services/accounting/internal/corpact"

// corpactPostureFile is the file the exemption's own text cites as the thing
// that makes the absence visible. Cited BY PATH, so the citation is checkable —
// an exemption pointing at a file that no longer exists is the stale-citation
// failure this repository has already ruled on.
var corpactPostureFile = filepath.Join(
	"services", "accounting", "cmd", "accounting", "entry_source_posture.go")

// corpactClaimMarker is the annotation every claim site must carry. It is one
// distinctive all-caps sentence rather than a phrase that neighbouring prose
// could assemble by accident: three guards in this repository have passed with
// the checked thing deleted because they matched their own explanation.
const corpactClaimMarker = "CORPORATE ACTIONS ARE NOT IN THIS BALANCE (#588)"

// corpactClaimSites are the files that tell a reader accounting folds corporate
// actions, together with what each one is load-bearing for. They are listed by
// path rather than discovered by grepping for the claim, because the claim is
// paraphrased differently at each site and a grep loose enough to find all five
// would also match this file.
var corpactClaimSites = []struct {
	path string
	why  string
}{
	{
		path: filepath.Join("infra", "nats", "tenancy.yaml"),
		why: "the accounting workload's publish grant on accounting.balance.portfolio carries the " +
			"ruling in prose, and it is what the person standing the estate up reads. They do not " +
			"open Go",
	},
	{
		path: filepath.Join("services", "accounting", "internal", "consume", "announce.go"),
		why:  "the Announcer is the producer of the balance; its doc is where the ruling is stated",
	},
	{
		path: filepath.Join("services", "accounting", "cmd", "accounting", "main.go"),
		why: "the composition root repeats the ruling where the producer is constructed, naming " +
			"#415 and #418 as the two consumers forbidden from computing their own",
	},
	{
		path: filepath.Join("internal", "cashview", "cashview.go"),
		why: "the pre-trade buying-power gate reads this view, and BuyingPowerRule fails CLOSED — " +
			"a balance short by a dividend refuses orders the portfolio can afford",
	},
	{
		path: filepath.Join("internal", "venueadapter", "balancerecon", "view.go"),
		why: "reconciliation compares this level against the exchange, and the exchange DOES see " +
			"corporate actions — so the gap surfaces there as a break that is not a break",
	},
}

// TestBalanceAuthorityClaimSaysCorporateActionsAreUnfed pins the five sites in
// both directions against the exemption.
func TestBalanceAuthorityClaimSaysCorporateActionsAreUnfed(t *testing.T) {
	root := moduleRoot(t)
	_, dark := darkPackageExempt[corpactDarkPackage]
	self := filepath.Join("test", "arch", "corporate_action_absence_is_stated_test.go")

	// Non-vacuity, before anything is read. A guard over an emptied table passes
	// perfectly, and this file carries the marker in its own source — listing
	// itself would make one site vouch for four others by reading the guard.
	if len(corpactClaimSites) < 5 {
		t.Fatalf("corpactClaimSites has %d entries, want the five that carry the balance-authority "+
			"claim: a shortened table is a guard that checks less than it says",
			len(corpactClaimSites))
	}
	for _, site := range corpactClaimSites {
		if site.path == self {
			t.Fatalf("%s lists itself as a claim site — it carries the marker in its own source and "+
				"would vouch for nothing", self)
		}
		if site.why == "" {
			t.Errorf("%s is listed with no reason: an entry that does not say what it is "+
				"load-bearing for cannot be judged when someone wants to delete it", site.path)
		}
		b, err := os.ReadFile(filepath.Join(root, site.path))
		if err != nil {
			t.Errorf("read %s: %v", site.path, err)
			continue
		}
		body := string(b)
		// Non-vacuity: an emptied or moved file satisfies the "marker removed" arm
		// for entirely the wrong reason.
		if len(strings.TrimSpace(body)) == 0 {
			t.Errorf("%s is empty — this guard would pass by reading nothing", site.path)
			continue
		}
		has := strings.Contains(body, corpactClaimMarker)

		switch {
		case dark && !has:
			t.Errorf("%s does not carry %q.\n\n%s is still exempt in darkPackageExempt, so nothing "+
				"publishes an accounting.v1.CorporateAction and no dividend, coupon or merger "+
				"payment has ever reached the journal. This file tells a reader that accounting "+
				"folds corporate actions and that nobody else may compute a balance. %s — and a "+
				"balance short by every corporate action is not a balance, it is a number that "+
				"looks like one (#588).", site.path, corpactClaimMarker, corpactDarkPackage, site.why)
		case !dark && has:
			t.Errorf("%s still carries %q, but %s is no longer exempt in darkPackageExempt.\n\n"+
				"If a corporate-action feed has been wired, this file is now telling a reader that "+
				"dividends and coupons are MISSING from a balance that contains them — and the "+
				"named remedy (check the gauge, do not compute your own) points at a posture that "+
				"should by now read 1. A stale disclaimer is the same defect as a stale claim: "+
				"remove the marker here, and with it the hard-false posture for "+
				"ledger.EntryCorporateAction in entry_source_postures.",
				site.path, corpactClaimMarker, corpactDarkPackage)
		}
	}
}

// AND THE PROCESS MUST SAY IT TOO. Four of the five sites above tell a reader to
// check kanz_accounting_entry_source_wired{type="corporate_action"} before
// trusting a balance. That gauge exists only because the accounting composition
// root calls stateEntrySourcePosture — and NOTHING ASSERTED THAT IT DOES.
// Deleting the call leaves `go build`, `go vet`, the whole
// services/accounting/... suite and the whole test/arch suite green: the tests in
// entry_source_posture_test.go call the function directly, so they prove the
// posture is CORRECT without proving it is REACHED. That is the composition-root
// blind spot this repository has shipped crashes through twice.
//
// Asserted off the AST with comments excluded, because main.go's own comment
// names the function — a guard that greps raw source would match the explanation
// and keep passing with the call deleted.
//
// Required unconditionally rather than only while the exemption stands: accrual
// is unproduced too, and a wired corporate-action feed makes this gauge report a
// 1 that is worth just as much as today's 0.
func TestAccountingCompositionRootStatesTheEntrySourcePosture(t *testing.T) {
	const fn = "stateEntrySourcePosture"
	path := filepath.Join(moduleRoot(t), "services", "accounting", "cmd", "accounting", "main.go")

	fset := token.NewFileSet()
	// No parser.ParseComments: comments are not in the AST at all here, so the
	// call can only be found by being a call.
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	var called bool
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == fn {
			called = true
		}
		return true
	})
	if !called {
		t.Fatalf("services/accounting/cmd/accounting/main.go never calls %s().\n\n"+
			"Without it kanz_accounting_entry_source_wired is never registered, so every series is "+
			"ABSENT rather than zero — and an absent series answers an alert with \"no data\", which "+
			"is the same ambiguity moved from the book into the monitoring system. The "+
			"darkPackageExempt entry for %s cites %s as the thing that makes the corporate-action "+
			"silence visible, and four other files tell an operator to read that gauge before "+
			"trusting a balance. All of them would be pointing at nothing (#588).",
			fn, corpactDarkPackage, corpactPostureFile)
	}
}

// The exemption's argument rests on a file it names. If that file is renamed or
// deleted the exemption keeps vouching for a posture nobody can find, which is
// exactly the decayed-citation failure the exemption's own text was rewritten to
// avoid when it stopped citing ledger.go by line number.
func TestCorpactExemptionCitesAPostureFileThatExists(t *testing.T) {
	reason, dark := darkPackageExempt[corpactDarkPackage]
	if !dark {
		t.Skipf("%s is wired — the exemption and its citation are gone", corpactDarkPackage)
	}
	// The exemption cites the file with a line break inside the path, so compare
	// on the base name it is guaranteed to spell in one piece.
	if base := filepath.Base(corpactPostureFile); !strings.Contains(reason, base) {
		t.Errorf("the darkPackageExempt entry for %s no longer cites %s.\n\n"+
			"Its whole argument for staying is that the absence is STATED rather than silent, and "+
			"that file is where it is stated. An exemption whose evidence a reader cannot find is "+
			"an exemption that says \"not yet\" without saying anything else.", corpactDarkPackage, base)
	}
	if _, err := os.Stat(filepath.Join(moduleRoot(t), corpactPostureFile)); err != nil {
		t.Errorf("the darkPackageExempt entry for %s cites %s, which cannot be read: %v",
			corpactDarkPackage, corpactPostureFile, err)
	}
}

// AND THE BALANCE ITSELF MUST CARRY THE POSTURE (#614).
//
// The gauge above tells an OPERATOR that corporate actions are unfed. It tells
// the pre-trade buying-power gate nothing: that gate reads a balance off the bus
// on the order-admission path, has no access to accounting's /metrics, and is
// forbidden a synchronous call into accounting (#450). Without the posture on
// the announcement it cannot tell a portfolio that is out of money from one
// whose dividend was never counted, and it refuses both with the same sentence.
//
// consume.Announcer only publishes what it is given. An EntrySourcePosture with
// both lists empty is published as NO STATEMENT — deliberately, so a caller that
// forgot cannot masquerade as a clean bill of health — which means a composition
// root that stops passing one degrades EVERY consumer to "unstated" while
// `go build`, `go vet` and both services' whole suites stay green. That is the
// composition-root blind spot this repository has shipped crashes through twice,
// and the reason this is asserted here rather than in a unit test that
// constructs its own Announcer.
//
// Asserted off the AST with comments excluded, for the reason the assertion
// above is: main.go's own comment names the function, and a guard that grepped
// raw source would match the explanation and keep passing with the argument
// deleted.
func TestAccountingAnnouncesTheEntrySourcePostureOnEveryBalance(t *testing.T) {
	const (
		constructor = "NewAnnouncer"
		posture     = "entrySourceCompleteness"
	)
	path := filepath.Join(moduleRoot(t), "services", "accounting", "cmd", "accounting", "main.go")

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	var constructed, stated bool
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != constructor {
			return true
		}
		constructed = true
		for _, arg := range call.Args {
			inner, ok := arg.(*ast.CallExpr)
			if !ok {
				continue
			}
			if id, ok := inner.Fun.(*ast.Ident); ok && id.Name == posture {
				stated = true
			}
		}
		return true
	})

	// Non-vacuity: a renamed or removed constructor would satisfy the assertion
	// below by never reaching it.
	if !constructed {
		t.Fatalf("services/accounting/cmd/accounting/main.go never calls consume.%s — this guard "+
			"found nothing to check. If the announcer moved, move this assertion with it.",
			constructor)
	}
	if !stated {
		t.Fatalf("consume.%s is constructed in main.go without %s(cfg).\n\n"+
			"Every accounting.balance.portfolio announcement then carries no completeness, and "+
			"every consumer reads it as UNSTATED. The one that matters is the pre-trade "+
			"buying-power gate: it fails CLOSED on a balance that is short by every dividend, "+
			"coupon and merger payment this platform has never ingested (#588), and without the "+
			"posture its refusal is worded exactly like a mandate's spending limit being hit. "+
			"Nothing else in the toolchain notices — the announcer still publishes, the suites "+
			"stay green, and the only symptom is orders refused for a reason nobody can "+
			"attribute (#614).", constructor, posture)
	}
}
