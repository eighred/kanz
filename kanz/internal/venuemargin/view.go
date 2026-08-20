package venuemargin

import (
	"context"
	"math/big"
	"sync"
	"time"

	collateralpb "github.com/eighred/kanz/kanz-schemas-go/collateral/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"google.golang.org/protobuf/proto"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/pkg/bus"
)

// Quantity is one margin figure INSEPARABLE FROM WHEN THE VENUE OBSERVED IT.
//
// # Why this is a type and not a *big.Rat plus a second return value
//
// #408's ruling puts staleness on the same footing as the number: "a margin
// ratio from ten minutes ago during a fast move is not a margin ratio". Every
// arrangement that keeps the two apart eventually separates them. A second
// return value is dropped with `_`. A struct with exported fields is copied
// field-by-field into a caller's own type and the timestamp is the field nobody
// carries over. A map of values with a timestamp beside the map is read through
// the map.
//
// So the value is UNREACHABLE except through a value that also carries its
// observation time: the fields are unexported, this package is the only thing
// that can construct one, and there is no accessor that yields the number alone
// without ObservedAt() sitting on the same object. A caller can still ignore the
// age — nothing can stop that — but it cannot do so without having been handed
// it, and that is the difference between a defect and a decision.
//
// The zero Quantity is not a zero margin figure. It is what a failed lookup
// returns beside ok=false, and its Value() is nil.
type Quantity struct {
	value      *big.Rat
	observedAt time.Time
}

// Value is the figure the venue reported. nil on the zero Quantity — which is
// what a lookup returns alongside ok=false, and which cannot be mistaken for a
// measured zero the way a zero big.Rat could.
//
// A COPY, because *big.Rat is mutable and the view hands the same pointer to
// every caller; one caller's Sub would otherwise silently rewrite the fold.
func (q Quantity) Value() *big.Rat {
	if q.value == nil {
		return nil
	}
	return new(big.Rat).Set(q.value)
}

// ObservedAt is when the VENUE's state was true, in UTC — the exchange's own
// stamp where it publishes one. Not when we published, folded or read it.
func (q Quantity) ObservedAt() time.Time { return q.observedAt }

// Age is how stale the figure is at now. Negative for a venue clock ahead of
// ours, which is reported rather than clamped: a systematically negative age is
// clock skew worth seeing, and clamping it to zero would present the most
// suspicious observations as the freshest.
func (q Quantity) Age(now time.Time) time.Duration { return now.UTC().Sub(q.observedAt) }

// snapshot is one venue account's last observation.
type snapshot struct {
	maintenanceMargin *big.Rat
	marginRatio       *big.Rat
	liquidation       map[string]*big.Rat
	observedAt        time.Time
	excluded          uint32
}

// accountKey is the routing dimension. THE ACCOUNT, NOT THE PORTFOLIO: the
// exchange margins and liquidates per account, an adapter's credential IS one
// account, and the portfolio that owns it is OMS deploy-time configuration the
// publisher does not hold. A caller resolves portfolio → account through
// execution.AccountBindings, which is single-valued because ErrAccountShared
// refuses to start the OMS on a shared one (#415).
type accountKey struct{ venue, account string }

// View is the folded venue-margin state, keyed by (venue, account).
//
// UNKNOWN IS THE DEFAULT AND STAYS THE DEFAULT until a venue says otherwise.
// Before the first observation, after one goes stale, and for every quantity the
// exchange declined to report, every lookup answers ok=false. The controls in
// #408's set all fail closed on that, which means a broken margin feed refuses
// orders rather than admitting them — the direction the ruling requires, and the
// opposite of what a zero-valued default would do.
type View struct {
	mu     sync.RWMutex
	byAcct map[accountKey]snapshot

	now    func() time.Time
	maxAge time.Duration

	onStale     func(venue, account string, age time.Duration)
	onUncovered func(venue, account string, excluded uint32)
	onUndated   func(venue, account string)
}

// Option customizes a View.
type Option func(*View)

// WithClock injects the clock (tests).
func WithClock(now func() time.Time) Option {
	return func(v *View) {
		if now != nil {
			v.now = now
		}
	}
}

// WithMaxAge overrides DefaultMaxAge. Non-positive ⇒ no bound, which is only
// ever right in a test — an unbounded margin view is the stale book #408 exists
// to remove.
func WithMaxAge(d time.Duration) Option { return func(v *View) { v.maxAge = d } }

// WithOnStale is called when a lookup finds an observation too old to act on.
func WithOnStale(fn func(venue, account string, age time.Duration)) Option {
	return func(v *View) { v.onStale = fn }
}

// WithOnUncovered is called when a folded observation says the venue did not
// report everything asked of it.
//
// IT FIRES ON THE FOLD, NOT ON THE LOOKUP, the same split riskview makes: the
// announcement is where the fact exists, and by lookup time the quantity is
// simply absent and indistinguishable from one this venue never carries. An
// operator watching a margin control refuse needs to know whether the exchange
// is answering with gaps or the feed has gone quiet — different incidents.
func WithOnUncovered(fn func(venue, account string, excluded uint32)) Option {
	return func(v *View) { v.onUncovered = fn }
}

// WithOnUndated is called when an observation arrives with no observation time
// and is therefore discarded.
func WithOnUndated(fn func(venue, account string)) Option {
	return func(v *View) { v.onUndated = fn }
}

// New returns an empty view.
func New(opts ...Option) *View {
	v := &View{byAcct: map[accountKey]snapshot{}, now: time.Now, maxAge: DefaultMaxAge}
	for _, opt := range opts {
		opt(v)
	}
	return v
}

// Handle folds a collateral.v1.VenueMarginState. It is a bus.EventHandler.
//
// A LEVEL, NOT A DELTA: this REPLACES the account's state. Merging would leave a
// quantity the venue has STOPPED reporting showing its last value forever, and a
// maintenance margin that never ages out is worse than none — the freshness
// bound would never fire on it, because a later observation carrying other
// fields keeps refreshing the timestamp the stale figure is judged against.
func (v *View) Handle(_ context.Context, _ *envelopepb.Envelope, payload []byte) error {
	var msg collateralpb.VenueMarginState
	if proto.Unmarshal(payload, &msg) != nil {
		// A permanent defect in these bytes: nacking replays them forever, while
		// the account simply ages out to UNKNOWN and every control refuses.
		return nil
	}
	// OUT-OF-DOMAIN EXPONENTS ARE REFUSED BEFORE ANY NUMBER IS READ (#95):
	// dec.FromProto materialises 10^abs(exponent).
	if _, in := dec.InDomainDeep(&msg); !in {
		return nil
	}
	key := accountKey{venue: msg.GetVenue(), account: msg.GetVenueAccountId()}
	if key.venue == "" || key.account == "" {
		// Unattributable to a liquidation boundary, so it cannot gate anything.
		return nil
	}
	observedAt := msg.GetObservedAt().AsTime().UTC()
	if observedAt.IsZero() || msg.GetObservedAt() == nil {
		// AN UNDATED MARGIN FIGURE IS NOT A MARGIN FIGURE. Folding it would put a
		// number of unknown age behind a freshness bound that could never judge it
		// — the exact substitution the bound exists to prevent — so it is dropped
		// and the account keeps whatever it had, which will itself age out.
		if v.onUndated != nil {
			v.onUndated(key.venue, key.account)
		}
		return nil
	}

	snap := snapshot{
		observedAt:  observedAt,
		liquidation: map[string]*big.Rat{},
		excluded:    msg.GetCoverage().GetExcludedCount(),
	}
	// PRESENCE IS THE ONLY THING FOLDED. An absent Decimal stays absent: it is
	// never materialised as zero, which is what dec.FromProto would hand back for
	// a nil message and what would then read as "this account needs no
	// collateral" on every dashboard and in every rule.
	if d := msg.GetMaintenanceMargin(); d != nil {
		snap.maintenanceMargin = dec.FromProto(d)
	}
	if d := msg.GetMarginRatio(); d != nil {
		snap.marginRatio = dec.FromProto(d)
	}
	for _, lp := range msg.GetLiquidationPrices() {
		if lp.GetVenueSymbol() == "" || lp.GetPrice() == nil {
			continue
		}
		snap.liquidation[lp.GetVenueSymbol()] = dec.FromProto(lp.GetPrice())
	}

	v.mu.Lock()
	v.byAcct[key] = snap
	v.mu.Unlock()

	if snap.excluded > 0 && v.onUncovered != nil {
		v.onUncovered(key.venue, key.account, snap.excluded)
	}
	return nil
}

// MaintenanceMargin is the collateral the exchange requires this account to
// keep, or ok=false when UNKNOWN.
//
// THE UNKNOWNS ARE ONE ANSWER ON PURPOSE — never observed, observed without this
// field, or observed too long ago. A caller must not be able to treat "this
// venue does not report maintenance margin" differently from "the feed has
// stopped": both mean this view cannot vouch for the number, and a rule that
// distinguished them would eventually pass one of them.
func (v *View) MaintenanceMargin(venue, account string) (Quantity, bool) {
	return v.lookup(venue, account, func(s snapshot) *big.Rat { return s.maintenanceMargin })
}

// MarginRatio is the exchange's own margin ratio for the account, or ok=false
// when UNKNOWN.
//
// IN THE VENUE'S UNITS AND DIRECTION. OKX's mgnRatio rises as the account gets
// safer; other venues' ratios do not agree on the units or on which way danger
// lies, and nothing here normalises them — normalising would mean asserting a
// venue's convention on its behalf. A limit written against this is written per
// venue.
func (v *View) MarginRatio(venue, account string) (Quantity, bool) {
	return v.lookup(venue, account, func(s snapshot) *big.Rat { return s.marginRatio })
}

// LiquidationPrice is the exchange's liquidation price for one open position, by
// the VENUE's own symbol, or ok=false when UNKNOWN.
//
// A position the venue reported without a liquidation price is UNKNOWN here and
// present in the observation's coverage — it is not absent-and-therefore-fine.
func (v *View) LiquidationPrice(venue, account, venueSymbol string) (Quantity, bool) {
	return v.lookup(venue, account, func(s snapshot) *big.Rat { return s.liquidation[venueSymbol] })
}

func (v *View) lookup(venue, account string, pick func(snapshot) *big.Rat) (Quantity, bool) {
	v.mu.RLock()
	snap, seen := v.byAcct[accountKey{venue: venue, account: account}]
	v.mu.RUnlock()
	if !seen {
		return Quantity{}, false
	}
	if v.maxAge > 0 {
		if age := v.now().UTC().Sub(snap.observedAt); age > v.maxAge {
			if v.onStale != nil {
				v.onStale(venue, account, age)
			}
			return Quantity{}, false
		}
	}
	val := pick(snap)
	if val == nil {
		return Quantity{}, false
	}
	return Quantity{value: new(big.Rat).Set(val), observedAt: snap.observedAt}, true
}

// Stats reports how many venue accounts are held and how many are current, for
// the posture gauge.
//
// "HELD BUT NOT CURRENT" IS THE STATE AN OPERATOR NEEDS BEFORE THE REFUSALS
// START. An account whose observations have stopped still appears here; it is
// the gap between the two numbers that says a margin control is about to refuse
// every order on it, and that gap is visible minutes before anyone files a
// ticket about orders being rejected.
func (v *View) Stats() (held, live int) {
	now := v.now().UTC()
	v.mu.RLock()
	defer v.mu.RUnlock()
	for _, s := range v.byAcct {
		held++
		if v.maxAge <= 0 || now.Sub(s.observedAt) <= v.maxAge {
			live++
		}
	}
	return held, live
}

var _ bus.EventHandler = (&View{}).Handle
