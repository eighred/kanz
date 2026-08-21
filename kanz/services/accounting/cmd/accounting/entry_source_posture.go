package main

import (
	"log/slog"
	"sort"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/services/accounting/internal/config"
	"github.com/eighred/kanz/services/accounting/internal/consume"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

// WHICH KINDS OF ENTRY ANYTHING ACTUALLY PRODUCES (#588).
//
// The ledger folds six kinds of journal entry and every fold is written, tested
// and reachable. Two of them have no producer anywhere in the module:
//
//	corporate_action  ledger.foldCorpAct is live; corpact builds the entry it
//	                  folds; NOTHING CONSTRUCTS corpact. accounting.v1
//	                  .CorporateAction has no publisher, no NATS subject and no
//	                  Kafka topic, and this composition root subscribes fills,
//	                  cash and FX only. So no split, dividend, merger or coupon
//	                  has ever adjusted a position or a cash balance.
//	accrual           accounting.AccrualEntry has no caller either: nothing
//	                  schedules the daily posting, and it would need a known
//	                  income amount (a coupon schedule or a declared dividend) to
//	                  post, which is the same missing upstream.
//
// A BOOK THAT NEVER ADJUSTS AND A QUIET QUARTER LOOK IDENTICAL, and that is the
// whole defect: positions and cash drift from reality on every corporate action
// while the service reports healthy, /readyz answers 200, and NAV materializes
// from a journal that looks right. nav.go's corporate_action attribution
// component reads 0, which an operator reads as "none occurred" rather than
// "none were ever ingested" — it is a residual, so it cannot tell them apart.
//
// WHAT THIS DOES NOT DO IS WIRE THEM. There is no corporate-actions announcement
// feed in this estate — no vendor Source, no venue reference endpoint, no ingest
// path — and inventing one is ruled out on the #345 ground this repository
// already applies to invented quotes: a SUCCESSFUL fold of fabricated
// announcements is worse than no fold, because the book is then confidently
// wrong rather than visibly untouched, and a corporate action RESTATES a client's
// positions. The posture is stated instead: unwired, with the reason and what
// would arm it, once at startup and continuously as a gauge.
//
// WHAT IS NOT KNOWABLE HERE, stated so nobody re-derives it: this cannot mark
// the POSITIONS that an unprocessed corporate action has distorted. Knowing that
// AAPL split needs an announcement — the very thing that is missing — and every
// proxy for it (a price series gap, a quantity that stops reconciling) is a
// guess about a client's holdings dressed as a fact. The honest granularity is
// the deployment, not the position: this book does not process corporate
// actions, so treat EVERY position in an instrument that may have had one as
// unadjusted.

// entrySource describes one entry type's producer posture. wired is per
// DEPLOYMENT for the types that have a producer (a build with no broker folds no
// fills either) and a hard false for the two that have none in any deployment.
type entrySource struct {
	wired bool
	// what produces this entry type, when something does.
	what string
	// why nothing does, and what would arm it. Empty when wired.
	why string
	arm string
}

// entrySourcePostures describes every entry type this build can fold. It is
// keyed by ledger.EntryType and CONSUMED BY ITERATING ledger.EntryTypes rather
// than by iterating this map: a type declared in the ledger and forgotten here
// still gets a series (at 0, with a loud log), because a metric that appeared
// only for the types someone remembered would make a forgotten one absent rather
// than zero — indistinguishable from a service that never had it.
func entrySourcePostures(cfg config.Config) map[ledger.EntryType]entrySource {
	broker := cfg.NATSURL != ""
	return map[ledger.EntryType]entrySource{
		ledger.EntryTrade: {
			wired: broker && len(cfg.FillSubjects) > 0,
			what:  "the fill consumer folds order.v1 fill FACTs (WIRE-01b)",
			why:   "no ACCOUNTING_NATS_URL, or no fill subjects configured — this deployment serves read/reconcile only",
			arm:   "set ACCOUNTING_NATS_URL and ACCOUNTING_FILL_SUBJECTS",
		},
		ledger.EntryCash: {
			wired: broker && len(cfg.CashSubjects) > 0,
			what:  "the cash consumer folds accounting.v1 subscription/redemption FACTs (WIRE-01f)",
			why:   "no ACCOUNTING_NATS_URL, or no cash subjects configured",
			arm:   "set ACCOUNTING_NATS_URL and ACCOUNTING_CASH_SUBJECTS",
		},
		ledger.EntryFee: {
			// Same subjects, same decoder: decodeCash maps the fee event type off
			// the cash subjects, so fee is wired exactly when cash is.
			wired: broker && len(cfg.CashSubjects) > 0,
			what:  "the cash consumer folds accounting.v1 fee FACTs (WIRE-01f)",
			why:   "no ACCOUNTING_NATS_URL, or no cash subjects configured",
			arm:   "set ACCOUNTING_NATS_URL and ACCOUNTING_CASH_SUBJECTS",
		},
		ledger.EntryCorporateAction: {
			// FALSE IN EVERY DEPLOYMENT, not just this one, and no environment
			// variable changes it. There is nothing to configure: no publisher of
			// accounting.v1.CorporateAction exists in the module, so there is no
			// subject a config could name.
			wired: false,
			what:  "corpact.CorporateAction.ToEntry, folded by ledger.foldCorpAct",
			why: "NOTHING ANNOUNCES A CORPORATE ACTION anywhere in this platform: " +
				"accounting.v1.CorporateAction has no publisher, no NATS subject and no Kafka topic, " +
				"and this service subscribes fills, cash and FX only. The maths, the bitemporal " +
				"restatement and the fold are written and tested; what is missing is upstream of them",
			arm: "a corporate-actions announcement feed (#588) — a vendor or custodian source " +
				"publishing accounting.v1.CorporateAction, then a subject here to subscribe. " +
				"Fabricating announcements is NOT the fix (#345): a successful fold of invented " +
				"actions restates a client's positions on evidence nobody has",
		},
		ledger.EntryAccrual: {
			wired: false,
			what:  "accounting.AccrualEntry, posting straight-line accrued income",
			why: "nothing calls AccrualEntry: no job schedules the daily posting, and it needs a " +
				"KNOWN income amount (a coupon schedule or a declared dividend) to post one",
			arm: "the same announcement feed as corporate_action, plus contract terms for coupon " +
				"schedules (#509 — internal/marketdata/termsload is the loader, itself unwired)",
		},
	}
}

// classifyEntrySources partitions EVERY entry type the ledger declares into the
// ones something in this build produces and the ones nothing does. undeclared is
// the subset of unproduced that no posture describes at all — a type someone
// added to the ledger and forgot here.
//
// ONE CLASSIFICATION, TWO READERS. The gauge (seedEntrySourcePosture) and the
// statement carried on every cash announcement (entrySourceCompleteness) must
// never be able to disagree: an operator told by /metrics that corporate actions
// are unfed, while the balances say they are fed, is worse off than one told
// nothing. So neither reader iterates the posture map itself.
//
// It iterates ledger.EntryTypes rather than the map for the reason the gauge
// does: a type declared in the ledger and forgotten here is still reported, as
// unproduced, rather than being silently absent from both readers.
func classifyEntrySources(postures map[ledger.EntryType]entrySource) (produced, unproduced, undeclared []ledger.EntryType) {
	for _, typ := range ledger.EntryTypes {
		// The zero value is not a source: it is what an unset Type decodes to, and
		// an entry carrying it is a decoder bug rather than a feed. Named as the
		// exact constant so a NEW type can never fall through this skip.
		if typ == ledger.EntryUnspecified {
			continue
		}
		p, declared := postures[typ]
		switch {
		case !declared:
			unproduced = append(unproduced, typ)
			undeclared = append(undeclared, typ)
		case p.wired:
			produced = append(produced, typ)
		default:
			unproduced = append(unproduced, typ)
		}
	}
	return produced, unproduced, undeclared
}

// entrySourceCompleteness is the same posture the gauge reports, in the shape
// that rides on every cash-balance announcement (#614) — so a consumer of a
// balance learns what it omits from the balance itself, rather than being
// expected to scrape the producer's /metrics before trusting a number on the
// order-admission path.
func entrySourceCompleteness(cfg config.Config) consume.EntrySourcePosture {
	produced, unproduced, _ := classifyEntrySources(entrySourcePostures(cfg))
	return consume.EntrySourcePosture{
		Produced:   entryTypeNames(produced),
		Unproduced: entryTypeNames(unproduced),
	}
}

// entryTypeNames renders entry types by the book's own names, sorted. Never nil
// for a non-empty input, and nil for an empty one: an empty list on the wire is
// how "this deployment feeds everything it folds" is spelled.
func entryTypeNames(types []ledger.EntryType) []string {
	if len(types) == 0 {
		return nil
	}
	out := make([]string, 0, len(types))
	for _, t := range types {
		out = append(out, t.String())
	}
	sort.Strings(out)
	return out
}

// stateEntrySourcePosture registers kanz_accounting_entry_source_wired with one
// series for EVERY entry type the ledger declares, and says out loud which of
// them nothing produces. It returns the unwired label names, sorted — the same
// list it logs — so a caller can assert on it.
//
// THE GAUGE IS THE DURABLE HALF. A startup line is gone at the next rollout;
// `kanz_accounting_entry_source_wired{type="corporate_action"} == 0` is queryable
// at 3am when a desk asks why a position did not double after a 2:1 split. Same
// contract as kanz_risk_calibration_scheduled (#113), which reports 0 for the
// calibrations that exist and are not scheduled rather than omitting them.
func stateEntrySourcePosture(reg prometheus.Registerer, logger *slog.Logger, cfg config.Config) []string {
	return seedEntrySourcePosture(reg, logger, entrySourcePostures(cfg))
}

// seedEntrySourcePosture is the half that does not read config, split out so a
// test can hand it a map with a declared entry type MISSING — the state a newly
// added type starts in — and prove the series is still seeded. Without this seam
// the branch that catches an undescribed type would itself be untested, which is
// the same shape of hole it exists to close.
func seedEntrySourcePosture(reg prometheus.Registerer, logger *slog.Logger, postures map[ledger.EntryType]entrySource) []string {
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kanz_accounting_entry_source_wired",
		Help: "1 when something in this deployment produces this kind of journal entry, 0 when the " +
			"ledger can fold it but nothing feeds it. A zero is a posture, not an error — but a book " +
			"that never adjusts looks exactly like one with nothing to adjust (#588).",
	}, []string{"type"})
	reg.MustRegister(g)

	producedTypes, unproducedTypes, undeclaredTypes := classifyEntrySources(postures)
	undeclared := map[ledger.EntryType]bool{}
	for _, typ := range undeclaredTypes {
		undeclared[typ] = true
	}
	for _, typ := range producedTypes {
		g.WithLabelValues(typ.String()).Set(1)
	}
	for _, typ := range unproducedTypes {
		name := typ.String()
		g.WithLabelValues(name).Set(0)
		if undeclared[typ] {
			// A type was added to the ledger and nobody stated its posture. Seeded
			// at 0 and SAID SO: silently omitting it would put the new type in the
			// one state this whole file exists to abolish — absent rather than zero.
			logger.Error("accounting: journal entry type has NO PRODUCER POSTURE DECLARED — it is "+
				"reported as unwired because nothing here says otherwise; add it to "+
				"entrySourcePostures", "type", name)
			continue
		}
		p := postures[typ]
		logger.Warn("accounting: NOTHING PRODUCES this kind of journal entry — the ledger folds it "+
			"correctly and never receives one, so the book silently carries on as though none "+
			"occurred", "type", name, "fold", p.what, "why", p.why, "would_arm_it", p.arm,
			"gauge", "kanz_accounting_entry_source_wired{type=\""+name+"\"}=0")
	}
	wired := entryTypeNames(producedTypes)
	unwired := entryTypeNames(unproducedTypes)

	if len(unwired) == 0 {
		logger.Info("accounting: every journal entry type this book folds has a producer", "types", wired)
		return unwired
	}
	// WARN, not Info. "This deployment does not process corporate actions" is the
	// sentence an operator needs to have read before they trust a position after
	// an ex-date, and Info is where it would be filtered out.
	logger.Warn("accounting: NOT every journal entry type this book folds has a producer — the "+
		"unproduced ones adjust nothing, and a book that never adjusts looks exactly like one "+
		"with nothing to adjust",
		"produced", wired, "not_produced", unwired,
		"consequence", "positions and cash drift from reality on every event of an unproduced kind, "+
			"and nav.go's corporate_action attribution component reads 0 as though none occurred")
	return unwired
}
