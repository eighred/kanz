package order

import (
	"context"
	"errors"
	"math/big"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/execution/tca"
	"github.com/eighred/kanz/internal/outbox"
)

// EXECUTION-QUALITY MEASUREMENT, ONCE PER DECISION (#866).
//
// # What this is for
//
// Until this existed the platform could not say whether working an order on a
// schedule was beating or losing to arrival on any parent it had ever traded.
// order.cost.recorded measures each FILL against the arrival mark, which is the
// right grain for ranking venues and the wrong grain for judging an algorithm: a
// parent worked in fifty slices produces fifty records, each true, and nothing
// anywhere adds them up. So every algorithm #864 proposes would have been a
// change nobody could prove was an improvement.
//
// # It measures; it decides nothing, and that is what sets its error policy
//
// A FAILURE HERE NEVER FAILS THE TRANSITION. Every other *Fact builder in this
// service is allowed to abort the state change it announces — a fill whose FACT
// cannot be captured must not be committed, because the books would then be
// short an execution nobody could reconstruct. This is the opposite: the state
// change is a real fill, a real cancellation, a real expiry, and refusing to
// commit it because a MEASUREMENT could not be built would put an analytic on
// the capital path. The measurement is lost, the loss is counted by reason, and
// the order proceeds.
//
// That asymmetry is the whole of the error handling below, and it is why every
// path returns nil records rather than an error.
//
// # Once, because terminal is once
//
// There is no dedup key and none is needed. An order reaches a terminal status
// exactly once — Route, ApplyFill, Cancel and Expire all refuse a terminal
// order, and Store.Save's compare-and-swap arbitrates the race between replicas
// — so the transaction that writes the terminal state is the transaction that
// carries this record, and a redelivery finds the order already terminal and
// takes the compensator path, which does not come here.

// attributionOutcome is the bounded label set for the counter below. Bounded by
// construction rather than by an error string, because this counter sees every
// terminal order and an unbounded label is a cardinality leak.
const (
	outcomeDecomposed = "decomposed"
	outcomeTotalOnly  = "total_only"
	// The order was terminal and measurable in principle but produced no record.
	outcomeNoArrival      = "no_arrival_mark"
	outcomeNoFills        = "no_fills"
	outcomeStoreError     = "children_unreadable"
	outcomeUnrepresetable = "unrepresentable"
	outcomeNotCaptured    = "not_captured"
	outcomeOther          = "other"
)

// AttributionOutcomes is every label value countAttribution can emit, in the
// order the constants above declare them.
//
// IT EXISTS SO THE COUNTER CAN BE SEEDED AT ZERO, and seeding is what makes an
// alert over this counter able to fire at all (#875).
//
// A Prometheus CounterVec exports NO series for a label value it has never been
// incremented with. So on the estate this measurement was built for — where the
// quote spine covers nothing and every decomposition comes back total_only —
// `kanz_oms_execution_attributions_total{outcome="decomposed"}` does not exist,
// and any rule of the form "total_only is rising AND decomposed is not" compares
// against an EMPTY VECTOR and yields nothing. The alert would be silent in
// precisely the state it was written to detect: the failure alerts/README.md
// records ten deleted rules for, arrived at from the other direction.
//
// Seeding is also the honest reading of the counter for a human. An OMS that has
// decomposed nothing and an OMS that has been running for four minutes both show
// no `decomposed` series unless it is seeded; with it, one shows 0 and the other
// shows nothing at all, and those are different claims.
//
// THE LIST IS EXPORTED FROM HERE RATHER THAN RETYPED AT THE COMPOSITION ROOT.
// A second hand-written copy in cmd/oms is how a new outcome gets counted but
// never seeded — the label would be live in the code and absent from the metric
// until the first time it fired, which is the one moment nobody wants to
// discover a gap. attribution_outcomes_test.go reads the const block above and
// fails if this slice omits any of it.
//
// Callers must copy before mutating — it is a package-level slice.
var AttributionOutcomes = []string{
	outcomeDecomposed,
	outcomeTotalOnly,
	outcomeNoArrival,
	outcomeNoFills,
	outcomeStoreError,
	outcomeUnrepresetable,
	outcomeNotCaptured,
	outcomeOther,
}

// WithAttributionCounter supplies the counter that makes this measurement's
// COVERAGE visible.
//
// IT IS THE DIFFERENCE BETWEEN "NOTHING CONFIGURED" AND "CHECKED, AND FINE".
// Without it, an OMS whose price spine covers none of its instruments and an OMS
// measuring every decision perfectly both publish some attribution FACTs and
// look identical from outside. The counter is labelled by OUTCOME — including
// the successful ones — so the ratio of decomposed to total_only to
// no_arrival_mark is readable at a glance, and a widening gap is an alertable
// price-spine problem rather than a quiet degradation of a report nobody checks.
//
// Nil ⇒ nothing is counted, which is the dev posture and not the deployed one.
func WithAttributionCounter(c *prometheus.CounterVec) ServiceOption {
	return func(s *Service) { s.attributions = c }
}

func (s *Service) countAttribution(outcome string) {
	if s.attributions == nil {
		return
	}
	s.attributions.WithLabelValues(outcome).Inc()
}

// attributionRecords builds the execution-attribution FACT for an order that is
// about to be committed in a TERMINAL status, to be enqueued in that same
// transaction.
//
// It returns nil — never an error — whenever there is nothing truthful to
// publish. The four honest silences:
//
//   - THE ORDER IS A CHILD. A slice is not a decision; attributing to one would
//     report the platform making fifty decisions where a desk made one, each
//     measured against a benchmark it did not choose.
//   - THE ORDER NEVER TRADED. A rejected order, or a parent cancelled before its
//     first slice filled, has no execution to measure. A zero here would be a
//     datapoint claiming a perfect fill that never happened.
//   - THERE WAS NO ARRIVAL MARK. The order is permanently unmeasurable, which is
//     a price-spine coverage gap and is counted as one.
//   - A FIGURE IS NOT EXACTLY REPRESENTABLE. A cost report is read as
//     authoritative, and a rounded basis-point number is how a venue comparison
//     gets decided by the fourth decimal.
func (s *Service) attributionRecords(ctx context.Context, st *orderpb.OrderState, now time.Time) []outbox.Record {
	if st == nil || st.GetParentOrderId() != "" || !IsTerminal(st) {
		return nil
	}

	slices, ok := s.attributionSlices(ctx, st)
	if !ok {
		return nil
	}
	a, err := tca.Attribute(st.GetSide(), dec.FromProto(st.GetArrivalPrice()), slices)
	if err != nil {
		switch {
		case errors.Is(err, tca.ErrNoArrivalMark):
			// NOT AN ERROR CONDITION AND NOT LOGGED AT ERROR. An instrument the
			// price spine does not cover produces this on every order, and an
			// ERROR per terminal order would train an operator to ignore the log
			// that is also the only place a real fault appears.
			s.countAttribution(outcomeNoArrival)
		case errors.Is(err, tca.ErrNoFills):
			s.countAttribution(outcomeNoFills)
		default:
			s.countAttribution(outcomeOther)
			s.logger.Warn("oms: a terminal order's execution cost could not be attributed",
				"order_id", st.GetOrderId(), "err", err)
		}
		return nil
	}

	payload, ok := attributionPayload(st, a, now)
	if !ok {
		s.countAttribution(outcomeUnrepresetable)
		s.logger.Error("oms: an execution attribution was computed and cannot be written down "+
			"exactly, so it is dropped rather than rounded — a cost report is read as "+
			"authoritative and a rounded basis-point figure is how a venue comparison gets "+
			"decided by the fourth decimal",
			"order_id", st.GetOrderId())
		return nil
	}

	rec, err := s.emitter.AttributionFact(ctx, payload, now)
	if err != nil {
		// outbox.From refuses a record with no tenant on the context. For every
		// OTHER FACT in this service that refusal stops the transition; here it
		// must not, because the transition is a real execution outcome and this
		// is a measurement of it.
		s.countAttribution(outcomeNotCaptured)
		s.logger.Warn("oms: a terminal order's execution attribution could not be captured for "+
			"delivery, so this decision will be missing from the execution-quality report",
			"order_id", st.GetOrderId(), "err", err)
		return nil
	}
	if a.Quality == tca.QualityDecomposed {
		s.countAttribution(outcomeDecomposed)
	} else {
		s.countAttribution(outcomeTotalOnly)
	}
	return []outbox.Record{rec}
}

// attributionSlices assembles the orders that traded into this decision.
//
// A SCHEDULED PARENT IS MEASURED FROM ITS CHILDREN AND NOT FROM ITSELF, because
// the parent's own aggregate is empty: children fill, the parent rests, and
// nothing folds a child's execution back onto it (retireIfFinished derives the
// parent's unfilled quantity by subtracting the children's fills for exactly
// this reason). Reading st.filled_quantity for a parent would report every
// worked order as having traded nothing.
//
// ok is false only when the children could not be READ. That is a store fault,
// not an unmeasurable order — measuring the parent as if it had no children
// would publish a confident zero — so it is counted separately and nothing is
// published.
func (s *Service) attributionSlices(ctx context.Context, st *orderpb.OrderState) ([]tca.Slice, bool) {
	if st.GetExecutionSchedule() == nil {
		return []tca.Slice{sliceOf(st)}, true
	}
	children, err := s.store.ListByParent(ctx, st.GetOrderId())
	if err != nil {
		s.countAttribution(outcomeStoreError)
		s.logger.Warn("oms: could not read the children of a finished parent to attribute its "+
			"execution cost; the decision is missing from the execution-quality report",
			"order_id", st.GetOrderId(), "err", err)
		return nil, false
	}
	out := make([]tca.Slice, 0, len(children))
	for _, c := range children {
		out = append(out, sliceOf(c))
	}
	return out, true
}

// sliceOf reads one order's contribution to a decision.
//
// nilIfUnset carries "not observed" across the proto boundary as nil rather than
// as the zero dec.FromProto returns for an absent field.
//
// IT IS NOT WHAT ENFORCES THE RULE, AND SAYING SO HERE IS THE POINT. Replacing
// these calls with a bare dec.FromProto was mutation-tested on 2026-08-31 and
// EVERY TEST STILL PASSED: tca.Slice.measurable() requires a POSITIVE mark and
// bid and an ask strictly above the bid, so a zero arrives as unmeasurable by
// the same route a nil does. The enforcement is there, it is mutation-proven
// there, and a reader must not come away believing this line is the guard.
//
// It stays because the conversion is still the correct one at this boundary —
// an absent field and a zero are different facts, and threading that difference
// intact means a future change to measurable() cannot silently turn every
// unquoted instrument into a free crossing. It is defence in depth with a stated
// depth, not a second implementation of the check.
func sliceOf(st *orderpb.OrderState) tca.Slice {
	return tca.Slice{
		OrderID:        st.GetOrderId(),
		FilledQuantity: nilIfUnset(st.GetFilledQuantity()),
		AveragePrice:   nilIfUnset(st.GetAverageFillPrice()),
		ReleaseMark:    nilIfUnset(st.GetReleasePrice()),
		ReleaseBid:     nilIfUnset(st.GetReleaseBid()),
		ReleaseAsk:     nilIfUnset(st.GetReleaseAsk()),
	}
}

// nilIfUnset converts an absent Decimal to an absent rational, keeping "not
// observed" distinguishable from "observed as zero" all the way to the
// measurement.
func nilIfUnset(d *commonpb.Decimal) *big.Rat {
	if d == nil {
		return nil
	}
	return dec.FromProto(d)
}

// attributionPayload renders the measurement as the FACT.
//
// EVERY FIGURE IS EXACT OR THE RECORD IS NOT WRITTEN, the same stance
// costwatch.publish takes: dec.ToProtoScaled preserves magnitude by rescaling,
// and a value it cannot represent at any exponent is not something to write down
// approximately.
//
// THE THREE LEGS ARE WRITTEN ONLY WHEN THE MEASUREMENT DECOMPOSED. On a
// TOTAL_ONLY result they are nil in the Attribution and stay unset on the wire —
// unset, not zero, because a consumer handed three zeros cannot tell a market
// that cost nothing to cross from one nobody could see.
func attributionPayload(st *orderpb.OrderState, a tca.Attribution, now time.Time) (*orderpb.ExecutionAttributionRecorded, bool) {
	qty, ok1 := tca.ToDecimal(a.FilledQuantity)
	avg, ok2 := tca.ToDecimal(a.AveragePrice)
	arr, ok3 := tca.ToDecimal(a.ArrivalPrice)
	total, ok4 := tca.ToDecimal(a.ShortfallBps)
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return nil, false
	}
	p := &orderpb.ExecutionAttributionRecorded{
		OrderId:          st.GetOrderId(),
		PortfolioId:      st.GetPortfolioId(),
		InstrumentId:     st.GetInstrumentId(),
		Side:             st.GetSide(),
		TerminalStatus:   st.GetStatus(),
		OrderedQuantity:  st.GetOrderedQuantity(),
		FilledQuantity:   qty,
		AverageFillPrice: avg,
		ArrivalPrice:     arr,
		ArrivalAt:        st.GetArrivalAt(),
		ShortfallBps:     total,
		Benchmark:        orderpb.Benchmark_BENCHMARK_ARRIVAL,
		Quality:          orderpb.AttributionQuality_ATTRIBUTION_QUALITY_TOTAL_ONLY,
		Slices:           int32(a.Slices),
		MeasuredSlices:   int32(a.Measured),
		MeasuredAt:       timestamppb.New(now.UTC()),
	}
	if a.Quality != tca.QualityDecomposed {
		return p, true
	}
	spread, ok5 := tca.ToDecimal(a.SpreadBps)
	impact, ok6 := tca.ToDecimal(a.ImpactBps)
	timing, ok7 := tca.ToDecimal(a.TimingBps)
	if !ok5 || !ok6 || !ok7 {
		// ONE UNREPRESENTABLE LEG DROPS THE WHOLE RECORD, not just that leg. A
		// record carrying two of three legs and a DECOMPOSED quality would be
		// read as a decomposition whose parts do not sum, and a record carrying
		// two legs and TOTAL_ONLY would be a shape nothing else in this schema
		// produces.
		return nil, false
	}
	p.SpreadBps, p.ImpactBps, p.TimingBps = spread, impact, timing
	p.Quality = orderpb.AttributionQuality_ATTRIBUTION_QUALITY_DECOMPOSED
	return p, true
}

// withAttribution appends the once-per-decision attribution to a terminal
// transition's announcement, so the measurement commits in the SAME transaction
// as the state change that produced it.
//
// ONE IDIOM, FIVE CALL SITES, AND THAT IS THE POINT. The five transitions a
// decision can end on — a last fill in work(), a last fill adopted from the
// venue, a cancellation, a withdrawal or expiry ADOPTED from the venue (#924),
// and a scheduled parent's expiry — are in three files, and a copied `append` at
// each is how one of them would quietly stop attributing. attributionRecords
// answers nil for everything that is not a measurable decision, so this is safe
// to call at any terminal Save and says nothing about which of them are parents.
//
// IT DOES NOT HAVE TO BE TRUSTED NOT TO BE CALLED ON A NON-TERMINAL TRANSITION:
// attributionRecords refuses a non-terminal state itself, so a caller that grows
// a new branch cannot publish a cost for an order that is still working.
func (s *Service) withAttribution(ctx context.Context, st *orderpb.OrderState, now time.Time, announce ...outbox.Record) []outbox.Record {
	return append(announce, s.attributionRecords(ctx, st, now)...)
}
