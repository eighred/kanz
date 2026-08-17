package approval

import (
	"errors"
	"math/big"

	"github.com/prometheus/client_golang/prometheus"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/dualcontrol"
	"github.com/eighred/kanz/services/oms/internal/compliance"
)

// THE ARMING GATE, AND WHY THERE ARE TWO VARIABLES (#410).
//
// The owner's ruling on the order path is three clauses:
//
//	threshold unset  ⇒ the control is ABSENT, and that fact is OBSERVABLE
//	threshold set    ⇒ orders at or above it require dual control
//	"require dual control" set with NO threshold ⇒ REFUSE TO START, naming both
//
// The third clause is the one worth stating out loud, because the obvious
// alternative is a default. A default threshold silently picks a number nobody
// chose, on a control whose entire purpose is that a human decided the number.
// An operator who copies DATAMASTER_REQUIRE_DUAL_CONTROL across by analogy —
// which is exactly what the estate's naming invites — would get a control that
// looks armed and holds whatever the default happened to be. So the OMS refuses
// to start and names both variables. config.Load owns that refusal, for the
// reason the OMS_VENUE_ACCOUNTS check gives: it is a pure function of strings
// already in hand, so it is the FIRST thing that fails and the easiest to test.
//
// DATAMASTER NEEDS NO THRESHOLD AND THIS IS NOT AN INCONSISTENCY. Its rule is
// "every override" and #495 recorded why: "a control with no threshold has
// nothing to calibrate". "Every order takes two people" is not a workable rule,
// so this one has a number, and the number is the only new concept — the arming
// flag is the same concept spelled the same way (OMS_REQUIRE_DUAL_CONTROL,
// beside OMS_REQUIRE_MANDATE, OMS_REQUIRE_VENUE_ACCOUNT,
// OMS_REQUIRE_VERIFIED_ACCOUNT and OMS_REQUIRE_ORDER_TYPE_SUPPORT).
//
// # UNARMED IS NOT UNRECORDED, and that is the half that ships today
//
// With a threshold set and the flag off, nothing is held: every order is still
// admitted on one signature. What changes is that each one is CLASSIFIED against
// the threshold and counted, and each order at or above it is logged with the
// act and the digest a second signature would have had to cover. So "how much of
// the order flow went through with one signature, and how much of it was large"
// is a readable number BEFORE anyone arms the control — which is the same stance
// #495 took with kanz_datamaster_overrides_total{signatures="single_signed"},
// and the same stance OMS_REQUIRE_MANDATE and OMS_REQUIRE_VERIFIED_ACCOUNT take:
// build the control, count the gap, arm it with the list in hand.

// Gate classifies an order against the dual-control threshold.
//
// It answers whether an order requires a second signature. It does NOT hold a
// pending order, and it deliberately has no opinion about where one should live
// — see the package doc. The placement consumes Decision.
type Gate struct {
	// require arms the refusal. See NewGate: today it cannot be true, because
	// nothing can hold an order awaiting approval yet.
	require bool
	// threshold is the notional at or above which an order requires dual
	// control. nil means the control is ABSENT.
	threshold *big.Rat
	// marks values an order that carries no price of its own. Optional; without
	// it every MARKET and STOP order is unvaluable — see Decide.
	marks   compliance.MarkSource
	metrics *metrics
}

// NewGate builds the gate. threshold nil ⇒ the control is absent.
//
// PRODUCTION CANNOT REACH AN ARMED GATE TODAY, and the refusal is config.Load's
// rather than this constructor's: there is nowhere to put an order awaiting
// approval until #410's proposals table is built, so arming would REJECT every
// order at or above the threshold instead of holding it — a trading outage
// delivered by a security improvement, which is the failure every other
// OMS_REQUIRE_* flag was deliberately shipped OFF to avoid.
//
// This constructor still accepts require=true, deliberately. The armed branch of
// Decide is the behaviour the proposals table will depend on, and a branch no test can
// construct is a branch nobody has checked. Datamaster's refusal has the same
// two halves (its composition root refuses the combination, and dualControlArmed
// re-checks inside the server) for the same reason: a constructor callable from
// a test is not a guarantee.
func NewGate(require bool, threshold *big.Rat, marks compliance.MarkSource, reg prometheus.Registerer) (*Gate, error) {
	if require && threshold == nil {
		// A gate armed with no number would compare every order against nothing.
		// config.Load refuses this first and names both variables; this is the
		// second line.
		return nil, errors.New("approval: OMS_REQUIRE_DUAL_CONTROL is set with no " +
			"OMS_DUAL_CONTROL_MIN_NOTIONAL — there is no threshold to compare an order against, and " +
			"defaulting to one would pick a number nobody chose")
	}
	if threshold != nil && threshold.Sign() <= 0 {
		return nil, errors.New("approval: OMS_DUAL_CONTROL_MIN_NOTIONAL must be positive — a " +
			"non-positive threshold puts every order at or above it, including the ones with no notional at all")
	}
	return &Gate{require: require, threshold: threshold, marks: marks, metrics: newMetrics(reg)}, nil
}

// Posture names how an order stood against the threshold. A CLOSED SET of four,
// because it is a metric label: an order id or an instrument here would be
// unbounded cardinality, and this counter runs on every admission.
type Posture string

const (
	// PostureAbsent: no threshold is configured, so nothing was compared. It is
	// NOT "below the threshold" — "nothing configured" and "checked, and fine"
	// must not look the same on a dashboard.
	PostureAbsent Posture = "absent"
	// PostureBelow: valued, and under the threshold.
	PostureBelow Posture = "below_threshold"
	// PostureAtOrAbove: valued, and at or above the threshold. Inclusive at the
	// boundary, per the ruling's wording.
	PostureAtOrAbove Posture = "at_or_above_threshold"
	// PostureUnvaluable: a threshold is configured and this order could not be
	// valued against it — a MARKET or STOP order with no fresh mark, or a
	// notional that is not representable.
	//
	// IT IS ITS OWN LABEL AND NOT below_threshold, WHICH IS THE POINT. An order
	// whose size nobody could compute must never be counted as small. On a
	// calibration dashboard a large unvaluable bucket means the threshold cannot
	// be trusted yet, and folding it into "below" would hide precisely the flow
	// the control is meant to catch.
	PostureUnvaluable Posture = "unvaluable"
)

// Signatures counts how many people signed an admitted order. A closed set of
// two, matching kanz_datamaster_overrides_total{signatures}.
const (
	SingleSigned = "single_signed"
	DualSigned   = "dual_signed"
)

// Decision is what the gate concluded about one order.
type Decision struct {
	// Posture is how the order stood against the threshold.
	Posture Posture
	// Required reports whether a second signature is mandatory. It can only be
	// true when the control is armed, which NewGate refuses today.
	Required bool
	// Act is the dual-control act this order would fall under. Carried so a
	// placement, and the log line below, name it from one place.
	Act dualcontrol.Act
	// Digest is the value an approval would have to cover, computed only for an
	// order at or above the threshold — a sha256 per admission for the whole book
	// buys nothing while the control is absent. Empty when not computed, and
	// empty when the terms could not be digested (DigestErr says why).
	Digest string
	// DigestErr is why the digest could not be computed. It is never a reason to
	// admit quietly: an order whose terms cannot be signed for is an order no
	// approval could ever cover.
	DigestErr error
}

// Decide classifies one order.
//
// A CHILD OF A SCHEDULED PARENT MUST NOT REACH HERE, and the caller enforces it.
// The parent was decided ONCE for the whole notional and its children are
// fractions of a quantity that decision already covered — the same rule the
// compliance gate follows (service.go), and for the same two reasons: N children
// each measured on their own never test the sum, and refusing a late child
// strands the parent part-filled.
func (g *Gate) Decide(cmd *orderpb.SubmitOrder) Decision {
	d := Decision{Act: dualcontrol.ActOrderSubmission, Posture: PostureAbsent}
	if g == nil || g.threshold == nil {
		return d
	}
	notional, ok := g.notional(cmd)
	if !ok {
		d.Posture = PostureUnvaluable
		// UNVALUABLE IS TREATED AS REQUIRING DUAL CONTROL WHEN ARMED. An order
		// the platform could not size must not slip under a threshold it was
		// never compared against; the safe direction on a control is more
		// signatures, not fewer.
		d.Required = g.require
		return d
	}
	if notional.Cmp(g.threshold) < 0 {
		d.Posture = PostureBelow
		return d
	}
	d.Posture = PostureAtOrAbove
	d.Required = g.require
	d.Digest, d.DigestErr = TermsOfSubmit(cmd).Digest()
	return d
}

// notional values the order at quantity x price. ok=false means it could not be
// valued at all, which is never a zero.
//
// THE PRICE COMES FROM compliance.OrderPrice, WHICH IS THE POINT. "What price
// does this order carry" already has exactly one answer in this service, and a
// threshold that valued an order differently from the pre-trade gate would mean
// an operator setting one number got two behaviours from it. A LIMIT order is
// valued at its own limit — the price the fund committed to — and a MARKET or
// STOP order at the reference mark, or not at all.
func (g *Gate) notional(cmd *orderpb.SubmitOrder) (*big.Rat, bool) {
	price := compliance.OrderPrice(cmd, g.marks)
	if price == nil {
		return nil, false
	}
	qty := cmd.GetQuantity()
	if qty == nil {
		return nil, false
	}
	q, ok := dec.FromProtoChecked(qty)
	if !ok {
		return nil, false
	}
	p, ok := dec.FromProtoChecked(price)
	if !ok {
		return nil, false
	}
	// Signed quantity is irrelevant to size: a sell of 10 is as large a decision
	// as a buy of 10. Abs, so a negative coefficient cannot make an order small.
	return new(big.Rat).Abs(new(big.Rat).Mul(q, p)), true
}

// Count records an admitted order's posture and how many people signed it.
//
// CALLED AFTER ADMISSION SUCCEEDS, never before. Counting at the gate would
// include orders that were then refused for compliance, an unroutable venue or
// an unsupported order type — which never "went through" at all, single-signed
// or otherwise, and would inflate the exact number the arming decision rests on.
func (g *Gate) Count(d Decision, signatures string) {
	if g == nil || g.metrics == nil {
		return
	}
	g.metrics.signatures.WithLabelValues(signatures, string(d.Posture)).Inc()
}

// Armed reports whether a second signature is mandatory anywhere. Today always
// false — see NewGate.
func (g *Gate) Armed() bool { return g != nil && g.require }

// Watching reports whether a threshold is configured at all. False means the
// control is ABSENT, which is a real answer and not a failure.
func (g *Gate) Watching() bool { return g != nil && g.threshold != nil }

// Threshold is the configured notional, or nil. For the startup log line, so the
// posture an operator reads comes from the gate itself rather than from a second
// reading of the config.
func (g *Gate) Threshold() *big.Rat {
	if g == nil {
		return nil
	}
	return g.threshold
}

type metrics struct {
	signatures *prometheus.CounterVec
}

func newMetrics(reg prometheus.Registerer) *metrics {
	if reg == nil {
		// nil is the TEST default and never the production one, matching the
		// datamaster override metrics: a control whose correctness depended on
		// the observability wiring being present would fail closed in the wrong
		// place.
		return nil
	}
	signatures := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kanz_oms_order_signatures_total",
		Help: "Orders ADMITTED, by how many people signed them and how they stood against " +
			"OMS_DUAL_CONTROL_MIN_NOTIONAL. single_signed is the #410 gap: one person committed the " +
			"fund's capital on their own authority. It is not an error while the control is unarmed — " +
			"it is the count that says how much of the order flow a second person never saw, and " +
			"{signatures=\"single_signed\",posture=\"at_or_above_threshold\"} is the number the arming " +
			"decision rests on. posture=\"absent\" means no threshold is configured, which is NOT the " +
			"same as below one; posture=\"unvaluable\" means a threshold is set and the order could not " +
			"be valued against it, so that flow is uncounted rather than small. " +
			"COUNTS ORDERS, AND ONE DECISION IS NOW N ORDERS (#435): an order worked as a schedule " +
			"appears as a resting parent plus one child per slice, and only the PARENT is counted here " +
			"— the children inherit its decision.",
	}, []string{"signatures", "posture"})
	reg.MustRegister(signatures)
	// EVERY SERIES EXISTS FROM THE FIRST SCRAPE, including the zeroes. A counter
	// that appears only once it fires makes "no large order has ever been
	// admitted" and "this build does not have the gate" identical on a dashboard,
	// and the second is the one worth knowing.
	for _, s := range []string{SingleSigned, DualSigned} {
		for _, p := range []Posture{PostureAbsent, PostureBelow, PostureAtOrAbove, PostureUnvaluable} {
			signatures.WithLabelValues(s, string(p))
		}
	}
	return &metrics{signatures: signatures}
}
