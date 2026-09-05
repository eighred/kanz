package main

import (
	"log/slog"
	"sort"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/services/accounting/internal/custody"
)

// WHICH SLICE OF THE BOOK EACH CUSTODIAN'S STATEMENT IS COMPARED AGAINST (#1073).
//
// Every custody comparison is scoped to exchange accounts, and there are two ways
// a pair gets its scope:
//
//	declared  ACCOUNTING_CUSTODY_ACCOUNTS names the accounts each custodian of the
//	          portfolio holds. Required whenever a portfolio has two or more
//	          custodians, because nothing else can say which holds what, and
//	          custody.NewBookScope refuses the start without it.
//	derived   the portfolio has ONE custodian, so it holds every exchange account
//	          the journal touches and the scope is taken from the journal itself.
//	          Nothing to declare, and nothing an operator can get wrong.
//
// THIS FILE REPLACES A LOG LINE THAT SAID THE OPPOSITE. buildCustodyConfig used to
// record an undeclared portfolio at INFO as an affirmatively correct
// configuration — "compares the whole portfolio book … reason: one custodian
// configured for this portfolio" — and comparing the whole book against one
// custodian's statement is the thing that manufactured the break. An operator who
// read that line was steered away from the only fact that mattered about it.
//
// # The residue, which neither basis reconciles
//
// A journal entry may settle against NO exchange account: migration 0003 reads ''
// as the positive declaration that it touched none, which is true of an investor
// subscription into the fund's own bank and of a corporate action. No exchange
// custodian's statement can list such an entry, so it is in NO comparison basis —
// under either scope, and that is correct.
//
// IT IS ALSO REAL MONEY THAT NOTHING RECONCILES, and until #1073 neither of the
// two disagreeing rules said so: the scoped path dropped it in silence and the
// single-custodian path put it in the comparison, where it came back as a cash
// break for the full amount every run. The rule is stated here, once at startup;
// the AMOUNT is stated per run by custody.LedgerBookLoader, which is the only
// place it is knowable — it takes a journal to compute.
//
// # Why a gauge and not only a log
//
// "This portfolio's scope was declared" and "this portfolio's scope was derived
// because it has one custodian" are indistinguishable from outside the process,
// and the difference decides whether ACCOUNTING_CUSTODY_ACCOUNTS is load-bearing
// for that portfolio. Same contract as kanz_accounting_settlement_basis_produced:
// a fact about this deployment that an operator must be able to read from a
// dashboard at 3am rather than from a Go file.

// custodyBasisMetric is the series name, held as a constant because the log line
// below cites it — an operator reading the log must be able to find the gauge.
const custodyBasisMetric = "kanz_accounting_custody_comparison_basis"

// The label values are custody's own constants rather than two more string
// literals here: the loader's per-run WARN and this gauge describe ONE fact, and
// an operator correlating them needs the words to be the same word.
const (
	custodyBasisDeclared = custody.BasisDeclared
	custodyBasisDerived  = custody.BasisDerived
)

// custodyBases returns every basis a pair can be on. It exists so the seeding
// below iterates it rather than a list retyped at each use: a basis added here
// and forgotten at one call site would be ABSENT rather than zero for the pairs
// that call site covers, which is the state a gauge exists to abolish.
func custodyBases() []string { return []string{custodyBasisDeclared, custodyBasisDerived} }

// stateCustodyBasisPosture registers kanz_accounting_custody_comparison_basis with
// one series per (portfolio, custodian, basis) — BOTH bases for every pair, so the
// one not in force reads 0 rather than being absent — and says out loud, for the
// derived ones, what the derivation does and does not cover.
//
// It returns the portfolios on a derived basis, sorted, so a caller can assert on
// it.
//
// REGISTERED UNCONDITIONALLY, NEVER BEHIND A BRANCH, and called from the
// composition root beside the other postures rather than from buildCustodyPlane.
// The plane is assembled only when a broker is configured, so registering there
// would export NO series at all for the "read/reconcile only" deployment — which
// still serves the ad-hoc reconcile route, and is therefore precisely a deployment
// whose comparison basis an operator needs to be able to read. Collectors
// registered inside a branch have shipped twice in this estate and were caught
// both times only by running the binary.
func stateCustodyBasisPosture(
	reg prometheus.Registerer, logger *slog.Logger, pairs []custody.Subject, scope *custody.BookScope,
) []string {
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: custodyBasisMetric,
		Help: "1 for the basis this (portfolio, custodian) pair's book side is scoped by. " +
			"declared=1 means ACCOUNTING_CUSTODY_ACCOUNTS names the accounts the custodian holds; " +
			"derived=1 means the portfolio has one custodian and the scope is taken from the accounts " +
			"its journal touches. Under BOTH, entries that settled against no exchange account are in " +
			"no comparison basis and are reconciled by nothing (#1073).",
	}, []string{"portfolio", "custodian", "basis"})
	reg.MustRegister(g)

	derived := map[string]bool{}
	for _, p := range pairs {
		basis := custodyBasisDerived
		if scope.Declared(p.PortfolioID) {
			basis = custodyBasisDeclared
		} else {
			derived[p.PortfolioID] = true
		}
		for _, b := range custodyBases() {
			value := 0.0
			if b == basis {
				value = 1
			}
			g.WithLabelValues(p.PortfolioID, p.CustodianID, b).Set(value)
		}
		if basis == custodyBasisDeclared {
			continue
		}
		// WARN, not Info. The sentence an operator has to have read before they
		// treat a clean run as evidence the book agrees with the world is "the
		// comparison did not cover everything in it", and Info is where that gets
		// filtered out. It is not an error: the derivation is correct, and the
		// exclusion is what stops the run fabricating a break.
		logger.Warn("accounting: custody reconciliation scopes this portfolio's book from the accounts "+
			"its JOURNAL touches, because it has one custodian — nothing to declare, and nothing an "+
			"operator can get wrong. Entries that settled against NO exchange account are in no "+
			"comparison basis and are RECONCILED BY NOTHING (#1073)",
			"portfolio", p.PortfolioID, "custodian", p.CustodianID, "basis", custodyBasisDerived,
			"declaration", "ACCOUNTING_CUSTODY_ACCOUNTS is not load-bearing for this portfolio; it "+
				"becomes required the moment a second custodian is added to ACCOUNTING_CUSTODY_PAIRS",
			"unreconciled", "cash held away from every custodian — an investor subscription into the "+
				"fund's own bank — is real in the book of record and confirmed by no statement; the "+
				"amount is logged per run by the book loader",
			"gauge", custodyBasisMetric+"{portfolio=\""+p.PortfolioID+"\",basis=\""+custodyBasisDerived+"\"}=1")
	}

	out := make([]string, 0, len(derived))
	for p := range derived {
		out = append(out, p)
	}
	sort.Strings(out)
	if len(pairs) == 0 {
		return out
	}
	logger.Info("accounting: custody comparison basis stated",
		"pairs", len(pairs), "derived_portfolios", out, "gauge", custodyBasisMetric)
	return out
}
