package main

import (
	"log/slog"
	"sort"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

// ON WHAT BASIS THIS BOOK SAYS AN ENTRY HAS SETTLED (#1043).
//
// The journal now carries a settlement axis: every entry declares whether its
// legs have actually changed hands (settled), are contractually owed on a future
// date (pending), or whether nobody asserted either (unknown). The fold keeps two
// bases from the same entries — Book.Positions and Book.Cash are what the
// portfolio has TRADED, Book.SettledPositions and Book.SettledCash are what has
// actually MOVED.
//
// TODAY THOSE TWO ARE THE SAME NUMBER FOR EVERY FILL, AND THAT IS A DECISION
// RATHER THAN AN OVERSIGHT. It is the sentence this file exists to say out loud.
// Both venue adapters in this estate are crypto spot and declare it through the
// capability surface — BinanceVenue.MarginModes and OKXVenue.MarginModes each
// return MARGIN_MODE_UNSPECIFIED alone, the cash/spot regime — and spot on those
// venues settles atomically with the match. So ledger.fillSettlement asserts
// SettlementSettled at the execution time, nothing in this platform ever produces
// a PENDING entry, and the settled book equals the traded book by construction.
//
// WHY A GAUGE AND NOT A COMMENT. "This deployment books every trade as settled at
// execution" and "this deployment tracks settlement and nothing is outstanding"
// are the same observation from outside: an empty unsettled ladder either way,
// zero pending entries either way, a NAV that reconciles either way. An operator
// asked at 3am why a T+2 purchase is spendable needs to be able to read which of
// the two they are looking at, and a comment in a Go file is not reachable from a
// dashboard. Same contract as kanz_accounting_entry_source_wired (#588), which
// reports 0 for the entry types the ledger can fold and nothing produces.
//
// WHAT THIS DOES NOT DO IS WIRE A T+n CONVENTION, and inventing one is ruled out
// on the #345 ground this service already applies to corporate actions: a book
// that confidently reports a fabricated settlement date is worse than one that
// visibly settles everything at execution, because the fabricated date would be
// consumed by a buying-power gate as though somebody had established it.
//
// test/arch/settlement_basis_absence_is_stated_test.go binds the posture below to
// those two MarginModes declarations, so the justification cannot outlive the
// capability it rests on: the day a venue adapter declares a non-CASH margin mode,
// the guard fails and this file has to be re-derived rather than re-read.

// settlementBasis describes one settlement basis's producer posture. produced is
// a hard fact about this BUILD — not about this deployment, since no environment
// variable changes any of them. what names the component that produces it (or
// would); why and arm are the reason nothing does and what would change that.
type settlementBasis struct {
	produced bool
	what     string
	why      string
	arm      string
}

// settlementBasisPostures describes every settlement basis the ledger declares.
// It is keyed by ledger.SettlementBasis and CONSUMED BY ITERATING
// ledger.SettlementBases rather than by iterating this map, for the reason
// entrySourcePostures is: a basis declared in the ledger and forgotten here still
// gets a series (at 0, with a loud log), because a metric that appeared only for
// the bases someone remembered would make a forgotten one absent rather than zero
// — indistinguishable from a service that never had it.
func settlementBasisPostures() map[ledger.SettlementBasis]settlementBasis {
	return map[ledger.SettlementBasis]settlementBasis{
		ledger.SettlementSettled: {
			produced: true,
			what: "ledger.fillSettlement asserts it for every order.v1 fill, from the venue's " +
				"spot convention",
		},
		ledger.SettlementUnknown: {
			// PRODUCED, AND NOT AS A FAILURE. Three of the ledger's entry sources
			// genuinely cannot assert a basis, so they say so rather than guess.
			produced: true,
			what: "the cash consumer, corpact.CorporateAction.ToEntry and " +
				"accounting.AccrualEntry, none of which can assert a settlement basis: " +
				"accounting.v1.LedgerEntry has no settlement field, and accrued income is " +
				"earned-not-received by construction",
		},
		ledger.SettlementPending: {
			// FALSE IN EVERY DEPLOYMENT, and no environment variable changes it.
			// There is nothing to configure: no venue adapter in this estate can
			// place an instrument that settles later than it executes, so there is
			// no convention a config could name.
			produced: false,
			what:     "ledger.fillSettlement, returning a future settlement date for a T+n venue",
			why: "EVERY WIRED VENUE ADAPTER IS CRYPTO SPOT AND DECLARES IT: BinanceVenue.MarginModes " +
				"and OKXVenue.MarginModes each return MARGIN_MODE_UNSPECIFIED alone, the cash/spot " +
				"regime, and spot on both venues settles atomically with the match. So every entry " +
				"this book folds settles at execution, the settled fold equals the traded fold, and " +
				"nothing is ever traded-not-settled. The axis, the fold and the durable column are " +
				"built and tested; what is missing is an instrument that needs them",
			arm: "a venue adapter declaring a non-CASH margin mode or a T+n instrument family — then " +
				"ledger.fillSettlement returns SettlementPending with the contractual date, and " +
				"#589's confirmation plane has a column to write the custodian's answer into. " +
				"Fabricating a settlement date is NOT the fix (#345): a buying-power gate would " +
				"spend against a date nobody established",
		},
	}
}

// classifySettlementBases partitions EVERY basis the ledger declares into the
// ones something in this build produces and the ones nothing does. undeclared is
// the subset of unproduced that no posture describes at all — a basis someone
// added to the ledger and forgot here.
func classifySettlementBases(postures map[ledger.SettlementBasis]settlementBasis) (produced, unproduced, undeclared []ledger.SettlementBasis) {
	for _, basis := range ledger.SettlementBases {
		p, declared := postures[basis]
		switch {
		case !declared:
			unproduced = append(unproduced, basis)
			undeclared = append(undeclared, basis)
		case p.produced:
			produced = append(produced, basis)
		default:
			unproduced = append(unproduced, basis)
		}
	}
	return produced, unproduced, undeclared
}

// settlementBasisNames renders bases by the ledger's own names, sorted. Never nil
// for a non-empty input.
func settlementBasisNames(bases []ledger.SettlementBasis) []string {
	if len(bases) == 0 {
		return nil
	}
	out := make([]string, 0, len(bases))
	for _, b := range bases {
		out = append(out, b.String())
	}
	sort.Strings(out)
	return out
}

// stateSettlementBasisPosture registers kanz_accounting_settlement_basis_produced
// with one series for EVERY settlement basis the ledger declares, and says out
// loud which of them nothing produces. It returns the unproduced label names,
// sorted, so a caller can assert on it.
//
// REGISTERED UNCONDITIONALLY, NEVER BEHIND A BRANCH. Collectors registered inside
// an `if producer != nil` make the broker-less deployment export NO series at all,
// so a `== 0` alert is silent in exactly the state it was written for — that has
// been shipped twice in this estate and caught both times only by running the
// binary. There is nothing to branch on here anyway: this posture is a fact about
// the build, not about the configuration.
func stateSettlementBasisPosture(reg prometheus.Registerer, logger *slog.Logger) []string {
	return seedSettlementBasisPosture(reg, logger, settlementBasisPostures())
}

// seedSettlementBasisPosture is the half that does not build the posture map,
// split out so a test can hand it a map with a declared basis MISSING — the state
// a newly added basis starts in — and prove the series is still seeded. Without
// this seam the branch that catches an undescribed basis would itself be
// untested, which is the same shape of hole it exists to close.
func seedSettlementBasisPosture(reg prometheus.Registerer, logger *slog.Logger, postures map[ledger.SettlementBasis]settlementBasis) []string {
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kanz_accounting_settlement_basis_produced",
		Help: "1 when something in this build produces a journal entry on this settlement basis, 0 " +
			"when the book can fold it and nothing does. pending=0 means every entry settles at " +
			"execution because every wired venue is crypto spot — a stated posture, not an error, " +
			"and indistinguishable from a settled ladder without this series (#1043).",
	}, []string{"basis"})
	reg.MustRegister(g)

	producedBases, unproducedBases, undeclaredBases := classifySettlementBases(postures)
	undeclared := map[ledger.SettlementBasis]bool{}
	for _, b := range undeclaredBases {
		undeclared[b] = true
	}
	for _, b := range producedBases {
		g.WithLabelValues(b.String()).Set(1)
	}
	for _, b := range unproducedBases {
		name := b.String()
		g.WithLabelValues(name).Set(0)
		if undeclared[b] {
			// A basis was added to the ledger and nobody stated its posture. Seeded
			// at 0 and SAID SO: silently omitting it would put the new basis in the
			// one state this file exists to abolish — absent rather than zero.
			logger.Error("accounting: settlement basis has NO PRODUCER POSTURE DECLARED — it is "+
				"reported as unproduced because nothing here says otherwise; add it to "+
				"settlementBasisPostures", "basis", name)
			continue
		}
		p := postures[b]
		logger.Warn("accounting: NOTHING PRODUCES a journal entry on this settlement basis — the "+
			"book folds it correctly and never receives one, so the settled view and the traded "+
			"view are the same number and nothing says which one you are reading",
			"basis", name, "producer", p.what, "why", p.why, "would_arm_it", p.arm,
			"gauge", "kanz_accounting_settlement_basis_produced{basis=\""+name+"\"}=0")
	}
	unproduced := settlementBasisNames(unproducedBases)
	if len(unproduced) == 0 {
		logger.Info("accounting: every settlement basis this book folds has a producer",
			"bases", settlementBasisNames(producedBases))
		return unproduced
	}
	// WARN, not Info. "Every trade in this book is booked as settled the instant
	// it executed" is the sentence an operator needs to have read before they
	// trust a buying-power refusal or a NAV struck on trade date, and Info is
	// where it would be filtered out.
	logger.Warn("accounting: NOT every settlement basis this book folds has a producer — every "+
		"entry settles at execution, so the traded book and the settled book are the same "+
		"number and no control can tell 'we own it' from 'we have merely bought it'",
		"produced", settlementBasisNames(producedBases), "not_produced", unproduced,
		"consequence", "NAV, exposure, leverage, mandate headroom and buying power (internal/cashview) "+
			"all read a book in which every trade is final at the instant it matched")
	return unproduced
}
