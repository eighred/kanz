package order

import (
	"fmt"
	"math/big"
	"time"

	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/execution/algo"
	"github.com/eighred/kanz/internal/execution/marketview"
	"github.com/eighred/kanz/internal/volprofilefeed"
)

// WHICH MARKET A SCHEDULE IS DERIVED AGAINST, AND WHY IT IS PINNED (#897).
//
// # The three paths, and the reason they must agree exactly
//
// This service enters internal/execution/algo in three places:
//
//	validateSchedule  admission — is this schedule workable at all?
//	authorizeChild    child admission — is this inbound order really a slice?
//	schedule.Due      the driver — which children should exist right now?
//
// All three passed algo.UnknownMarket until #897, so a VWAP or POV order was
// refused at the first. Giving them a market view is not one change but four
// decisions, and the sharp one is authorizeChild: it re-derives the parent's
// whole schedule and compares the inbound child's quantity as an EXACT RATIONAL.
//
// If each path built a view at its own `now` over whatever profile was current,
// the three would disagree the moment a session boundary passed between them —
// and the driver's own child would be refused as a forgery, on a parent that then
// stops advancing while every screen shows it working. On two pods it would not
// even need a session boundary: two folds are two curves.
//
// So the market is PINNED. Admission resolves one published profile version,
// records it on the order, and every later derivation resolves THAT version. The
// curve becomes an input the order stores durably, exactly like its quantity and
// its window, which is the property test/arch/schedule_is_derived_test.go's whole
// argument rests on: same inputs, same schedule, on every pod and after every
// restart.
//
// # NOTHING HERE SWITCHES ON THE ALGORITHM'S NAME
//
// The obvious implementation asks "is this VWAP or POV?" and resolves a profile
// only then. That would be a SECOND LIST of which algorithms are volume-driven,
// beside algo.Registered(), and the two would drift the first time one grew a
// member — with the failure that an order naming a new volume-driven algorithm is
// admitted with no curve and refused for a reason nothing explains.
//
// Instead the best available view is offered to every schedule and the ALGORITHM
// decides. TWAP ignores it and is unaffected; VWAP and POV ask, and refuse on
// UNKNOWN. Whether the view was CONSULTED is then observed rather than predicted
// (see consultedView), which is also what keeps the pin honest: a TWAP order does
// not acquire a profile version it never read.

// VolumeProfiles is the slice of internal/volprofilefeed.Registry this service
// needs.
//
// DECLARED AT THE CONSUMER, so the OMS depends on two methods rather than on a
// registry's whole surface, and so a test can drive admission with a fake without
// standing up a broker. It is the same stance internal/prediction/registry takes
// with LogSubscriber.
type VolumeProfiles interface {
	// Current is the newest published profile for a series. Read at ADMISSION
	// only — see Registry.Current.
	Current(volprofilefeed.Series) (volprofilefeed.Profile, bool)

	// Resolve is the exact version a parent order pinned. No nearest match.
	Resolve(volprofilefeed.Series, string) (volprofilefeed.Profile, bool)
}

// WithVolumeProfiles binds the published intraday volume profiles (#897).
//
// UNSET IS A VALID DEPLOYMENT AND IT IS NOT SILENT. Without it this service holds
// no market data at all, exactly as it did before #897, and every volume-driven
// order is refused at admission under NO_VOLUME_PROFILE — with a reason that says
// the OMS is not bound to a profile feed rather than one that blames the market.
func WithVolumeProfiles(p VolumeProfiles) ServiceOption {
	return func(s *Service) { s.volumeProfiles = p }
}

// shapeOf turns a published profile into the curve a view integrates.
//
// A NON-KNOWN PROFILE HAS NO SHAPE, and that is the whole three-value model
// arriving intact: ABSENT, STALE and TOO_FEW_SESSIONS all produce ok=false here,
// so the algorithm refuses rather than being handed a flat curve — which would be
// TWAP under another name.
func shapeOf(p volprofilefeed.Profile) (marketview.Shape, bool) {
	if !p.Known() {
		return marketview.Shape{}, false
	}
	exp := make([]*big.Rat, len(p.Expected))
	copy(exp, p.Expected)
	return marketview.Shape{
		InstrumentID: p.Series.InstrumentID,
		Bucket:       p.Bucket,
		// THE PRODUCER'S HORIZON, NOT THIS POD'S CONFIGURATION. It bounds how long
		// an interval the curve may be integrated over, and a bound read locally
		// would make that refusal a property of the reader — two pods deployed with
		// different settings would then derive different schedules from one order.
		Horizon:  p.Horizon,
		Expected: exp,
	}, true
}

// viewOf turns a resolved profile into the pinned view a schedule integrates.
//
// IT CONSTRUCTS NO algo.UnknownMarket and returns nil instead, deliberately. Two
// functions in this package may answer UNKNOWN — currentMarket and pinnedMarket —
// because each of them is a lookup that failed, and a third site would be a path
// that refuses a volume-driven order without ever asking anything. A helper that
// returned UnknownMarket would be exactly that third site, and
// test/arch/pinned_profile_is_the_only_market_test.go would say so.
func viewOf(p volprofilefeed.Profile) (algo.MarketView, bool) {
	sh, ok := shapeOf(p)
	if !ok {
		return nil, false
	}
	v, err := marketview.NewPinned(sh, p.Version)
	if err != nil {
		return nil, false
	}
	return v, true
}

// storedMarket resolves the curve the ORDER ITSELF carries (#943).
//
// # Why an order carries a curve at all
//
// #897 stamped a VERSION and left the shape it names on the bus. A pod resolves
// the version by replaying market.crypto.volume_profile into the registry, and
// the MARKET stream retains 24h while the registry retains seven days — so the
// same pin resolved on a pod that had been up all week and resolved to nothing on
// the pod that replaced it. A parent worked over more than a day, on a pod that
// then rolled, stopped advancing until somebody cancelled and re-submitted it.
//
// The curve is a schedule INPUT, so it belongs where the other inputs are: on the
// order, in Postgres, backed up and restored. This reads it back.
//
// # THE STORED CURVE IS CHECKED AGAINST THE PIN, NEVER TRUSTED BESIDE IT
//
// volprofilefeed.Decode recomputes the version from the message's own content and
// refuses one that does not describe itself; this then requires that content hash
// to equal the version field 7 carries, and requires the curve to name THIS
// order's (instrument, venue). A blob that fails any of the three is UNKNOWN and
// the parent stops — the same answer as no curve at all, and for a sharper
// reason: a stored shape that disagrees with the version the audit record carries
// would attribute every fill to a schedule that was never derived, which is the
// mislabelling the whole pin exists to prevent.
//
// The bool is "there was something here to resolve", not "it resolved". A false
// with a non-nil message is a REFUSAL and must not fall through to the registry:
// a bad blob silently replaced by a good registry answer is a corruption nothing
// would ever report.
func (s *Service) storedMarket(instrumentID, venue, version string,
	wire *marketpb.VolumeProfile) (algo.MarketView, bool) {

	p, err := volprofilefeed.Decode(wire)
	if err != nil {
		return nil, false
	}
	if p.Version != version {
		return nil, false
	}
	// THE SERIES CHECK IS REDUNDANT TODAY AND KEPT DELIBERATELY. versionOf hashes
	// the instrument and the venue, so a curve measured on another book cannot
	// carry this order's pin and the comparison above already refuses it — a
	// mutation deleting these three lines survives every test, and that is the
	// honest state of them rather than a gap. What they defend is the day the
	// series leaves versionOf's input list: from then on this is the only thing
	// between a parent and a curve measured on a different exchange, where the same
	// instrument has a different intraday shape. The premise is under test at
	// internal/volprofilefeed.TestVersion_DependsOnTheSeries, which fails loudly if
	// it ever stops holding.
	if p.Series.InstrumentID != instrumentID || p.Series.Venue != venue {
		return nil, false
	}
	return viewOf(p)
}

// pinnedMarket resolves the exact profile version an order was planned against.
//
// IT RETURNS algo.UnknownMarket RATHER THAN AN ERROR when the version cannot be
// resolved, and the distinction matters: a TWAP parent legitimately pins nothing,
// so "no view" is the ordinary case and must not be an error the driver reports.
// What makes an UNRESOLVABLE pin loud is the algorithm — VWAP and POV refuse on
// UNKNOWN — so the failure surfaces as an unworkable schedule naming the slice it
// could not size, which is the same message an operator would get for a market
// nobody measured. The second return says which of the two happened, for the log.
//
// # THE ORDER IS ASKED FIRST, AND THE REGISTRY IS THE FALLBACK (#943)
//
// A schedule that reaches the registry at all is a schedule whose answer depends
// on how long this pod has been running, because what a pod can replay is bounded
// by the bus and what an order carries is not. So an order that carries its curve
// is derived from its curve — on a pod with an empty registry, on a pod that has
// never seen the FACT, and after the version has aged off the stream entirely.
//
// The registry path remains for exactly one population: parents admitted before
// this field existed, which carry a version and no curve. Their behaviour is
// unchanged, including the stop.
func (s *Service) pinnedMarket(instrumentID, venue, version string,
	wire *marketpb.VolumeProfile) (algo.MarketView, bool) {

	if version == "" || instrumentID == "" || venue == "" {
		return algo.UnknownMarket{}, false
	}
	if wire != nil {
		v, ok := s.storedMarket(instrumentID, venue, version, wire)
		if !ok {
			return algo.UnknownMarket{}, false
		}
		return v, true
	}
	if s.volumeProfiles == nil {
		return algo.UnknownMarket{}, false
	}
	p, ok := s.volumeProfiles.Resolve(volprofilefeed.Series{InstrumentID: instrumentID, Venue: venue}, version)
	if !ok {
		return algo.UnknownMarket{}, false
	}
	v, ok := viewOf(p)
	if !ok {
		return algo.UnknownMarket{}, false
	}
	return v, true
}

// scheduleMarket is the view for an order that ALREADY EXISTS — the driver's
// path and the child-admission path.
//
// IT NEVER READS "CURRENT". That is the one rule this file exists to hold: a
// derivation after admission resolves the recorded version or nothing, so the
// schedule a pod derives cannot depend on when it looked or on which pod it is.
func (s *Service) scheduleMarket(st *orderpb.OrderState) algo.MarketView {
	sch := st.GetExecutionSchedule()
	v, _ := s.pinnedMarket(st.GetInstrumentId(), st.GetVenue(),
		sch.GetVolumeProfileVersion(), sch.GetVolumeProfile())
	return v
}

// currentMarket is the view for a schedule being ADMITTED, and the profile it
// would be pinned to.
//
// ADMISSION IS THE ONE MOMENT "whatever is newest" IS THE RIGHT QUESTION, because
// it is the moment the answer is RECORDED. Everything after it resolves the
// record.
//
// IT RETURNS THE MESSAGE RATHER THAN THE VERSION STRING (#943), because both are
// stamped on the order and they must be the same profile. Handing back a version
// and making the caller fetch the curve separately would be two lookups that can
// disagree — and the one thing a stored curve must never do is disagree with the
// version beside it.
func (s *Service) currentMarket(instrumentID, venue string) (algo.MarketView, *marketpb.VolumeProfile) {
	if s.volumeProfiles == nil || instrumentID == "" || venue == "" {
		return algo.UnknownMarket{}, nil
	}
	p, ok := s.volumeProfiles.Current(volprofilefeed.Series{InstrumentID: instrumentID, Venue: venue})
	if !ok {
		return algo.UnknownMarket{}, nil
	}
	v, ok := viewOf(p)
	if !ok {
		return algo.UnknownMarket{}, nil
	}
	// THE REGISTRY REFUSES A PROFILE THAT CARRIES NO MESSAGE, so this is not a
	// reachable state through the bus — it is here because a hand-built fake in a
	// test is, and a schedule pinned to a version whose curve cannot be stored
	// would be the #943 gap re-entering through the composition root.
	if p.Wire == nil {
		return algo.UnknownMarket{}, nil
	}
	return v, p.Wire
}

// consultedView records whether the schedule actually ASKED about volume.
//
// # Why the pin is observed rather than predicted
//
// A parent order records the profile version its schedule was derived against,
// and that record has to be TRUE. Stamping it on every scheduled order would put
// a version on TWAP parents, which read no curve at all — an audit record
// asserting that a schedule was sized against a market it never looked at, and
// the mislabelling this whole issue exists to prevent, arrived at from the
// opposite direction.
//
// The alternative to observing is a list of which algorithms are volume-driven,
// which is a second copy of algo.Registered() and drifts from it the first time
// one grows a member. Watching the seam costs a bool and cannot drift: an
// algorithm that asks is an algorithm whose schedule depends on the answer.
type consultedView struct {
	algo.MarketView
	asked bool
}

func (c *consultedView) ExpectedVolume(instrumentID string, from, to time.Time) (*big.Rat, bool) {
	c.asked = true
	return c.MarketView.ExpectedVolume(instrumentID, from, to)
}

// volumeProfileGap says WHY this OMS could not size a volume-driven schedule.
//
// # It is the difference between four operator actions
//
// NO_VOLUME_PROFILE is one refusal code with four causes, and a client or a desk
// reading "no volume profile" alone cannot tell which of them applies:
//
//	no feed bound       this OMS is not wired to the profile spine at all —
//	                    a deployment fault, and nothing about this order.
//	no venue named      a volume-driven schedule must say WHICH book it is
//	                    measured against; the operator fixes it in the command.
//	nothing published   market-ingest has never announced this series — a
//	                    missing subscription on the edge.
//	not yet KNOWN       announced, and the curve is ABSENT, STALE or short of
//	                    the desk's session floor. This one resolves itself, and
//	                    the counts say how far off it is.
//
// "Nothing configured" and "checked, and nothing is known" must never look the
// same, and on this path they would.
func (s *Service) volumeProfileGap(instrumentID, venue string) string {
	if s.volumeProfiles == nil {
		return "this OMS is not bound to a volume-profile feed at all (no WithVolumeProfiles at " +
			"the composition root), so it can see no volume for any instrument — a deployment " +
			"fault rather than anything about this order or this market"
	}
	if venue == "" {
		return "the order names no venue, and a volume profile is keyed by (instrument, venue) " +
			"because two venues on one instrument have different intraday shapes — a schedule " +
			"sized against a blend of books it will never touch is a wrong number, not a " +
			"simplification. Name the venue this parent is worked on"
	}
	p, ok := s.volumeProfiles.Current(volprofilefeed.Series{InstrumentID: instrumentID, Venue: venue})
	if !ok {
		return fmt.Sprintf("nothing has ever published an intraday volume profile for %s on %s — "+
			"the market-data edge folds one per subscribed (instrument, venue), so this is a "+
			"missing subscription there rather than a thin market", instrumentID, venue)
	}
	if p.Known() {
		// A MEASURED CURVE IS PRESENT AND THE SCHEDULE STILL COULD NOT BE SIZED,
		// which is a FIFTH cause and not one of the four above.
		//
		// IT IS REACHABLE WITHOUT A DEFECT, and the ordinary way is a WINDOW
		// LONGER THAN THE HISTORY BEHIND THE CURVE: nothing bounds a working
		// window at admission — refuseUndrivableSchedule bounds the slice RATE,
		// not the span — so a parent worked over more than the producer's
		// retention horizon asks the profile to extrapolate past everything
		// measured, and it refuses. A slice interval that lands in a bucket
		// nothing trades in is the other, and it carries its own algo sentinel.
		//
		// Reporting either with the other four's text would tell an operator their
		// market has not been measured while a measured curve sits in front of
		// them — the stale-premise failure, arriving in a refusal.
		return fmt.Sprintf("a measured profile for %s on %s IS present (version %s, %d completed "+
			"session(s) retained over %s, as of %s) and the schedule still could not be sized "+
			"against it — most often a working window longer than that retention, which asks the "+
			"curve to extrapolate past every session it was built from. The algorithm's own report "+
			"below names the interval it could not answer for",
			instrumentID, venue, p.Version, p.Sessions, p.Horizon, p.AsOf.Format(time.RFC3339))
	}
	return fmt.Sprintf("the newest published profile for %s on %s is %s over %d completed "+
		"session(s) (%d of %d intraday buckets observed, as of %s), so it carries no measured curve",
		instrumentID, venue, p.Verdict, p.Sessions, p.Window.Observed, p.Window.Buckets,
		p.AsOf.Format(time.RFC3339))
}
