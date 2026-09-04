package order

import (
	"math/big"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/execution/tca"
	"github.com/eighred/kanz/services/oms/internal/schedule"
)

// REALISED PARTICIPATION, ONCE PER WORKED DECISION (#1007).
//
// # What was missing
//
// EXECUTION_ALGO_POV refuses a parent whose FORECAST participation would exceed
// max_participation_rate, at admission, once. Nothing measured what the
// participation turned out to be — not a metric, not a FACT, not any post-trade
// record — so a cap of 0.08 read from every screen, every audit record and every
// allocator report as a rate that was honoured, with nothing anywhere able to
// confirm or contradict it. A cap enforced against a forecast and never measured
// against the tape is a promise with no evidence behind it, and the direction of
// the error is the bad one: a forecast is most wrong exactly when the market is
// thinnest, which is exactly when the cap matters.
//
// # It measures; it decides nothing, and it never fails a transition
//
// This rides attributionRecords, and inherits its error policy whole: a failure
// here loses a measurement and never refuses the state change that produced it.
// Putting an analytic on the capital path is the failure #866's own doc rules
// out at length, and a participation figure is a strictly weaker claim than the
// shortfall beside it.
//
// # IT ALSO INHERITS THAT RECORD'S SILENCES, AND ONE OF THEM IS NOT OBVIOUS
//
// ExecutionAttributionRecorded is not published at all for a decision with NO
// ARRIVAL MARK — an instrument the price spine never covered. Participation does
// not need an arrival mark and would be perfectly measurable there, but it has
// nowhere to travel: a second FACT would be a second answer to "how did this
// decision execute", joined on order_id by every reader forever. So on an estate
// whose price spine covers nothing, participation is unmeasured too, and the gap
// is visible where the cause is — no_arrival_mark on
// kanz_oms_execution_attributions_total — rather than as a participation
// coverage problem it is not.
//
// # THE SCHEDULE IS RE-DERIVED, NOT REMEMBERED
//
// The intervals come from algo.Plan.Interval over the plan planFromOrder builds
// from the parent's own durable fields, and each child is matched to its slice
// index through schedule.ChildID — the same derivation the driver used to create
// it. Nothing is stored, nothing is carried on the order, and two pods measuring
// the same finished parent measure it over the same minutes. That is the
// property test/arch/schedule_is_derived_test.go protects for the schedule
// itself, and there is no reason for its measurement to be weaker.

// participation outcome labels. Bounded by construction, like the attribution
// outcomes beside them: this counter sees every terminal decision and an
// unbounded label would be a cardinality leak.
const (
	participationNotWorked    = "not_worked"
	participationMeasured     = "measured"
	participationPartial      = "partial"
	participationUnobservable = "unobservable"
)

// ParticipationQualities is every label value the participation counter can
// emit, in the order tca declares the qualities.
//
// IT EXISTS SO THE COUNTER CAN BE SEEDED AT ZERO, for the reason
// AttributionOutcomes exists, and here the argument is sharper. A CounterVec
// exports NO series for a label value it has never been incremented with, so an
// OMS wired to no realised-volume feed — which is every deployment until the
// composition root binds one — exports nothing at all under this name. "The
// participation of every decision is unobservable" and "this build does not
// measure participation" would then be the same observation: no series. Seeded,
// the first is a rising `unobservable` and the second is a flat set of zeros,
// and those are different things to go and fix.
//
// THE LIST IS EXPORTED FROM HERE RATHER THAN RETYPED AT THE COMPOSITION ROOT, so
// a quality added to tca cannot be counted in the code and absent from the
// metric until the first time it fires.
//
// Callers must copy before mutating — it is a package-level slice.
var ParticipationQualities = []string{
	participationNotWorked,
	participationMeasured,
	participationPartial,
	participationUnobservable,
}

// participationLabel maps a measured quality to its metric label.
//
// tca.ParticipationUnknown IS NOT REPRESENTED, and that is deliberate rather
// than an omission: it is the zero value MeasureParticipation never returns, and
// giving it a label would make "the measurement did not run" indistinguishable
// from an outcome it produced. It falls to the default, which is the same
// unobservable bucket a live source that could see nothing lands in — the
// conservative reading, and the counter's seeded zeros are what make an
// impossible value showing up there visible as a step from nothing.
func participationLabel(q tca.ParticipationQuality) string {
	switch q {
	case tca.ParticipationNotWorked:
		return participationNotWorked
	case tca.ParticipationMeasured:
		return participationMeasured
	case tca.ParticipationPartial:
		return participationPartial
	default:
		return participationUnobservable
	}
}

// participationQualityOf maps a measured quality onto the wire enum.
//
// AN UNMAPPED VALUE BECOMES UNSPECIFIED, NEVER A PLAUSIBLE ONE. order.v1's own
// doc says a record that does not say what it was measured against is a record
// nothing may aggregate, and defaulting an unknown quality to MEASURED would
// publish a participation figure asserting a confidence nobody computed.
func participationQualityOf(q tca.ParticipationQuality) orderpb.ParticipationQuality {
	switch q {
	case tca.ParticipationNotWorked:
		return orderpb.ParticipationQuality_PARTICIPATION_QUALITY_NOT_WORKED
	case tca.ParticipationMeasured:
		return orderpb.ParticipationQuality_PARTICIPATION_QUALITY_MEASURED
	case tca.ParticipationPartial:
		return orderpb.ParticipationQuality_PARTICIPATION_QUALITY_PARTIAL
	case tca.ParticipationUnobservable:
		return orderpb.ParticipationQuality_PARTICIPATION_QUALITY_UNOBSERVABLE
	default:
		return orderpb.ParticipationQuality_PARTICIPATION_QUALITY_UNSPECIFIED
	}
}

// WithRealisedVolume binds the tape a worked decision's participation is
// measured against (#1007).
//
// UNSET IS A VALID DEPLOYMENT AND IT IS NOT SILENT. Without it every worked
// parent's participation is published as UNOBSERVABLE and counted as such, which
// is the honest answer — this OMS cannot see the tape — and is visibly different
// from a measured rate. What it must never become is a zero: see
// tca.MeasureParticipation.
func WithRealisedVolume(v tca.RealisedVolume) ServiceOption {
	return func(s *Service) { s.realisedVolume = v }
}

// WithParticipationCounter supplies the COVERAGE counter for the participation
// measurement, labelled by quality. Nil ⇒ nothing is counted, which is the dev
// posture and not the deployed one.
func WithParticipationCounter(c *prometheus.CounterVec) ServiceOption {
	return func(s *Service) { s.participations = c }
}

// WithParticipationBreachCounter supplies the counter that turns "the forecast
// was wrong" from an inference into an EVENT (#1007).
//
// IT IS THE ONE SERIES THAT SAYS A CONTROL DID NOT HOLD. Everything else this
// measurement publishes is a number in a FACT somebody has to go and read; this
// is the alertable signal, and it moves only on a measured interval whose rate
// exceeded the order's own declared cap. Nil ⇒ a breach is recorded in the FACT
// and nothing pages, which is the dev posture.
func WithParticipationBreachCounter(c *prometheus.CounterVec) ServiceOption {
	return func(s *Service) { s.capExceeded = c }
}

func (s *Service) countParticipation(q tca.ParticipationQuality) {
	if s.participations == nil {
		return
	}
	s.participations.WithLabelValues(participationLabel(q)).Inc()
}

// measureParticipation measures what fraction of the tape a finished decision
// actually was, and counts the outcome.
//
// children is what attributionSlices already read, threaded through rather than
// re-read: a second ListByParent would be a second answer to "what traded into
// this decision", and the two could disagree if a cancel landed between them —
// so the cost figure and the participation figure on ONE record would describe
// two different sets of children.
//
// AN UNSCHEDULED ORDER IS NOT_WORKED RATHER THAN UNOBSERVABLE. "This order had
// no working window" and "this order's window could not be seen" are different
// operator problems and only the second is a market-data gap; collapsing them
// would make the coverage counter read as a broken feed on an estate that simply
// trades most of its orders whole.
func (s *Service) measureParticipation(st *orderpb.OrderState, children []*orderpb.OrderState) tca.Participation {
	p := tca.MeasureParticipation(st.GetInstrumentId(), s.participationIntervals(st, children), s.realisedVolume)
	s.countParticipation(p.Quality)
	if s.capExceeded != nil && p.Exceeds(participationCapOf(st)) {
		// LABELLED BY THE VENUE THE WORST INTERVAL WAS WORKED ON AND BY THE
		// INSTRUMENT, because those are what a desk acts on: a cap exceeded on a
		// thin pair on one book is a routing or a sizing decision about that book,
		// and an aggregate count is a number nobody can do anything with. Both
		// label spaces are bounded by what this OMS actually trades rather than by
		// anything a caller can invent — an instrument reaches here only by having
		// been admitted, routed and filled.
		s.capExceeded.WithLabelValues(p.MaxSliceVenue, st.GetInstrumentId()).Inc()
		s.logger.Warn("oms: a worked order's REALISED participation exceeded the cap it was "+
			"admitted under — the forecast volume profile it was scheduled against was thicker "+
			"than the tape that printed, so children were sized for volume that did not arrive",
			"order_id", st.GetOrderId(), "instrument", st.GetInstrumentId(),
			"venue", p.MaxSliceVenue, "slice", p.MaxSliceIndex,
			"realised_rate", p.MaxSlice.FloatString(6),
			"cap", participationCapOf(st).FloatString(6),
			"measured_intervals", p.Measured, "intervals", p.Intervals)
	}
	return p
}

// participationIntervals pairs each of the decision's slice intervals with the
// child that worked it.
//
// # THE MATCH IS BY DERIVED ID, NOT BY ORDER OF ARRIVAL
//
// schedule.ChildID(parent, i) is how the driver named every child, so recomputing
// it is how a child is identified with its slice — and the store's list order,
// the creation order and the fill order are all irrelevant. That matters because
// a schedule can have HOLES: a crash between two creations leaves slice 2 missing
// while 3 exists, which is the whole reason the driver uses a predicate over
// indices rather than a count. Matching positionally would attribute slice 3's
// fills to slice 2's minutes for every slice after the hole.
//
// A CHILD THAT MATCHES NO INDEX IS DROPPED, and it is a state worth naming
// rather than smoothing over: it means an order carries a parent_order_id whose
// schedule does not derive it, which authorizeChild refuses at admission. There
// is nothing truthful to measure it as, and inventing an interval for it would
// put fills into minutes the schedule never claimed.
func (s *Service) participationIntervals(st *orderpb.OrderState, children []*orderpb.OrderState) []tca.Interval {
	if st.GetExecutionSchedule() == nil || len(children) == 0 {
		return nil
	}
	plan, err := planFromOrder(st)
	if err != nil {
		return nil
	}
	index := make(map[string]int, plan.Slices)
	for i := range plan.Slices {
		index[schedule.ChildID(st.GetOrderId(), i)] = i
	}

	out := make([]tca.Interval, 0, len(children))
	for _, c := range children {
		i, ok := index[c.GetOrderId()]
		if !ok {
			continue
		}
		from, to, ok := plan.Interval(i)
		if !ok {
			continue
		}
		out = append(out, tca.Interval{
			Index: i,
			// THE CHILD'S OWN VENUE, NOT THE PARENT'S. A parent that names no venue
			// leaves each child to the router's choice, so the book a slice was
			// worked on is a property of the child — and dividing a fill on OKX by
			// Binance's candles is a denominator that never carried it.
			Venue:  c.GetVenue(),
			From:   from,
			To:     to,
			Filled: nilIfUnset(c.GetFilledQuantity()),
		})
	}
	return out
}

// participationCapOf is the cap the decision was admitted under, or nil.
//
// READ FROM THE ORDER AND NEVER FROM A CONFIGURATION. The cap is a term of the
// order — a mandate-level decision about a fund, in pov.go's words — so an OMS
// default would either flag orders a desk permits or stay silent on ones it does
// not, while looking from every screen like a control that was configured.
func participationCapOf(st *orderpb.OrderState) *big.Rat {
	c := st.GetExecutionSchedule().GetMaxParticipationRate()
	if c == nil {
		return nil
	}
	return dec.FromProto(c)
}

// attachParticipation writes a measurement onto the attribution FACT.
//
// # A RATE THAT IS NOT EXACTLY REPRESENTABLE IS DROPPED, AND THE QUALITY DROPS
// # WITH IT
//
// A participation rate is a ratio of two exact quantities and can be a
// non-terminating decimal — 1/3 of a tape is a perfectly ordinary answer — so
// unlike the money figures beside it this one really can fail to convert. When
// it does, the record must not keep a quality of MEASURED beside an absent rate:
// that shape says "we measured it and here is nothing", which is the exact
// confusion the quality vocabulary exists to prevent. It degrades to
// UNOBSERVABLE, which is true — this record cannot show what the participation
// was — and the counts beside it still say how much of the decision was covered.
//
// IT NEVER DROPS THE WHOLE RECORD, unlike an unrepresentable cost leg. The
// shortfall is the headline #866 exists for and it is unaffected; losing it
// because a ratio would not round would trade the measurement that works for the
// one that did not.
func attachParticipation(p *orderpb.ExecutionAttributionRecorded, st *orderpb.OrderState, m tca.Participation) {
	p.ParticipationQuality = participationQualityOf(m.Quality)
	p.ParticipationIntervals = int32(m.Intervals)
	p.MeasuredParticipationIntervals = int32(m.Measured)
	p.MaxSliceParticipationIndex = -1
	p.ExecutionAlgo = st.GetExecutionSchedule().GetAlgo()
	// THE CAP TRAVELS EVEN WHEN THE RATE DOES NOT. A reader holding an
	// UNOBSERVABLE record still needs to know what was claimed — that is the
	// statement the missing measurement failed to check — and joining back to
	// OrderState for it is what makes every consumer's version of "was the cap
	// honoured" slightly different.
	if c, ok := tca.ToDecimal(participationCapOf(st)); ok {
		p.ParticipationCap = c
	}
	if m.Overall == nil || m.MaxSlice == nil {
		return
	}
	overall, ok1 := tca.ToDecimal(m.Overall)
	worst, ok2 := tca.ToDecimal(m.MaxSlice)
	if !ok1 || !ok2 {
		p.ParticipationQuality = orderpb.ParticipationQuality_PARTICIPATION_QUALITY_UNOBSERVABLE
		return
	}
	p.RealisedParticipationRate = overall
	p.MaxSliceParticipationRate = worst
	p.MaxSliceParticipationIndex = int32(m.MaxSliceIndex)
}
