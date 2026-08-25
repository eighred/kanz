package execution

import (
	"errors"
	"fmt"
	"strings"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

// ErrNoVenue is returned when the router has NO venue at all. An OMS with no
// execution wired is a deliberate configuration (a paper/observation deployment),
// and its orders rest.
var ErrNoVenue = errors.New("execution: no venue available")

// ErrUnpriced is returned when a venue cannot resolve an execution price for an
// order and never will — it has no price source for that order type. It is a
// PERMANENT condition, deliberately distinct from a transient venue fault: a
// caller must REFUSE the order (terminal) rather than return the error for
// retry, because every retry resolves the same way. The order used to rest
// silently instead, forever, looking exactly like a working limit order.
var ErrUnpriced = errors.New("execution: no price source for this order type")

// ErrVenueNotConfigured is returned when an order NAMES a venue this OMS has no
// adapter for. It is deliberately NOT wrapped in ErrNoVenue, because the two are
// opposite situations and the caller must not confuse them:
//
//   - ErrNoVenue          — nothing is wired; resting the order is correct.
//   - ErrVenueNotConfigured — the order asked for a specific venue by name and
//     this OMS has no way to reach it. It can never be executed: not now, not on a
//     retry, not ever. Resting it tells the strategy its order is working while
//     nothing on this platform will ever send it anywhere.
//
// It used to wrap ErrNoVenue, so `errors.Is(err, ErrNoVenue)` was true for both and
// the OMS rested an order it could never fill (EXEC-M8).
var ErrVenueNotConfigured = errors.New("execution: target venue is not configured")

// ErrNoDefaultVenue is returned when an order names NO venue, this router holds
// more than one, and no default has been declared (#437).
//
// IT USED TO PICK venues[0]. The type called itself a "smart-order-router" and
// its fallback "first configured / best venue" — two different things, and only
// the first was implemented. An untargeted order went wherever the config
// happened to list first, chosen by nobody, and the name said it had been ranked.
//
// Refusing is not a regression from that: an order routed to an arbitrary venue
// is worse than one refused with a reason, which is the ruling ErrVenueNotConfigured
// already encodes one case over. The order is refused at admission with a message
// naming every candidate, so the operator picks — once, in config — rather than
// the slice order picking silently on every order.
var ErrNoDefaultVenue = errors.New("execution: order names no venue and no default venue is configured")

// Router dispatches an order to the venue adapter that will work it (M4).
//
// A TARGETED ORDER IS A LOOKUP, NOT A RANKING, and that is most orders. When an
// order carries a target venue (OrderState.venue, stamped by the allocation
// fan-out) this sends it to the venue whose MIC matches — and, when the order
// names an exchange account, to the adapter holding that account. That is an
// allocation matrix, and no ranking may touch it: the fan-out already decided
// where that leg belongs, and second-guessing it would move a portfolio'"'"'s order
// onto another portfolio'"'"'s collateral.
//
// AN UNTARGETED ORDER IS RANKED, on measured cost (#437 B, on #436'"'"'s signal).
// Among the venues this router holds, the one whose realized implementation
// shortfall has been lowest wins — provided every candidate has enough recent
// evidence to be compared. Otherwise the ranker abstains and the DECLARED
// default stands.
//
// So the order of preference is: the fan-out'"'"'s explicit target, then measured
// cost, then the operator'"'"'s named default, then a refusal listing the
// candidates. What it is never again is venues[0] — the array index that made
// this issue.
type Router struct {
	venues []Venue
	// ranker orders venues by MEASURED cost when an order names none (#437 B).
	// Nil, or abstaining, leaves the declared default in charge — see Route.
	ranker CostRanker
	// defaultMIC is the venue an untargeted order goes to, named explicitly.
	// Empty with one venue means that venue; empty with several means refuse.
	defaultMIC string
}

// CostRanker orders already-acceptable venues by what they have actually cost.
//
// A PREFERENCE, NEVER A PERMISSION. It is consulted only after every correctness
// check has passed, and its answer cannot admit a venue they refused or refuse
// one they allowed. ok=false is an ABSTENTION — "use your declared default" —
// not a refusal, and it is the common case before enough fills exist to rank on.
type CostRanker interface {
	Preferred(candidates []string) (string, bool)
}

// RouterOption configures a Router.
type RouterOption func(*Router)

// WithCostRanker supplies the measured-cost ranking for untargeted orders
// (#437 B). Absent, an untargeted order goes to the declared default exactly as
// before — which is the behaviour every deployment has today.
func WithCostRanker(r CostRanker) RouterOption {
	return func(rt *Router) { rt.ranker = r }
}

// WithDefaultVenue names the venue an order carrying no target is worked at.
//
// A NAME, NOT A POSITION. Passing a MIC states the choice where a reader and a
// reviewer can see it; relying on argument order states it nowhere, and reorders
// the trading destination of every untargeted order the day somebody sorts the
// list or adds an adapter above it.
func WithDefaultVenue(mic string) RouterOption {
	return func(r *Router) { r.defaultMIC = mic }
}

// NewRouter builds a router over the given venues.
func NewRouter(venues []Venue, opts ...RouterOption) *Router {
	r := &Router{venues: venues}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// DefaultVenue reports the venue an untargeted order would reach, and whether
// one is resolvable at all. The composition root logs it at startup so the
// destination is visible before an order finds it rather than after.
func (r *Router) DefaultVenue() (Venue, bool) {
	switch {
	case len(r.venues) == 0:
		return nil, false
	case r.defaultMIC != "":
		for _, v := range r.venues {
			if v.MIC() == r.defaultMIC {
				return v, true
			}
		}
		return nil, false
	case len(r.venues) == 1:
		return r.venues[0], true
	default:
		return nil, false
	}
}

// Route selects the venue to work st. A non-empty OrderState.venue routes to
// that specific venue by MIC — the allocation-matrix path, which is the one every
// order from the fan-out producers takes. Empty goes to the DECLARED default, and
// is refused when several venues are configured and nobody declared one (#437).
//
// THE ACCOUNT IS PART OF THE ROUTE, AND IT IS THE PART THAT SPENDS THE MONEY.
//
// A venue (MIC) can hold many exchange accounts, and each adapter deployment holds
// exactly one credential — so an adapter IS an account. Routing on the MIC alone means
// every portfolio's orders reach whichever adapter happens to be configured for that
// venue, and they all margin against ITS collateral. That is cross-collateralization,
// and it happens in the router, silently, one line above the exchange.
//
// So when an order names an account (the OMS stamps it at admission from the
// portfolio's binding), the route must match BOTH: the venue AND the account. An order
// for Basket Alpha cannot reach Basket Beta's credential, because no venue in this
// router holds both.
func (r *Router) Route(st *orderpb.OrderState) (Venue, error) {
	if len(r.venues) == 0 {
		return nil, ErrNoVenue
	}
	target, account := st.GetVenue(), st.GetVenueAccountId()
	if target != "" {
		for _, v := range r.venues {
			if v.MIC() != target {
				continue
			}
			if account != "" && v.Account() != account {
				continue // right venue, WRONG COLLATERAL — keep looking
			}
			return v, nil
		}
		if account != "" {
			return nil, fmt.Errorf("%w: no adapter at %q holds account %q", ErrVenueNotConfigured, target, account)
		}
		return nil, fmt.Errorf("%w: %q", ErrVenueNotConfigured, target)
	}
	// RANK BEFORE FALLING BACK, AND ONLY AMONG VENUES ALREADY DEEMED ACCEPTABLE
	// (#437 B).
	//
	// Every venue here has passed the same checks a targeted order's would: this
	// router holds it, and — since an untargeted order names no account — the
	// account question does not arise. The ranker reorders; it cannot widen the
	// set. So the worst outcome is preferring the wrong one of two acceptable
	// venues, which is exactly what the configured default already risks, better
	// informed.
	//
	// AN ABSTENTION FALLS BACK TO THE DECLARED DEFAULT, never to venues[0]. That
	// ordering is the whole point of option A: this issue exists because a
	// destination chosen by slice position looked like a decision, and a ranker
	// with no data must not reintroduce it.
	// A VENUE THAT CANNOT PLACE THIS ORDER'S TYPE IS NOT A CANDIDATE (#405).
	//
	// Admission refuses a TARGETED order whose named venue cannot place its type.
	// An untargeted one had no such check: the destination was chosen by cost or
	// by the declared default, neither of which asks whether the order can be
	// placed there at all. A stop-loss in a two-venue deployment would be admitted,
	// stored, announced — and then routed, half the time, to the adapter that
	// refuses it. That is precisely the defect #405 exists to end, reached by the
	// one path its admission gate does not cover.
	//
	// Narrowing here rather than refusing: if ONE venue can place it, the order
	// belongs there, and that is a better answer than a refusal.
	placeable := r.placeable(st.GetOrderType(), st.GetTimeInForce())
	if len(placeable) == 0 {
		return nil, fmt.Errorf("%w: no venue this OMS holds can place a %s order with %s (it holds %s)",
			ErrVenueNotConfigured, st.GetOrderType(), st.GetTimeInForce(), strings.Join(r.mics(), ", "))
	}
	if r.ranker != nil {
		if mic, ok := r.ranker.Preferred(placeable); ok {
			for _, v := range r.venues {
				if v.MIC() == mic {
					return v, nil
				}
			}
			// The ranker named a venue this router does not hold. Nothing to do
			// but ignore it — the fallback below is still correct, and refusing
			// here would let a stale ranking take down untargeted routing.
		}
	}
	v, ok := r.DefaultVenue()
	if !ok {
		if r.defaultMIC != "" {
			return nil, fmt.Errorf("%w: %q is configured as the default but no adapter holds it",
				ErrVenueNotConfigured, r.defaultMIC)
		}
		return nil, fmt.Errorf("%w: this OMS holds %s. Name one so an untargeted order has a "+
			"destination somebody chose", ErrNoDefaultVenue, strings.Join(r.mics(), ", "))
	}
	if r.SupportsOrderType(v.MIC(), st.GetOrderType()) &&
		r.SupportsTimeInForce(v.MIC(), st.GetTimeInForce()) {
		return v, nil
	}
	// THE DECLARED DEFAULT CANNOT PLACE THIS TYPE, and exactly one venue can, so
	// there is no choice left to make and no reason to refuse.
	if len(placeable) == 1 {
		for _, cand := range r.venues {
			if cand.MIC() == placeable[0] {
				return cand, nil
			}
		}
	}
	// SEVERAL COULD, AND NOBODY SAID WHICH. Picking one would be the array index
	// #437 removed wearing a narrower disguise — the operator named a default that
	// does not apply here, so the destination is genuinely unchosen.
	return nil, fmt.Errorf("%w: the declared default %q cannot place a %s order with %s, and %s "+
		"all can — name the venue on the order, or make the default one that can",
		ErrNoDefaultVenue, v.MIC(), st.GetOrderType(), st.GetTimeInForce(), strings.Join(placeable, ", "))
}

// placeable lists the MICs of venues that can place this order type, in
// configuration order. A venue that declares nothing is included: "did not say"
// is permissive here exactly as it is in SupportsOrderType, so a deployment whose
// adapters predate the declaration keeps working.
func (r *Router) placeable(t orderpb.OrderType, tif orderpb.TimeInForce) []string {
	out := make([]string, 0, len(r.venues))
	for _, v := range r.venues {
		if r.SupportsOrderType(v.MIC(), t) && r.SupportsTimeInForce(v.MIC(), tif) {
			out = append(out, v.MIC())
		}
	}
	return out
}

// mics lists the configured venue MICs, for a refusal that names the candidates
// rather than leaving the operator to find them.
func (r *Router) mics() []string {
	out := make([]string, 0, len(r.venues))
	for _, v := range r.venues {
		out = append(out, v.MIC())
	}
	return out
}

// Supports reports whether an adapter for this MIC — and, when account is non-empty,
// for that specific exchange account — is configured. The OMS checks it at ADMISSION,
// so an order naming a venue that does not exist is refused before it is ever admitted
// — the same treatment a compliance breach or a malformed order gets, and for the same
// reason: it can never be executed.
//
// An empty account asks only "can this OMS reach that venue at all".
func (r *Router) Supports(mic, account string) bool {
	for _, v := range r.venues {
		if v.MIC() != mic {
			continue
		}
		if account != "" && v.Account() != account {
			continue
		}
		return true
	}
	return false
}

// OrderTypeAware is implemented by a Venue that has declared which order types
// its adapter can place. A Venue that does not implement it has said nothing,
// which is not the same as saying "none" — see Router.SupportsOrderType.
type OrderTypeAware interface {
	SupportsOrderType(t orderpb.OrderType) bool
}

// WithOrderTypes returns v carrying the order types its adapter declared (#405).
//
// A WRAPPER RATHER THAN A FIELD ON GRPCVenue, deliberately. The capability is
// learned once, at startup, from Describe — and a mutable field set after
// construction is a field something can read before it is written. Wrapping
// makes the venue immutable again: the value that reaches the Router already
// knows what it can do, and there is no window in which it does not.
//
// Empty types returns v unchanged, so an adapter that did not answer stays
// exactly what it was rather than becoming a wrapper that claims nothing.
func WithOrderTypes(v Venue, types []orderpb.OrderType) Venue {
	if len(types) == 0 {
		return v
	}
	return orderTypeAware{Venue: v, types: types}
}

type orderTypeAware struct {
	Venue
	types []orderpb.OrderType
}

func (o orderTypeAware) SupportsOrderType(t orderpb.OrderType) bool {
	return ContainsOrderType(o.types, t)
}

// OrderTypeDeclarer is implemented by a connector that states which order types
// it can translate for its exchange.
//
// THE DECLARATION AND THE TRANSLATION MUST NOT DRIFT, and nothing in the type
// system can hold them together: the truth is a switch inside Place, and this is
// a list beside it. Each connector carries a test that walks every value of
// order.v1.OrderType and asserts translation succeeds for exactly the declared
// ones — so a type added to the switch without the list, or promised in the list
// without the switch, fails there rather than at a venue.
type OrderTypeDeclarer interface {
	OrderTypes() []orderpb.OrderType
}

// ContainsOrderType reports whether types names t. One membership test, used by
// the adapter that declares, the identity that carries and the router that gates.
func ContainsOrderType(types []orderpb.OrderType, t orderpb.OrderType) bool {
	for _, got := range types {
		if got == t {
			return true
		}
	}
	return false
}

// SupportsOrderType reports whether the adapter at this MIC can place t.
//
// THE OMS CHECKS IT AT ADMISSION, for the reason Supports above gives: an order
// that can never be executed must be refused before it is admitted, not after.
// A stop-loss used to pass admission, be stored, and have its ORDER_ACCEPTED
// FACT published — and only then be refused inside Execute by an adapter that
// implements MARKET and LIMIT only. Risk carried exposure for it and the caller
// had been told it was working, for the one order type whose entire purpose is
// to act when nobody is watching (#405).
//
// TRUE WHEN NOBODY SAID OTHERWISE, in two distinct cases, and both are handled
// elsewhere rather than conflated here:
//
//   - no adapter at this MIC — Supports already refuses that, and returning
//     false here as well would report one misconfiguration as two;
//   - an adapter that declared nothing — it predates venue.v1's
//     supported_order_types, and refusing every order for it would turn a schema
//     addition into a trading outage. The OMS names it, counts it, and refuses
//     only under OMS_REQUIRE_ORDER_TYPE_SUPPORT.
func (r *Router) SupportsOrderType(mic string, t orderpb.OrderType) bool {
	for _, v := range r.venues {
		if v.MIC() != mic {
			continue
		}
		if aware, ok := v.(OrderTypeAware); ok {
			return aware.SupportsOrderType(t)
		}
		return true
	}
	return true
}

// AccountFor returns the account the configured adapter at this MIC actually trades.
//
// It is what the OMS stamps when no binding governs the portfolio: the account the
// order WILL hit is a fact whether or not anybody decided it should, and recording the
// real one is what makes shared collateral visible in the ledger instead of hidden by
// it.
func (r *Router) AccountFor(mic string) (string, bool) {
	for _, v := range r.venues {
		if v.MIC() == mic {
			return v.Account(), true
		}
	}
	return "", false
}

// ===== TIME-IN-FORCE, THE SAME SHAPE AS ORDER TYPE (#486) =====
//
// Deliberately a mirror of OrderTypeAware / OrderTypeDeclarer / SupportsOrderType
// above rather than a second design. The question is identical — "can this
// adapter express what the order asks for" — and the two gates sit one line
// apart in admission; a reader who has understood one has understood both.
//
// WHY IT NEEDED ITS OWN GATE AT ALL. time-in-force was the fourth instance of
// #240/#405's defect family and the worst of them: the earlier three produced an
// order that did NOTHING, this one produced an order that did the WRONG THING.
// Both spot connectors sent a hard-coded good-til-cancelled whatever the trader
// asked for, so an IOC RESTED at the exchange and a FOK could rest partially
// filled. They now refuse what they cannot express; this moves that refusal to
// admission, before the order is stored and announced.

// TimeInForceAware is implemented by a Venue that has declared which
// time-in-force instructions its adapter can express. A Venue that does not
// implement it has said nothing, which is not the same as saying "none".
type TimeInForceAware interface {
	SupportsTimeInForce(t orderpb.TimeInForce) bool
}

// WithTimeInForce returns v carrying the time-in-force set its adapter declared.
//
// A WRAPPER RATHER THAN A FIELD, for the reason WithOrderTypes gives: the
// capability is learned once, at startup, from Describe, and a mutable field set
// after construction is a field something can read before it is written.
//
// Empty returns v unchanged, so an adapter that did not answer stays exactly
// what it was rather than becoming a wrapper that claims nothing.
func WithTimeInForce(v Venue, tifs []orderpb.TimeInForce) Venue {
	if len(tifs) == 0 {
		return v
	}
	return timeInForceAware{Venue: v, tifs: tifs}
}

type timeInForceAware struct {
	Venue
	tifs []orderpb.TimeInForce
}

func (t timeInForceAware) SupportsTimeInForce(tif orderpb.TimeInForce) bool {
	return ContainsTimeInForce(t.tifs, tif)
}

// TimeInForceDeclarer is implemented by a connector that states which
// time-in-force instructions it can translate for its exchange.
//
// THE DECLARATION AND THE TRANSLATION MUST NOT DRIFT. Each connector carries a
// test walking every value of order.v1.TimeInForce, asserting translation
// succeeds for exactly the declared ones — so an instruction added to the switch
// without the list, or promised in the list without the switch, fails there
// rather than at a venue.
type TimeInForceDeclarer interface {
	TimeInForce() []orderpb.TimeInForce
}

// ContainsTimeInForce reports whether tifs names t. One membership test, used by
// the adapter that declares, the identity that carries and the router that gates.
func ContainsTimeInForce(tifs []orderpb.TimeInForce, t orderpb.TimeInForce) bool {
	for _, got := range tifs {
		if got == t {
			return true
		}
	}
	return false
}

// SupportsTimeInForce reports whether the adapter at mic said it can express t.
//
// UNKNOWN IS PERMISSIVE, exactly as it is for order types: a venue that declares
// nothing has not answered the question, and refusing every order for it would
// be a trading outage caused by a schema addition.
func (r *Router) SupportsTimeInForce(mic string, t orderpb.TimeInForce) bool {
	for _, v := range r.venues {
		if v.MIC() != mic {
			continue
		}
		if aware, ok := v.(TimeInForceAware); ok {
			return aware.SupportsTimeInForce(t)
		}
		return true
	}
	return true
}

// MarginModeAware is implemented by a Venue that has declared which collateral
// regimes its adapter can express. A Venue that does not implement it has said
// nothing, which is not the same as saying "none".
type MarginModeAware interface {
	SupportsMarginMode(m orderpb.MarginMode) bool
}

// WithMarginModes returns v carrying the margin-mode set its adapter declared
// (#417).
//
// THE FIFTH INSTANCE OF ONE DEFECT FAMILY, and the only one that never reached a
// venue. #240 (leverage), #405 (stop_price, order types) and #486 (time_in_force)
// all shipped: a term was accepted at the perimeter and dropped before the wire,
// so the order did nothing, or did the wrong thing. Leverage was stopped earlier
// than that — internal/signal/translate refused it outright, because SubmitOrder
// had nowhere to carry it — and that refusal was correct and could only ever say
// no. It could not say "OKX cannot express this, another venue can", because
// nothing described what an adapter can do.
//
// This is what makes the answer a capability contract instead of a hardcode.
//
// A WRAPPER RATHER THAN A FIELD, for the reason WithOrderTypes gives: the
// capability is learned once, at startup, from Describe, and a mutable field set
// after construction is a field something can read before it is written.
//
// EMPTY RETURNS v UNCHANGED, which is the load-bearing half. An adapter
// predating this field answers with nothing, and reading that as "supports no
// margin mode" would refuse every order it can already place — turning a schema
// addition into a trading outage. Unlevered spot stays admissible everywhere.
func WithMarginModes(v Venue, modes []orderpb.MarginMode) Venue {
	if len(modes) == 0 {
		return v
	}
	return marginModeAware{Venue: v, modes: modes}
}

type marginModeAware struct {
	Venue
	modes []orderpb.MarginMode
}

func (m marginModeAware) SupportsMarginMode(mode orderpb.MarginMode) bool {
	return ContainsMarginMode(m.modes, mode)
}

// MarginModeDeclarer is implemented by a connector that states which collateral
// regimes it can translate for its exchange.
//
// THE DECLARATION AND THE TRANSLATION MUST NOT DRIFT, and nothing in the type
// system can hold them together: the truth is what the connector puts on the
// wire — `tdMode` at OKX, the endpoint family at Binance — and this is a list
// beside it. Each connector carries a test walking every value of
// order.v1.MarginMode and asserting translation succeeds for exactly the
// declared ones, so a regime added to one without the other fails there rather
// than at a venue.
type MarginModeDeclarer interface {
	MarginModes() []orderpb.MarginMode
}

// ContainsMarginMode reports whether modes names m. One membership test, used by
// the adapter that declares, the identity that carries and the router that gates.
func ContainsMarginMode(modes []orderpb.MarginMode, m orderpb.MarginMode) bool {
	for _, got := range modes {
		if got == m {
			return true
		}
	}
	return false
}

// SupportsMarginMode reports whether the adapter at mic said it can work an
// order under m.
//
// UNKNOWN IS PERMISSIVE, exactly as it is for order types and time-in-force: a
// venue this router does not hold, or one that declared nothing, returns true.
// The gate this feeds refuses an order the venue CANNOT place; it is not a
// second entitlement check, and reading silence as refusal would take the estate
// down on a schema addition.
func (r *Router) SupportsMarginMode(mic string, m orderpb.MarginMode) bool {
	for _, v := range r.venues {
		if v.MIC() != mic {
			continue
		}
		if aware, ok := v.(MarginModeAware); ok {
			return aware.SupportsMarginMode(m)
		}
		return true
	}
	return true
}
