package main

import (
	"log/slog"
	"sort"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/services/accounting/internal/custody"
)

// WHICH SLICE OF THE BOOK EACH CUSTODIAN'S STATEMENT IS COMPARED AGAINST (#1073).
//
// The gauge describes the configured book geometry. A declared scope names
// the accounts whose venue identities and custody assignments must be admitted.
// For a pair with journal-derived geometry, dated account approval is unknown.
// A new financial comparison requires an admitted declaration (#1074).
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
		Help: "1 for the configured account scope. Declared accounts require independent dated venue and custody evidence. " +
			"A derived scope alone cannot support a custody comparison. Entries without an exchange account remain unreconciled.",
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
		logger.Warn("accounting: journal attribution alone cannot prove a custody assignment. "+
			"New financial comparisons require independently approved accounts. Entries with no exchange account are RECONCILED BY NOTHING.",
			"portfolio", p.PortfolioID, "custodian", p.CustodianID, "basis", custodyBasisDerived,
			"declaration", "register dated venue identity and custody assignment evidence for the accounts named in ACCOUNTING_CUSTODY_ACCOUNTS",
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
