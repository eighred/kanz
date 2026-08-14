// Package costwatch measures what each fill cost and exports it by venue (#436).
//
// It is the consumer half of the transaction-cost work: internal/execution/tca
// does the arithmetic, this subscribes the fill FACTs and records the result.
//
// # Per FILL, not per order, and that is what makes it stateless
//
// The obvious design is one cost record per terminal order. It does not work
// here, and the reason is worth writing down because it is not obvious from
// outside: the individual Fill — and therefore its FEE — lives only in the FACT.
// The stored OrderState keeps a cumulative quantity and an average price, and no
// fee total at all. So a per-order measure would have to accumulate fees across
// deliveries, and an in-memory accumulator is lost on restart and wrong across
// replicas — on the one path where being quietly wrong is indistinguishable from
// being right.
//
// Every fill FACT, however, carries BOTH the fill (with its fee and its venue)
// AND the resulting OrderState (with its arrival mark). So each fill is
// independently measurable against the decision-time benchmark, with no state
// kept anywhere. "What did this order cost" is a notional-weighted sum of its
// fill records; "which venue should we have used" is an aggregate by venue —
// which is the question #436 actually asks, and the one a per-order record
// answers less well, because an order can fill across several venues.
//
// # It measures; it decides nothing
//
// No order is routed, sized or refused here. #437 (venue ranking) and #435
// (execution algorithms) are the consumers of this signal, and both are better
// built on a measurement than on an assertion.
package costwatch

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/execution/tca"
	"github.com/eighred/kanz/pkg/bus"
)

// Event types this folds. Both carry the same three fields; the only difference
// is whether the order is now terminal, which does not change the arithmetic.
const (
	EventTypeFilled          = "order.order.filled"
	EventTypePartiallyFilled = "order.order.partially_filled"

	// EventTypeCostRecorded is the durable half of this measurement (#436).
	//
	// Inside the `order` domain deliberately: it is derived from the order's own
	// fills and its admission mark, so the EXECUTION stream already carries it
	// (order.>), the archiver's default subject list already archives it, and the
	// Kafka topic falls out as <tenant>.order.cost with no new mapping.
	EventTypeCostRecorded = "order.cost.recorded"

	// schemaVersion is the EVT-16 version this FACT carries.
	schemaVersion = 1
)

// Bus is the publish seam. An interface rather than *bus.Producer so a test can
// assert what was published without a broker.
type Bus interface {
	Publish(ctx context.Context, e bus.Event) error
}

// Watch folds fill FACTs into realized-cost measurements.
type Watch struct {
	tenant string
	bus    Bus
	// costs is the per-venue fold the ROUTER ranks on (#437 B). Fed in-process
	// rather than over the bus: publishing a FACT and subscribing to it in the
	// same service would put a broker round trip inside a feedback loop and make
	// the OMS a consumer of its own record. Nil ⇒ nothing ranks, and the declared
	// default stands.
	costs  *execution.VenueCosts
	now    func() time.Time
	logger *slog.Logger

	shortfallBps      *prometheus.HistogramVec
	priceShortfallBps *prometheus.HistogramVec
	unmeasurable      *prometheus.CounterVec
}

// New registers the cost metrics and returns the handler.
//
// LABELLED BY VENUE, because that is the question. A single aggregate bps figure
// says trading cost something; a per-venue distribution says WHICH venue, which
// is the difference between a number and a decision.
//
// A HISTOGRAM RATHER THAN A GAUGE: cost is a distribution, and the mean of a
// distribution with a long tail is the statistic that hides the tail. The buckets
// straddle zero because beating the benchmark is a real and common outcome — a
// bucket set starting at zero would report every saving as the smallest possible
// cost.
func New(reg prometheus.Registerer, tenant string, b Bus, costs *execution.VenueCosts, logger *slog.Logger) *Watch {
	buckets := []float64{-100, -50, -25, -10, -5, 0, 5, 10, 25, 50, 100, 250, 500}
	w := &Watch{
		tenant: tenant,
		bus:    b,
		costs:  costs,
		now:    time.Now,
		logger: logger,
		shortfallBps: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "kanz_execution_shortfall_bps",
			Help: "Realized implementation shortfall per fill, in basis points of arrival " +
				"notional, INCLUDING fees. Positive cost money against the decision-time mark; " +
				"negative beat it.",
			Buckets: buckets,
		}, []string{"venue"}),
		priceShortfallBps: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "kanz_execution_price_shortfall_bps",
			Help: "The same measure EXCLUDING fees, so a venue's execution quality and its fee " +
				"schedule can be told apart — a venue can be good at one and bad at the other.",
			Buckets: buckets,
		}, []string{"venue"}),
		unmeasurable: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kanz_execution_unmeasurable_fills_total",
			Help: "Fills whose cost could not be measured, by reason. NOT zero-cost fills: " +
				"scoring them as zero would drag every venue average toward whichever venue " +
				"trades the instruments the price spine covers worst.",
		}, []string{"venue", "reason"}),
	}
	if w.logger == nil {
		w.logger = slog.Default()
	}
	reg.MustRegister(w.shortfallBps, w.priceShortfallBps, w.unmeasurable)
	return w
}

// Handle measures one fill FACT. It is a bus.EventHandler.
//
// EVERY RETURN IS NIL — every delivery is ACKED. This observes; it does not own
// the fill. A nack would redeliver bytes that will fail the same way forever
// while occupying the consumer real fills arrive on, and the fill FACT is durable
// regardless of whether this handler could read it. Losing a cost measurement is
// a gap in a report; losing the fill would be a gap in the books, and that is
// somebody else's consumer.
func (w *Watch) Handle(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
	// A FILL BELONGS TO ONE TENANT'S BOOK (#223). Inert while this OMS serves
	// __system__, and correct the day per-tenant compute lands (#97).
	if err := bus.RequireTenantScope(env.GetTenantId(), w.tenant); err != nil {
		return err
	}

	fill, state, ok := decodeFill(env.GetEventType(), payload)
	if !ok {
		w.unmeasurable.WithLabelValues("", "undecodable").Inc()
		return nil
	}

	res, err := tca.Measure(state, []*orderpb.Fill{fill})
	if err != nil {
		// NOT A ZERO. An unmeasurable fill is counted by REASON so the gap is
		// visible and attributable — "no arrival mark" is a price-spine coverage
		// problem, "mixed fee currency" is a venue-adapter one, and a single
		// bucket would make both look like the same quiet system.
		w.unmeasurable.WithLabelValues(fill.GetVenue(), reasonOf(err)).Inc()
		return nil
	}

	venue := res.Venue
	total, _ := res.ShortfallBps.Float64()
	// FEED THE RANKER (#437 B). The same measurement, in-process, so an
	// untargeted order can go to the venue that has actually been cheapest
	// instead of the one somebody named once. It is a PREFERENCE: the ranker
	// abstains below its evidence floor, and the declared default stands.
	if w.costs != nil {
		w.costs.Observe(venue, total)
	}
	price, _ := res.PriceShortfallBps.Float64()
	w.shortfallBps.WithLabelValues(venue).Observe(total)
	w.priceShortfallBps.WithLabelValues(venue).Observe(price)

	// AND THE DURABLE HALF (#436). The metrics answer "which venue" for a human
	// reading a dashboard; they cannot answer it for a MACHINE. Prometheus is not
	// queryable from the routing path, it is aggregated and lossy by
	// construction, and its retention is weeks — while a venue ranking (#437)
	// needs cost per venue over a long window, joined to the orders it came from.
	//
	// A FAILED PUBLISH DOES NOT FAIL THE DELIVERY. This handler observes; the
	// fill is already durable and already folded by the projector and the books.
	// Nacking would redeliver a fill nobody needs redelivered, to re-measure
	// something that changes nothing, on the consumer real fills arrive on. The
	// loss is one row of a cost report, and it is counted so the gap is visible
	// rather than assumed.
	if err := w.publish(ctx, fill, state, res); err != nil {
		w.unmeasurable.WithLabelValues(venue, "publish_failed").Inc()
		w.logger.Error("costwatch: cost measured but not recorded — the metric moved and the FACT "+
			"did not, so a venue comparison over a long window will be short by this fill",
			"order_id", res.OrderID, "fill_id", fill.GetFillId(), "venue", venue, "err", err)
	}

	// FLOAT64 HERE AND ONLY HERE, DELIBERATELY. The measurement is exact
	// (*big.Rat) all the way through tca; this converts at the very last step,
	// to hand a number to Prometheus, which has no other numeric type. The rule
	// this platform keeps is that money and prices never pass through a float —
	// a basis-point statistic being bucketed for a dashboard is neither.
	w.logger.Debug("execution cost measured",
		"order_id", res.OrderID, "venue", venue, "instrument", res.Instrument,
		"filled", res.FilledQuantity.FloatString(8),
		"avg_price", res.AveragePrice.FloatString(8),
		"arrival_price", res.ArrivalPrice.FloatString(8),
		"fees", res.Fees.FloatString(8), "fee_currency", res.FeeCurrency,
		"shortfall_bps", res.ShortfallBps.FloatString(4),
		"price_shortfall_bps", res.PriceShortfallBps.FloatString(4),
	)
	return nil
}

// decodeFill reads whichever fill FACT this is. Both messages carry the same
// three fields, and the arithmetic does not care which one arrived: a partial
// fill costs what it costs whether or not more follow.
//
// OUT-OF-DOMAIN EXPONENTS ARE REFUSED BEFORE ANY NUMBER IS READ (#95).
// Decimal.exponent is an unvalidated wire field and dec.FromProto materialises
// 10^abs(exponent), so a FACT carrying {1, 2000000000} does not produce a wrong
// cost — it never returns. The handler stops acking, the subscription stalls
// behind that one message, and the service still reports healthy. InDomainDeep
// walks EVERY Decimal in the message, so a field added to the schema later is
// covered without anyone coming back here.
func decodeFill(eventType string, payload []byte) (*orderpb.Fill, *orderpb.OrderState, bool) {
	switch eventType {
	case EventTypeFilled:
		var m orderpb.OrderFilled
		if proto.Unmarshal(payload, &m) != nil || m.GetFill() == nil {
			return nil, nil, false
		}
		if _, in := dec.InDomainDeep(&m); !in {
			return nil, nil, false
		}
		return m.GetFill(), m.GetState(), true
	case EventTypePartiallyFilled:
		var m orderpb.OrderPartiallyFilled
		if proto.Unmarshal(payload, &m) != nil || m.GetFill() == nil {
			return nil, nil, false
		}
		if _, in := dec.InDomainDeep(&m); !in {
			return nil, nil, false
		}
		return m.GetFill(), m.GetState(), true
	}
	return nil, nil, false
}

// reasonOf maps a measurement refusal to a stable label. Bounded by construction
// — an unbounded label from an error string would be a cardinality leak in a
// metric that sees every fill.
func reasonOf(err error) string {
	switch {
	case errors.Is(err, tca.ErrNoArrivalMark):
		return "no_arrival_mark"
	case errors.Is(err, tca.ErrNoFills):
		return "no_fill_quantity"
	case errors.Is(err, tca.ErrMixedFeeCurrency):
		return "mixed_fee_currency"
	default:
		return "other"
	}
}

// publish records the measurement as a FACT on the order domain.
//
// EVERY FIGURE IS EXACT OR THE RECORD IS NOT WRITTEN. dec.ToProtoScaled
// preserves magnitude by rescaling, and a value it cannot represent at any
// exponent is not something to write down approximately — a cost report is read
// as authoritative, and a rounded basis-point figure is how a venue comparison
// gets decided by the fourth decimal.
func (w *Watch) publish(ctx context.Context, fill *orderpb.Fill, st *orderpb.OrderState, r tca.Result) error {
	if w.bus == nil {
		return nil // no bus wired: the metrics still work, which is the dev posture
	}
	qty, ok1 := tca.ToDecimal(r.FilledQuantity)
	px, ok2 := tca.ToDecimal(r.AveragePrice)
	arr, ok3 := tca.ToDecimal(r.ArrivalPrice)
	sf, ok4 := tca.ToDecimal(r.ShortfallBps)
	psf, ok5 := tca.ToDecimal(r.PriceShortfallBps)
	fees, ok6 := tca.ToDecimal(r.Fees)
	if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 || !ok6 {
		return errUnrepresentable
	}

	payload := &orderpb.TransactionCostRecorded{
		OrderId:           r.OrderID,
		FillId:            fill.GetFillId(),
		InstrumentId:      r.Instrument,
		Venue:             r.Venue,
		Side:              fill.GetSide(),
		FilledQuantity:    qty,
		FillPrice:         px,
		ArrivalPrice:      arr,
		Fee:               &commonpb.Money{Amount: fees, CurrencyCode: r.FeeCurrency},
		ShortfallBps:      sf,
		PriceShortfallBps: psf,
		MeasuredAt:        timestamppb.New(w.now().UTC()),
		// THE VWAP WINDOW. Copied straight through rather than re-derived: both
		// are already exact upstream, and a consumer holding only measured_at
		// knows when the measurement ran and nothing about the market it should
		// be compared against. Unset stays unset — an order admitted with no
		// usable mark has no arrival_at, and the epoch is not a window.
		ArrivalAt:  st.GetArrivalAt(),
		ExecutedAt: fill.GetExecutedAt(),
	}
	return w.bus.Publish(ctx, bus.Event{
		Subject:       EventTypeCostRecorded,
		EventType:     EventTypeCostRecorded,
		EventClass:    envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion: schemaVersion,
		Domain:        "order",
		EventTime:     w.now().UTC(),
		// PARTITIONED BY ORDER, not by fill: every measurement for one order lands
		// on one partition in order, so a consumer summing an order's cost sees
		// its fills in the sequence they happened.
		PartitionKey:     r.OrderID,
		PayloadSchemaRef: "order.v1.TransactionCostRecorded:1",
		Payload:          payload,
	})
}

var errUnrepresentable = errors.New("costwatch: a cost figure is not representable as an exact Decimal")
