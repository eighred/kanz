package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/proto"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	"github.com/eighred/kanz/internal/cashview"
	"github.com/eighred/kanz/internal/marketdata/mark"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/compliance/internal/config"
	"github.com/eighred/kanz/services/compliance/internal/monitor"
)

// WHAT THE POST-TRADE BOOK IS WORTH (#787).
//
// The monitor's own input is holdings and nothing else. Position FACTs carry no
// cash and no marks, so the book it builds is a positions total valued at
// whatever the OMS position projector recorded — average cost. Gross exposure is
// the sum of the same values, which made a max_gross_leverage cap score exactly
// 1.0 for any long-only book and bind on nothing (#780); since #786 that is a
// refusal naming the gap rather than a silent pass, which is honest and is still
// not enforcement.
//
// This is the other two thirds, folded exactly as the OMS folds them: the same
// internal/cashview for what accounting says a portfolio can spend, the same
// internal/marketdata/mark for what its holdings are worth now. The same
// packages, not similar ones — a second implementation of either would give the
// pre-trade and post-trade halves of one control different answers for reasons
// no operator could see.
//
// IT IS A NAMED BUILDER RETURNING WHAT IT BUILT, which is what
// composition_root_length_test.go asks for and what services/oms/cmd/oms/
// pretrade.go established. While these were locals in runConsumers no test could
// reach them: whether the mark fold got the configured staleness bound, and
// whether a cash announcement actually re-evaluates, were both unassertable.
type postTradeValuation struct {
	// Cash is what accounting says each portfolio can spend. NEVER nil: an
	// unwired cash source and a portfolio nobody has announced are different
	// states, and only the second is a legitimate UNKNOWN.
	Cash *cashview.View
	// Marks is the reference-price fold. NEVER nil either, and empty is not the
	// same as absent: an empty fold answers nil for every instrument, which is the
	// same refusal a stalled feed produces and the correct one.
	Marks *mark.Source
}

// buildPostTradeValuation wires both folds and the gauge that says how much of
// the book can currently be valued.
func buildPostTradeValuation(cfg config.Config, reg prometheus.Registerer, logger *slog.Logger) postTradeValuation {
	// A NIL LOGGER DEFAULTS RATHER THAN PANICS, the same way monitor.NewMonitor
	// treats one. A builder that dereferences it is a composition root that dies
	// on a caller's omission instead of degrading — and the posture warning below
	// is the one thing here that MUST reach an operator.
	if logger == nil {
		logger = slog.Default()
	}
	v := postTradeValuation{
		Cash:  cashview.New(),
		Marks: mark.New(time.Now, cfg.PriceMaxAge),
	}
	// REGISTERED UNCONDITIONALLY, so "no instrument can be valued" is a zero on a
	// dashboard rather than a missing series. A deployment with no price feed and
	// one whose feed died must not look the same.
	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "kanz_compliance_marked_instruments",
		Help: "Instruments whose reference mark is within COMPLIANCE_PRICE_MAX_AGE. A book " +
			"holding anything outside this set cannot be valued, so its gross-leverage rule " +
			"REFUSES rather than reporting a ratio.",
	}, func() float64 { _, live := v.Marks.Stats(); return float64(live) }))

	// THE DEGRADED POSTURE IS ANNOUNCED, not discovered from a refusal. An
	// operator reading "leverage cannot be verified" in a breach record must be
	// able to tell a stalled feed from a deployment that was never given one.
	if len(cfg.PriceSubjects) == 0 {
		logger.Warn("compliance has NO PRICE FEED: post-trade books cannot be valued, so every "+
			"gross-leverage rule will REFUSE rather than report a ratio. Concentration, "+
			"restriction and issuer rules are unaffected.",
			"fix", "set COMPLIANCE_PRICE_SUBJECTS (default: market.*.trade,market.*.quote)")
	} else {
		logger.Info("compliance folding the price spine for post-trade valuation",
			"subjects", cfg.PriceSubjects, "max_age", cfg.PriceMaxAge)
	}
	return v
}

// CashHandler folds a balance announcement AND re-evaluates the portfolio it
// names.
//
// FOLDING IT ALONE IS NOT ENOUGH, and this is the part a test has to be able to
// reach. Cash is half of equity, so drawing on a margin loan moves a portfolio's
// gross leverage with NO position FACT behind it. A handler that only updated
// the number would leave that breach waiting for an unrelated position change or
// for the next sweep — which is the same class of silence #787 is about, one
// layer in.
//
// A PAYLOAD THE FOLD REJECTED IS NOT RE-DECIDED HERE. cashview.Handle has
// already judged those bytes unusable and acked them; forming a second opinion
// would let this handler act on a balance the fold refused to store.
func (v postTradeValuation) CashHandler(mon *monitor.Monitor) bus.EventHandler {
	return func(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
		if err := v.Cash.Handle(ctx, env, payload); err != nil {
			return err
		}
		var bal accountingpb.PortfolioCashBalance
		if proto.Unmarshal(payload, &bal) != nil || bal.GetPortfolioId() == "" {
			return nil
		}
		return mon.Reevaluate(ctx, env.GetTenantId(), bal.GetPortfolioId())
	}
}

// reevaluateBooks re-runs every book the monitor holds, on an interval.
//
// IT IS WHAT MAKES A PASSIVE BREACH VISIBLE AT ALL (#787). Every other path into
// the monitor is woken by a FACT — a position change, a cash announcement — and
// the breach this service exists to catch is the one with no FACT behind it: a
// financed book that falls 20% raises its gross leverage with no order placed
// anywhere. The package doc calls that "a passive, market-move-induced breach"
// and names it as the reason the monitor exists; nothing ever delivered one.
//
// IT LOGS AND CONTINUES RATHER THAN TERMINATING, for the reason the reference
// refresh cycle beside it does: a monitor that stops is a fund nothing is
// watching at all, which is strictly worse than one whose last sweep failed. A
// transient mandate lookup is the expected failure and it resolves itself.
//
// THE FIRST SWEEP RUNS AFTER ONE INTERVAL, NOT IMMEDIATELY. At boot the position
// replay is still draining and the mark fold is empty, so an immediate pass
// would evaluate half-built books — and because it goes through the same
// transition test as every other path, it could record a BREACH that the next
// sweep silently un-breaches. One interval of nothing is the honest start.
func reevaluateBooks(ctx context.Context, mon *monitor.Monitor, interval time.Duration, logger *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := mon.ReevaluateAll(ctx); err != nil && ctx.Err() == nil {
				logger.Error("compliance: post-trade re-evaluation sweep did not complete; a passive "+
					"breach may be unreported until the next one", "err", err, "interval", interval)
			}
		}
	}
}
