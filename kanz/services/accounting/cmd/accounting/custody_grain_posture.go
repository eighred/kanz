package main

import (
	"log/slog"
	"sort"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/services/accounting/internal/recon"
)

// AT WHAT GRAIN THIS DEPLOYMENT RECONCILES AGAINST A CUSTODIAN (#1049).
//
// The control compares two things now: netted positions and cash, and the
// EXECUTIONS behind them, matched by external reference against the statement's
// trade lines. The second only happens when the statement declares
// StatementGrain TRANSACTIONS.
//
// TODAY NOTHING IN THIS BUILD PUBLISHES A CustodianStatement AT ANY GRAIN, AND
// THAT IS THE SENTENCE THIS FILE EXISTS TO SAY OUT LOUD. accounting.custody.
// statement has a CONSUMER (custody.StatementConsumer), a NATS grant, a stream
// and a durable table, and no producer anywhere in the module — translating an
// MT535/MT940, a prime broker's SFTP drop or a custody API into a canonical
// statement is a feed adapter's job and is deferred to #105/#106. So every
// scheduled run concludes NO_STATEMENT until something outside this build
// publishes one.
//
// WHY A GAUGE AND NOT A COMMENT. "This deployment reconciles at trade level and
// every execution matched" and "this deployment compares netted balances and no
// execution was ever matched" are the SAME clean run from outside: the same
// outcome, the same staleness gauge, the same empty break list. An operator asked
// at 3am whether a fill that never reached the book would have been caught needs
// to read which of the two they are looking at, and a Go comment is not reachable
// from a dashboard. Same contract as kanz_accounting_settlement_basis_produced
// (#1043) and kanz_accounting_entry_source_wired (#588), both of which likewise
// report 0 for something the platform can fold and nothing produces.
//
// WHAT THIS DOES NOT DO IS INVENT A FEED, on the #345 ground this service already
// applies to corporate actions and to quotes: a reconciliation that matched
// fabricated trade lines would report a clean transaction pass that proves
// nothing, which is strictly worse than a netted pass that visibly compares
// totals. The schema, the pass and the lifecycle are built and tested; what is
// missing is a custodian that sends the lines.
//
// THE PER-RUN HALF IS ELSEWHERE, deliberately. This states the BUILD's posture at
// startup; custody.Reconciler.stateLeg states what the statement in front of each
// run actually carried, which is the only place a feed wired outside this module
// becomes visible.

// custodyGrainMetric is the series name, held as a constant because the log lines
// below cite it — an operator reading the log must be able to find the gauge.
const custodyGrainMetric = "kanz_accounting_custody_statement_grain_produced"

// custodyGrain describes one statement grain's producer posture. produced is a
// hard fact about this BUILD — no environment variable changes any of them, since
// the producer would be a feed adapter that does not exist. what names the
// component that would produce it; why and arm are the reason nothing does and
// what would change that.
type custodyGrain struct {
	produced bool
	what     string
	why      string
	arm      string
}

// custodyGrainPostures describes every grain a custodian statement can declare.
// It is keyed by recon.Grain and CONSUMED BY ITERATING recon.Grains() rather than
// by iterating this map, for the reason settlementBasisPostures is: a grain
// declared in the engine and forgotten here still gets a series (at 0, with a loud
// log), because a metric that appeared only for the grains somebody remembered
// would make a forgotten one ABSENT rather than zero — indistinguishable from a
// service that never had it.
func custodyGrainPostures() map[recon.Grain]custodyGrain {
	const noFeed = "NOTHING IN THIS BUILD PUBLISHES accounting.v1.CustodianStatement AT ALL. The subject has " +
		"a consumer, a NATS grant and a durable table, and no producer anywhere in the module: turning " +
		"an MT535/MT940, a prime broker's SFTP drop or a custody API into a canonical statement is a " +
		"feed adapter's job, for the same reason exchange protocol lives in the venue adapter, and it " +
		"is deferred to #105/#106. An adapter deployed OUTSIDE this module does publish one, and the " +
		"per-run log line from custody.Reconciler is where that becomes visible"
	return map[recon.Grain]custodyGrain{
		recon.GrainUnknown: {
			produced: false,
			what: "any producer that omits the grain field — including every statement written before " +
				"#1049 added it",
			why: noFeed,
			arm: "a feed adapter publishing a statement with no grain asserted. The transaction leg does " +
				"NOT run on one: nobody said what the feed supplies, and an empty transaction list " +
				"decides nothing",
		},
		recon.GrainBalancesOnly: {
			produced: false,
			what:     "a custodian feed adapter that supplies positions and cash and no trade lines (#105/#106)",
			why:      noFeed,
			arm: "a feed adapter declaring BALANCES_ONLY. That is a legitimate posture and a common one — " +
				"what it must never be is INFERRED from an empty transaction list, which is why the " +
				"field exists",
		},
		recon.GrainTransactions: {
			produced: false,
			what: "a custodian feed adapter supplying the complete set of trade lines for a business date " +
				"(#105/#106), or a venue trade-history poll standing in for one",
			why: noFeed,
			arm: "a feed adapter declaring TRANSACTIONS and carrying external_ref in the SAME reference " +
				"space ledger.FromFill stamps on the journal (the venue fill id). Fabricating trade " +
				"lines is NOT the fix (#345): a clean transaction pass over invented references would " +
				"assert that every fill reached the custodian, which is the one claim nobody has checked",
		},
	}
}

// classifyCustodyGrains partitions EVERY grain the engine declares into the ones
// something in this build produces and the ones nothing does. undeclared is the
// subset of unproduced that no posture describes at all — a grain someone added to
// the engine and forgot here.
func classifyCustodyGrains(postures map[recon.Grain]custodyGrain) (produced, unproduced, undeclared []recon.Grain) {
	for _, g := range recon.Grains() {
		p, declared := postures[g]
		switch {
		case !declared:
			unproduced = append(unproduced, g)
			undeclared = append(undeclared, g)
		case p.produced:
			produced = append(produced, g)
		default:
			unproduced = append(unproduced, g)
		}
	}
	return produced, unproduced, undeclared
}

// custodyGrainNames renders grains by the engine's own names, sorted. Never nil
// for a non-empty input.
func custodyGrainNames(grains []recon.Grain) []string {
	if len(grains) == 0 {
		return nil
	}
	out := make([]string, 0, len(grains))
	for _, g := range grains {
		out = append(out, g.String())
	}
	sort.Strings(out)
	return out
}

// stateCustodyGrainPosture registers kanz_accounting_custody_statement_grain_produced
// with one series for EVERY grain the engine declares, and says out loud which of
// them nothing produces. It returns the unproduced label names, sorted, so a
// caller can assert on it.
//
// REGISTERED UNCONDITIONALLY, NEVER BEHIND A BRANCH. Collectors registered inside
// an `if producer != nil` make the broker-less deployment export NO series at all,
// so a `== 0` alert is silent in exactly the state it was written for — that has
// shipped twice in this estate (#973, #963) and was caught both times only by
// running the binary. There is nothing to branch on here anyway: this posture is a
// fact about the build, not about the configuration, and the broker-less
// "read/reconcile only" deployment still answers the ad-hoc reconcile route and is
// therefore precisely a deployment whose comparison grain an operator needs to be
// able to read.
func stateCustodyGrainPosture(reg prometheus.Registerer, logger *slog.Logger) []string {
	return seedCustodyGrainPosture(reg, logger, custodyGrainPostures())
}

// seedCustodyGrainPosture is the half that does not build the posture map, split
// out so a test can hand it a map with a declared grain MISSING — the state a
// newly added grain starts in — and prove the series is still seeded. Without this
// seam the branch that catches an undescribed grain would itself be untested,
// which is the same shape of hole it exists to close.
func seedCustodyGrainPosture(reg prometheus.Registerer, logger *slog.Logger, postures map[recon.Grain]custodyGrain) []string {
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: custodyGrainMetric,
		Help: "1 when something in this build publishes a custodian statement at this grain, 0 when the " +
			"control can consume it and nothing produces one. transactions=0 means NO execution is " +
			"matched against a custodian trade line anywhere in this deployment, so a fill that never " +
			"reached the book stays inferable from a netted position difference rather than observed " +
			"on a reference (#1049).",
	}, []string{"grain"})
	reg.MustRegister(g)

	producedGrains, unproducedGrains, undeclaredGrains := classifyCustodyGrains(postures)
	undeclared := map[recon.Grain]bool{}
	for _, x := range undeclaredGrains {
		undeclared[x] = true
	}
	for _, x := range producedGrains {
		g.WithLabelValues(x.String()).Set(1)
	}
	for _, x := range unproducedGrains {
		name := x.String()
		g.WithLabelValues(name).Set(0)
		if undeclared[x] {
			// A grain was added to the engine and nobody stated its posture.
			// Seeded at 0 and SAID SO: silently omitting it would put the new
			// grain in the one state this file exists to abolish — absent rather
			// than zero.
			logger.Error("accounting: custodian statement grain has NO PRODUCER POSTURE DECLARED — it is "+
				"reported as unproduced because nothing here says otherwise; add it to "+
				"custodyGrainPostures", "grain", name)
			continue
		}
		p := postures[x]
		logger.Warn("accounting: NOTHING PRODUCES a custodian statement at this grain",
			"grain", name, "producer", p.what, "why", p.why, "would_arm_it", p.arm,
			"gauge", custodyGrainMetric+"{grain=\""+name+"\"}=0")
	}
	unproduced := custodyGrainNames(unproducedGrains)
	if len(unproduced) == 0 {
		logger.Info("accounting: every custodian statement grain the control consumes has a producer",
			"grains", custodyGrainNames(producedGrains))
		return unproduced
	}
	// WARN, not Info. "No execution in this book has ever been matched against a
	// custodian's record" is the sentence an operator must have read before they
	// treat a clean reconciliation as evidence that every fill reached the ledger,
	// and Info is where it would be filtered out.
	logger.Warn("accounting: custody reconciliation compares NETTED BALANCES and no execution — a total "+
		"is blind to its own composition, so a wrongly-booked execution and a missing one on the same "+
		"instrument produce the same correct-looking position and neither is reported (#1049)",
		"produced", custodyGrainNames(producedGrains), "not_produced", unproduced,
		"consequence", "'which of my executions has the custodian never seen' is answerable only at the "+
			"grain of an end-of-day netted instrument position; the break kinds that name an execution "+
			"exist and nothing can produce one")
	return unproduced
}
