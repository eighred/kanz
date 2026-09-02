package main

import (
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"

	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/compliancebus"
	"github.com/eighred/kanz/internal/marketdata/mark"
	"github.com/eighred/kanz/internal/refdata"
	"github.com/eighred/kanz/services/oms/internal/compliance"
	"github.com/eighred/kanz/services/oms/internal/config"
)

// THE PRE-TRADE GATE IS BUILT HERE, NOT INSIDE runConsumers (#643).
//
// # What being inside a 1,400-line function cost
//
// The gate's constructor takes six positional arguments, and two of them arrived
// as bare `nil` on one line in the middle of the composition root. An AST scan of
// the whole function found exactly three bare nils and all three were on that
// line:
//
//   - arg 4, the instrument Classifier — every SECTOR and ISSUER mandate limit
//     passed silently, for months, because the dimension they are written
//     against could not be resolved (#640, since repaired);
//   - arg 5, the DecisionRecorder — the enforcement point that decides whether
//     capital MOVES recorded nothing anywhere, while internal/compliance's own
//     type doc claimed "every decision is recorded, pass or reject, so the audit
//     trail is complete".
//
// Six lines below those nils, the same call wires the margin source with a
// paragraph explaining why a nil seam is unacceptable. The author of that
// paragraph was looking at the nils while writing it. At 1,400 lines they are not
// visible as a set.
//
// # Why this is a builder with a return value rather than a shorter function
//
// The estate's compensating control for untestable composition is 52 arch guards
// that parse cmd/ as source text. That strategy works for properties somebody
// thought to encode, and it structurally cannot cover "this seam should not have
// been nil" — a nil argument is syntactically identical to a deliberate one.
//
// A builder that RETURNS the thing it built can be called by a test, and
// comp.PreTradeGate.Posture reports which seams it holds. pretrade_test.go
// asserts the gate this function returns carries a recorder and a margin source
// on every deployment, and a classifier on any deployment that configures a
// security master. That test was impossible to write while these fifty lines
// lived inside runConsumers.

// preTradeDeps are the live-state seams the gate reads. They are passed in
// because they are folds the composition root already owns and shares with other
// consumers — the position book, the mandate registry, the margin view.
//
// EVERYTHING THIS BUILDER CAN DECIDE FOR ITSELF IS NOT IN HERE, deliberately.
// The classifier, the decision recorder and the observer counters are
// CONSTRUCTED below rather than accepted, because they are the composition
// decisions this file exists to make visible — a recorder passed in as a
// parameter would let a caller pass nil again, and the test would be grading its
// own fixture.
type preTradeDeps struct {
	// Books is the hypothetical post-trade book source: holdings, cash and risk.
	Books comp.BookSource
	// Mandates resolves the mandate governing a portfolio.
	Mandates comp.MandateSource
	// Margin is the #408 venue-margin source. It is always wired, even where no
	// account is bound: it answers UNKNOWN there, which refuses.
	Margin comp.MarginSource
	// Marks is the reference-mark fold, read by the unpriced observer to tell a
	// mark that has EXPIRED from one never seen.
	Marks *mark.Source
}

// preTradeWiring is what the builder produces: the gate, plus the two things the
// composition root still needs afterwards for the reference-refresh loop.
type preTradeWiring struct {
	// Gate is the wired pre-trade gate.
	Gate *comp.PreTradeGate
	// Classifier is the reference-data cache backing the gate's classifier, or
	// nil when this deployment configures no security master. THE ONE SEAM THAT
	// MAY LEGALLY BE NIL — "no reference source on this deployment" and "the
	// source does not know this instrument" are different violations with
	// different operator actions, and collapsing them here would lose that.
	Classifier *refdata.Cache
	// CloseRecorder drains the decision recorder's queue on shutdown. NEVER nil:
	// a no-op on the log arm, so the caller defers one thing rather than choosing.
	CloseRecorder func()
	// RefreshFailures counts refresh cycles that reported a failed lookup. It is
	// returned rather than created at the loop, because it is REGISTERED
	// unconditionally: a deployment with no master must still publish the series
	// at zero, or "the refresh is failing" and "there is no refresh" read the
	// same.
	RefreshFailures prometheus.Counter
}

// buildPreTradeGate wires the COMP-01 pre-trade gate and everything that
// observes it.
//
// It returns an error only for a configuration the classifier refuses — a
// deployment that named a security master this pod cannot reach it. That is a
// startup failure by design: the alternative is a pod that admits orders while
// every sector and issuer limit silently passes, which is #640 exactly.
func buildPreTradeGate(cfg config.Config, deps preTradeDeps, producer compliancebus.Bus, reg prometheus.Registerer, logger *slog.Logger) (preTradeWiring, error) {
	// AN UNGOVERNED PORTFOLIO IS COUNTED AND ANNOUNCED (EXEC-M14).
	//
	// It used to be silent: an order for a portfolio nobody had put under mandate
	// returned Allowed:true with no log and no metric, so "we forgot to mandate
	// fund X" and "fund X passed compliance" were the same observable event. This
	// counter is what makes "how much of the book is ungoverned" a number
	// somebody can look at, rather than a question nobody has asked.
	// LABELLED BY WHICH UNGOVERNED STATE IT IS (#926), because the two mean
	// different things and an operator acts on them differently.
	//
	//   never_mandated  nobody has run `kanz-mandate` for this portfolio. The
	//                   onboarding state, and the one OMS_REQUIRE_MANDATE=false
	//                   was meant to trade through.
	//   mandate_lapsed  the portfolio HAS versions and none is in force. It WAS
	//                   governed and is not now — the #916 shape, where scheduling
	//                   a change evicted the version in force from the compacted
	//                   stream and the replica booted holding only a future-dated
	//                   one. Every health signal was green and this counter was
	//                   the only trace, reading exactly like onboarding.
	//
	// SEEDED FOR EVERY VERDICT at registration, from compliance.Governances(),
	// because a CounterVec exports nothing for a label it has never incremented —
	// so an alert on mandate_lapsed would query an empty vector and never fire,
	// which is #62's ten deleted rules exactly.
	ungovernedByState := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kanz_compliance_ungoverned_orders_total",
		Help: "Orders admitted or refused for a portfolio that NO MANDATE GOVERNS, by which " +
			"ungoverned state it is. never_mandated is the onboarding gap; mandate_lapsed means " +
			"the portfolio WAS governed and none of its mandate versions is in force, which is " +
			"not an onboarding state and is money (#926).",
	}, []string{"governance"})
	for _, g := range comp.Governances() {
		if g.NoMandate() {
			ungovernedByState.WithLabelValues(g.String()).Add(0)
		}
	}
	// A MANDATE THAT WAS PUBLISHED AND COULD NOT BE READ (#619). Distinct from
	// ungoverned above, and the distinction is the whole point: ungoverned means
	// nobody wrote a mandate, which an operator may knowingly trade through. This
	// means somebody wrote one and this process cannot apply it — and because the
	// mandate stream is compacted, no redelivery and no restart will fix it. Only
	// a republish will. The two counters must never be summed into one.
	mandateUnreadable := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kanz_compliance_unreadable_mandate_orders_total",
		Help: "Orders REFUSED because the portfolio has a published mandate this process could not " +
			"apply. Non-zero means a portfolio is un-governable until its mandate is republished; " +
			"unlike an ungoverned portfolio this does not depend on OMS_REQUIRE_MANDATE.",
	})
	// AN UNPRICED REFUSAL IS COUNTED TOO (COMP-M2).
	//
	// Ungoverned already had a counter; Unpriced and Unvaluable — the other
	// "nothing was evaluated" refusals — had only a once-per-(portfolio,
	// instrument) warn log. That log fires once for the LIFE OF THE PROCESS, so a
	// sustained pricing outage goes invisible after the first refused order. The
	// two causes are split by label because they are different incidents with
	// different responses: "never_seen" is a cold pod, a thin instrument, or a
	// subscription delivering nothing (a warm-up); "expired" is a feed that WAS
	// reporting and has stalled (an outage).
	unpriced := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kanz_compliance_unpriced_orders_total",
		Help: "Orders refused PRICE_UNAVAILABLE because no usable reference mark exists for the instrument. " +
			"Sustained non-zero means the market-data spine is not reaching this OMS for that instrument.",
	}, []string{"reason"})
	// AN ADMISSION AGAINST A BALANCE NOBODY VOUCHED FOR IS COUNTED (#671).
	//
	// Ungoverned and Unpriced above make a REFUSAL or an unconstrained pass
	// visible. This is the third shape and the one that was still silent: every
	// rule ran, every rule passed, and the buying-power rule spent against a cash
	// figure missing entry types this deployment does not produce (#588). A
	// refusal already names them (attributeCash puts balance_omits on the
	// violation); an ADMISSION named nothing.
	//
	// LABELLED BY POSTURE because the operator actions differ: "incomplete" means
	// a feed that does not exist yet and the omission is known; "unstated" means
	// the producing service is not declaring completeness at all, which is a
	// wiring fault in the announcer and is fixable today.
	//
	// Expect this to be NON-ZERO on every current deployment — corporate_action
	// is unproduced estate-wide — which is the point: the number says how much of
	// the day's flow cleared against a balance nobody stands behind, instead of
	// that being a question nobody has asked.
	unaccounted := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kanz_compliance_unaccounted_admissions_total",
		Help: "Orders ADMITTED against a cash balance whose producer did not vouch for it. " +
			"incomplete = the producer named missing entry types; unstated = the producer said nothing. " +
			"On a SHORT book an unfolded entitlement OVERSTATES cash, so buying power can admit an " +
			"order the fund cannot pay for.",
	}, []string{"posture"})
	refreshFailures := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kanz_instrument_reference_refresh_failures_total",
		Help: "Reference-data refresh cycles that reported at least one failed lookup. Registered " +
			"before anything can fail so \"none\" is a zero series rather than a missing one.",
	})
	reg.MustRegister(ungovernedByState, mandateUnreadable, unpriced, unaccounted, refreshFailures)

	// THE INSTRUMENT CLASSIFIER (#640), CACHED AND NEVER DIALLED FROM THE GATE.
	// The rules run inside the order-admission path, so a classifier that dialled
	// datamaster from inside them would put an uncancellable HTTP call on that
	// path and couple admission to another service's availability. The cache is
	// filled by the refresh cycle the composition root starts instead.
	refCache, err := cfg.RefData.NewCache(cfg.Tenant, "svc:oms")
	if err != nil {
		logger.Error("oms: the instrument classifier refused its configuration", "err", err)
		return preTradeWiring{}, err
	}
	cfg.RefData.LogPosture(logger, "oms")
	var classifier comp.Classifier
	if refCache != nil {
		classifier = refCache.Compliance()
	}
	// ZERO IS A READABLE ANSWER, so the gauges are registered whether or not a
	// master is wired: an alert asking "is any classifier armed" must find a
	// series to read, not a missing one (#622).
	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "kanz_instrument_classifier_wired",
		Help: "1 when this pod has a reference-data source for instrument classification. ZERO " +
			"MEANS EVERY SECTOR, ISSUER AND ASSET_CLASS MANDATE RULE IS REFUSED as unverifiable.",
	}, func() float64 {
		if refCache == nil {
			return 0
		}
		return 1
	}))
	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "kanz_instrument_classifier_resolved",
		Help: "Instruments this pod can currently classify. Held against " +
			"kanz_instrument_classifier_wanted it is the difference between a warming cache and " +
			"a reference-data gap.",
	}, func() float64 {
		if refCache == nil {
			return 0
		}
		return float64(refCache.Stats().Resolved)
	}))
	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "kanz_instrument_classifier_wanted",
		Help: "Instruments asked about and not yet resolved. Each one is a holding whose " +
			"classified-dimension mandate rules are being REFUSED right now; persistently " +
			"non-zero means the refresh is failing or the master does not hold the book.",
	}, func() float64 {
		if refCache == nil {
			return 0
		}
		return float64(refCache.Stats().Wanted)
	}))

	// THE DECISION RECORDER, WHICH WAS THE SECOND BARE NIL (#643) AND IS NOW ON
	// THE AUDIT CHAIN (#713).
	//
	// ASYNCHRONOUS, and that is the whole reason this took two changes. The
	// compliance service's recorder publishes SYNCHRONOUSLY, which suits the
	// post-trade monitor and not this caller: order admission is deliberately
	// outbox-backed — every FACT is written in the same transaction as the order
	// and drained by a relay — so nothing on that path waits for a broker. A
	// synchronous publish in front of every order would put broker latency into
	// admission, a change to the TRADING path made for a REPORTING reason.
	//
	// WITHOUT A BUS IT FALLS BACK TO THE LOG RATHER THAN TO NOTHING. That is the
	// api-gateway's rule for the same seam — "neither arm is a no-op, which is the
	// point" — and it keeps a local run drivable without making an unrecorded gate
	// look like a recorded one. Which arm this build took is on the posture gauge
	// below and in the startup log, so the two are never guessed at.
	recorder, closeRecorder := buildDecisionRecorder(producer, reg, logger)

	gate := comp.NewPreTradeGate(
		comp.NewEngine(nil), deps.Books, deps.Mandates, classifier, recorder, logger,
		comp.WithRequireMandate(cfg.RequireMandate),
		// THE SOURCE IS ALWAYS WIRED, even on a deployment that binds no accounts
		// and observes no margin. It answers UNKNOWN there, which refuses — and
		// refusing is only reachable for a portfolio whose mandate DECLARES margin
		// trading, so nothing that trades today changes. Leaving the seam nil
		// instead would make "no margin control on this build" and "margin unknown"
		// the same observable state, which is the conflation this estate designs
		// against.
		comp.WithMarginSource(deps.Margin),
		comp.WithUngovernedObserver(func(_, _ string, g comp.Governance) {
			ungovernedByState.WithLabelValues(g.String()).Inc()
		}),
		comp.WithUnreadableObserver(func(string, string) { mandateUnreadable.Inc() }),
		comp.WithUnaccountedObserver(func(_, _, omits string) {
			// The posture, not the omitted list, is the label: entry-type names are a
			// bounded set today but they come from the producing deployment, and a
			// metric label fed by another service's vocabulary is unbounded
			// cardinality waiting to happen. The names are in the WARN, once per
			// portfolio, where they cost nothing.
			if omits == "" {
				unaccounted.WithLabelValues("unstated").Inc()
				return
			}
			unaccounted.WithLabelValues("incomplete").Inc()
		}),
		comp.WithUnpricedObserver(func(portfolioID, instrumentID string, firstForPair bool) {
			// Two very different incidents arrive at the same refusal, and an
			// operator needs to tell them apart: a mark we have NEVER seen means a
			// cold pod, a thin instrument, or a subscription delivering nothing;
			// a mark we HAVE seen but which expired means the feed was working and
			// stalled. One is a warm-up, the other is an outage.
			//
			// THE COUNTER IS UNCONDITIONAL AND THE LOG IS NOT (#883). Both halves
			// of that sentence are load-bearing:
			//
			//   - the counter is a RATE. How fast orders are being refused is the
			//     number an operator watches during a price-feed outage, and it is
			//     only a rate if every refusal increments it. Moving either Inc()
			//     below the firstForPair check would silently turn this series
			//     into a count of distinct instruments first seen unpriced.
			//   - the log is a CONDITION. An operator wants to be told the
			//     condition CHANGED, not that it is still true; the rate is
			//     already on the counter, and a per-order line only buries the
			//     transition it should be announcing. This warned on every order
			//     while internal/compliance's gate warned once per pair, so one
			//     event carried two dedup policies. firstForPair is the gate's own
			//     verdict, so both lines now appear together, once per
			//     (tenant, portfolio, instrument), and the diagnosis below —
			//     warm-up versus outage, the distinction #96's tombstone design
			//     exists to preserve — is what the gate's generic line cannot say.
			if _, asOf, seen := deps.Marks.Lookup(instrumentID); seen {
				unpriced.WithLabelValues("expired").Inc()
				if !firstForPair {
					return
				}
				logger.Warn("order refused: the reference mark is STALE — the price feed has stopped reporting for this instrument",
					"portfolio", portfolioID, "instrument", instrumentID,
					"mark_as_of", asOf, "max_age", cfg.PriceMaxAge)
				return
			}
			unpriced.WithLabelValues("never_seen").Inc()
			if !firstForPair {
				return
			}
			logger.Warn("order refused: NO reference mark has ever been seen for this instrument — a cold pod warming up, an instrument nothing quotes, or a price subscription delivering nothing",
				"portfolio", portfolioID, "instrument", instrumentID, "subjects", cfg.PriceSubjects)
		}),
	)

	announceGatePosture(gate, reg, logger)
	return preTradeWiring{
		Gate: gate, Classifier: refCache, RefreshFailures: refreshFailures,
		CloseRecorder: closeRecorder,
	}, nil
}

// buildDecisionRecorder returns the COMP-01e recorder for the pre-trade gate and
// the function that drains it on shutdown.
//
// WITH A PRODUCER it publishes every decision onto platform.compliance.decision,
// off the caller's goroutine, so AUDIT-01's append-only projection is the system
// of record. WITHOUT one it records to the log, which is weaker — not on the hash
// chain, not queryable beside the FACTs it justified — and is not nothing.
//
// A DROP IS COUNTED, and the counter exists on both arms so that "this build has
// no durable recorder" and "the durable recorder is keeping up" are not the same
// absence. It is seeded at zero for the reason every counter here is: a series
// that appears on its first increment reads as no-data to an alert, which cannot
// fire on the transition that matters.
func buildDecisionRecorder(producer compliancebus.Bus, reg prometheus.Registerer, logger *slog.Logger) (comp.DecisionRecorder, func()) {
	dropped := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kanz_compliance_decisions_dropped_total",
		Help: "Pre-trade compliance decisions that did NOT reach the audit trail, by this process. " +
			"Non-zero means the trail is missing decisions this gate did make WHILE THE ORDERS THEY " +
			"GATED WENT AHEAD — the queue overflowed or the broker refused. Zero on a build with no " +
			"bus does NOT mean the trail is complete: read it beside " +
			"kanz_compliance_pretrade_recorder_durable, which is 0 there.",
	})
	durable := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "kanz_compliance_pretrade_recorder_durable",
		Help: "1 when this pod's pre-trade decisions are published to the audit chain, 0 when they " +
			"reach the LOG ONLY. Zero is a real posture, not a defect — but a decision that exists " +
			"only in stdout is not on the AUDIT-01 hash chain and cannot be queried beside the FACTs " +
			"it justified.",
	})
	reg.MustRegister(dropped, durable)

	if producer == nil {
		durable.Set(0)
		logger.Warn("oms: pre-trade compliance decisions are recorded to the LOG ONLY — no bus is "+
			"configured, so they are not on the AUDIT-01 hash chain and do not survive log "+
			"retention (#713)",
			"fix", "set OMS_NATS_URL", "metric", "kanz_compliance_pretrade_recorder_durable")
		return comp.NewSlogRecorder(logger), func() {}
	}
	rec, err := compliancebus.NewAsyncRecorder(producer, logger,
		compliancebus.WithOverflowHandler(func(comp.DecisionRecord) { dropped.Inc() }),
		compliancebus.WithErrorHandler(func(error) { dropped.Inc() }),
	)
	if err != nil {
		// Unreachable: NewAsyncRecorder errors only on a nil bus, excluded above.
		// Degrade rather than refuse to start — an unrecorded gate is bad, an OMS
		// that will not start is a trading outage.
		durable.Set(0)
		logger.Error("oms: the durable decision recorder could not be built — falling back to the log",
			"err", err)
		return comp.NewSlogRecorder(logger), func() {}
	}
	durable.Set(1)
	logger.Info("oms: pre-trade compliance decisions publish to the audit chain",
		"subject", compliancebus.SubjectDecision,
		"counter", "kanz_compliance_decisions_dropped_total")
	return rec, rec.Close
}

// announceGatePosture publishes which of the gate's controls this build actually
// holds, as a log line at startup and a gauge per seam.
//
// THIS IS THE SIGNAL THAT DID NOT EXIST, and its absence is why #640 ran for
// months. Every rule the classifier feeds returned "pass" and no metric anywhere
// distinguished that from a book with no sector limits in it. A per-seam gauge
// makes "which controls does this gate have" a dashboard question rather than a
// source-reading exercise, and it reads 0 for a seam that is missing rather than
// going absent — a missing series reads as no-data to an alert, which cannot
// fire.
func announceGatePosture(gate *comp.PreTradeGate, reg prometheus.Registerer, logger *slog.Logger) {
	p := gate.Posture()
	seams := map[string]bool{
		"books":      p.Books,
		"mandates":   p.Mandates,
		"classifier": p.Classifier,
		"recorder":   p.Recorder,
		"margin":     p.Margin,
	}
	wired := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kanz_compliance_pretrade_seam_wired",
		Help: "1 when the pre-trade gate holds this seam, 0 when it does not. classifier=0 means " +
			"EVERY SECTOR AND ISSUER LIMIT PASSES unverified (#640); recorder=0 means no pre-trade " +
			"decision is recorded anywhere, so the platform cannot say an order was CHECKED; " +
			"margin=0 means a mandate declaring margin trading cannot be gated at all (#408). A " +
			"seam reads 0 rather than going absent, because a missing series cannot alert.",
	}, []string{"seam"})
	reg.MustRegister(wired)
	for name, held := range seams {
		if held {
			wired.WithLabelValues(name).Set(1)
			continue
		}
		wired.WithLabelValues(name).Set(0)
	}
	if p.Complete() {
		logger.Info("oms: the pre-trade gate holds every control it can",
			"metric", "kanz_compliance_pretrade_seam_wired")
		return
	}
	logger.Warn("oms: THE PRE-TRADE GATE IS MISSING A CONTROL — the rules that depend on the "+
		"missing seam do not refuse, they PASS, and a mandate written against them is not being "+
		"enforced (#643)",
		"books", p.Books, "mandates", p.Mandates, "classifier", p.Classifier,
		"recorder", p.Recorder, "margin", p.Margin,
		"metric", "kanz_compliance_pretrade_seam_wired")
}

// compile-time assertion that the OMS adapter still satisfies the seam this
// builder feeds it into. Without it a signature drift in the shared compliance
// package surfaces as a type error 600 lines into main.go rather than here.
var _ comp.BookSource = (*compliance.BookSource)(nil)
