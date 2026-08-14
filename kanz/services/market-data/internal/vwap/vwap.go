// Package vwap answers the second TCA question: did we trade worse than the
// market did (#436).
//
// # Two measures, and they disagree in the case that matters
//
// Implementation shortfall — already computed, per fill, by the OMS — says what
// the DECISION cost: the gap between the mark when somebody decided to trade and
// the price actually paid. Slippage against interval VWAP says something else:
// whether this execution was worse than the average participant's over the same
// window.
//
// An order can BEAT arrival because the market moved in its favour while still
// being worked worse than everyone else. internal/execution/tca's own test pins
// exactly that case — shortfall reports a saving, VWAP slippage reports a loss —
// and only the second measure sees it.
//
// # Why this lives in market-data
//
// It is a join between a cost record and a bar series, and market-data OWNS the
// bar series. The alternatives were worse: the OMS is the capital path and would
// have needed a market-data Postgres pool to compute an analytic nothing on the
// trading path reads, and the risk-engine holds bars only incidentally, for VaR.
// Reading your own data is the cheapest correct place for a join.
//
// It SUBSCRIBES and never publishes back. The cost record is the OMS's; this
// adds a second reading of it and stays out of the loop that produced it.
package vwap

import (
	"context"
	"log/slog"
	"math/big"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/proto"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/execution/tca"
	"github.com/eighred/kanz/internal/marketdata/store"
	"github.com/eighred/kanz/pkg/bus"
)

// EventTypeCostRecorded is the OMS's per-fill cost record (#475).
const EventTypeCostRecorded = "order.cost.recorded"

// Bars is the read this join needs. An interface rather than *store.Postgres so
// a test can answer without a database.
type Bars interface {
	Bars(ctx context.Context, q store.BarQuery) ([]store.Bar, error)
}

// Watch folds cost records into VWAP slippage.
type Watch struct {
	tenant string
	bars   Bars
	now    func() time.Time
	logger *slog.Logger

	slippage   *prometheus.HistogramVec
	unjoinable *prometheus.CounterVec
}

// Option customizes a Watch.
type Option func(*Watch)

// WithClock injects the clock (tests).
func WithClock(now func() time.Time) Option {
	return func(w *Watch) {
		if now != nil {
			w.now = now
		}
	}
}

// New registers the metrics and returns the handler.
func New(reg prometheus.Registerer, tenant string, bars Bars, logger *slog.Logger, opts ...Option) *Watch {
	w := &Watch{
		tenant: tenant,
		bars:   bars,
		now:    time.Now,
		logger: logger,
		slippage: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "kanz_execution_vwap_slippage_bps",
			Help: "Realized fill price against the interval VWAP over the order's working window, " +
				"in basis points. Positive traded worse than the market's own average; negative " +
				"beat it. Distinct from shortfall, which measures against the DECISION-time mark.",
			Buckets: []float64{-100, -50, -25, -10, -5, 0, 5, 10, 25, 50, 100, 250, 500},
		}, []string{"venue"}),
		unjoinable: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kanz_execution_vwap_unjoinable_total",
			Help: "Cost records that could not be compared to a VWAP, by reason. NOT zero " +
				"slippage: an execution nobody could benchmark must not read as one that matched " +
				"the market exactly.",
		}, []string{"venue", "reason"}),
	}
	for _, opt := range opts {
		opt(w)
	}
	if w.logger == nil {
		w.logger = slog.Default()
	}
	reg.MustRegister(w.slippage, w.unjoinable)
	return w
}

// Handle joins one cost record to the bars covering its window. It is a
// bus.EventHandler.
//
// EVERY RETURN IS NIL — every delivery is ACKED. This is a second reading of a
// record somebody else owns and already made durable. A nack would redeliver it
// to recompute an analytic nothing acts on, on the consumer real cost records
// arrive on; and a bar series that is missing now will still be missing on the
// redelivery, because bars are backfilled on their own schedule and not in
// response to this.
func (w *Watch) Handle(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
	if err := bus.RequireTenantScope(env.GetTenantId(), w.tenant); err != nil {
		return err
	}

	var rec orderpb.TransactionCostRecorded
	if proto.Unmarshal(payload, &rec) != nil {
		w.unjoinable.WithLabelValues("", "undecodable").Inc()
		return nil
	}
	// OUT-OF-DOMAIN EXPONENTS ARE REFUSED BEFORE ANY NUMBER IS READ (#95):
	// dec.FromProto materialises 10^abs(exponent), so a corrupt record would hang
	// this consumer rather than produce a wrong figure.
	if _, in := dec.InDomainDeep(&rec); !in {
		w.unjoinable.WithLabelValues(rec.GetVenue(), "undecodable").Inc()
		return nil
	}
	venue := rec.GetVenue()

	// THE WINDOW OR NOTHING. arrival_at and executed_at bound the interval this
	// fill was worked over; without both there is no market to compare against,
	// and an unset timestamp decodes as the epoch — a window from 1970 would
	// select no bars and, if it selected any, would compare against the wrong
	// century.
	if rec.GetArrivalAt() == nil || rec.GetExecutedAt() == nil {
		w.unjoinable.WithLabelValues(venue, "no_window").Inc()
		return nil
	}
	from := rec.GetArrivalAt().AsTime().UTC()
	to := rec.GetExecutedAt().AsTime().UTC()
	if !to.After(from) {
		// A fill that executed at or before its own benchmark observation. Real,
		// and not a window: an immediately-marketable order can fill inside the
		// same bar the mark came from.
		w.unjoinable.WithLabelValues(venue, "empty_window").Inc()
		return nil
	}

	bars, err := w.bars.Bars(ctx, store.BarQuery{
		InstrumentID: rec.GetInstrumentId(),
		Venue:        venue,
		Resolution:   store.Resolution1m,
		From:         from,
		To:           to,
		// THE KNOWLEDGE HORIZON IS STATED, NOT DEFAULTED. A zero AsOf means "now"
		// (bar.go), and #428 records why relying on that default is dangerous —
		// the store's safe default is a backtest's unsafe one. Here the LATEST
		// knowledge is genuinely what is wanted: a restated bar is a correction to
		// what the market did, and a post-trade measure should use the correction.
		// Saying so explicitly is what stops the next reader assuming otherwise.
		AsOf: w.now().UTC(),
	})
	if err != nil {
		w.unjoinable.WithLabelValues(venue, "bar_read_failed").Inc()
		w.logger.Error("vwap: could not read the bar window for a cost record",
			"order_id", rec.GetOrderId(), "instrument", rec.GetInstrumentId(),
			"venue", venue, "err", err)
		return nil
	}

	prices := make([]*big.Rat, 0, len(bars))
	volumes := make([]*big.Rat, 0, len(bars))
	for _, b := range bars {
		prices = append(prices, dec.FromProto(b.Close))
		volumes = append(volumes, dec.FromProto(b.Volume))
	}
	ref, ok := tca.IntervalVWAP(prices, volumes)
	if !ok {
		// No bars, or no volume in any of them. Both mean the same thing for this
		// measure: nobody traded in that window, so there is no average
		// participant to compare against.
		w.unjoinable.WithLabelValues(venue, "no_volume").Inc()
		return nil
	}

	slip, ok := tca.SlippageVsVWAPBps(
		tca.Result{AveragePrice: dec.FromProto(rec.GetFillPrice())}, ref, rec.GetSide())
	if !ok {
		w.unjoinable.WithLabelValues(venue, "no_benchmark").Inc()
		return nil
	}
	f, _ := slip.Float64()
	w.slippage.WithLabelValues(venue).Observe(f)

	w.logger.Debug("vwap slippage measured",
		"order_id", rec.GetOrderId(), "fill_id", rec.GetFillId(), "venue", venue,
		"instrument", rec.GetInstrumentId(),
		"window_from", from, "window_to", to, "bars", len(bars),
		"vwap", ref.FloatString(8), "slippage_bps", slip.FloatString(4))
	return nil
}
