package venuemargin

import (
	"context"
	"math/big"
	"sort"
	"sync"
	"time"

	collateralpb "github.com/eighred/kanz/kanz-schemas-go/collateral/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
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

// Coverage is what an observation said about HOW MUCH of the venue's margin
// state it actually carries — the difference between "the exchange answered
// everything we asked" and "we do not know what it left out".
//
// # Why this is a type and not a uint32
//
// domain.v1.InputCoverage's own contract is that PRESENCE IS THE SIGNAL: absent
// means "this publisher does not report coverage", present with
// excluded_count = 0 means "it reports, and everything applicable was covered".
// A bare count renders both as zero and re-creates the conflation the record
// exists to break — and the direction of that mistake is the wrong one, because
// the publisher that says nothing is the one nobody has checked.
//
// The zero Coverage is therefore NOT "fully covered". It is what a never-covered
// lookup and a coverage-less publisher both yield, and Complete is false on it.
type Coverage struct {
	reported bool
	excluded uint32
}

// Reported says whether the observation carried a coverage record at all. False
// means the publisher does not report coverage — not that it covered everything.
func (c Coverage) Reported() bool { return c.reported }

// ExcludedCount is how many quantities the venue could not answer for. It is
// meaningful only when Reported is true; on an unreported coverage it is zero
// for the reason a count is always zero when nobody counted.
func (c Coverage) ExcludedCount() uint32 { return c.excluded }

// Complete reports whether the venue answered EVERYTHING asked of it: coverage
// was reported AND it excluded nothing.
//
// A GATE MUST USE THIS RATHER THAN ExcludedCount() == 0. The two differ exactly
// on the publisher that reports no coverage, and that is the case a margin
// control must refuse: an observation that does not say what it left out cannot
// be shown to have left out nothing.
func (c Coverage) Complete() bool { return c.reported && c.excluded == 0 }

// snapshot is one venue account's last observation.
type snapshot struct {
	maintenanceMargin *big.Rat
	marginRatio       *big.Rat
	liquidation       map[string]openPosition
	observedAt        time.Time
	coverage          Coverage
}

// openPosition is one reported open position: the exchange's own symbol is the
// key, and the instrument it was attributed to travels WITH the price.
//
// ATTRIBUTION IS PART OF THE OBSERVATION, NOT A LOOKUP EACH READER REPEATS. The
// venue adapter is the one process holding both the exchange's symbols and the
// instrument -> symbol table they came from, so it is the only place the
// inversion is even defined (execution.InstrumentFor, #707/#708). A reader that
// re-derived it would need a copy of that adapter's configuration — the second
// answer #408 control 4's exemption exists to refuse.
//
// instrument is EMPTY when the adapter could not attribute the symbol, and that
// is never "this position has no instrument". The position is still held: a
// leveraged position this deployment cannot name is precisely the one an
// operator must see, and the observation's Coverage carries the reason.
type openPosition struct {
	instrument string
	price      *big.Rat
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
	if !validFieldSupport(msg.GetMaintenanceMargin(), msg.GetMaintenanceMarginSupport()) ||
		!validFieldSupport(msg.GetMarginRatio(), msg.GetMarginRatioSupport()) {
		// Invalid enum values and UNSUPPORTED fields carrying numeric claims are
		// contradictory critical state. Drop the whole level rather than fold a
		// partial answer under one fresh observation timestamp.
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
		liquidation: map[string]openPosition{},
		// PRESENCE, NOT JUST THE COUNT. msg.GetCoverage() is nil for a publisher
		// that reports no coverage, and GetExcludedCount() on it is zero — the same
		// zero a publisher that covered everything produces. Recording the presence
		// separately is what lets a gate refuse the first and admit the second.
		coverage: Coverage{
			reported: msg.GetCoverage() != nil,
			excluded: msg.GetCoverage().GetExcludedCount(),
		},
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
		snap.liquidation[lp.GetVenueSymbol()] = openPosition{
			instrument: lp.GetInstrumentId(),
			price:      dec.FromProto(lp.GetPrice()),
		}
	}

	v.mu.Lock()
	v.byAcct[key] = snap
	v.mu.Unlock()

	if snap.coverage.excluded > 0 && v.onUncovered != nil {
		v.onUncovered(key.venue, key.account, snap.coverage.excluded)
	}
	return nil
}

func validFieldSupport(value *commonpb.Decimal, support collateralpb.SupportStatus) bool {
	switch support {
	case collateralpb.SupportStatus_SUPPORT_STATUS_UNSPECIFIED,
		collateralpb.SupportStatus_SUPPORT_STATUS_SUPPORTED:
		return true
	case collateralpb.SupportStatus_SUPPORT_STATUS_UNSUPPORTED:
		return value == nil
	default:
		return false
	}
}

// Coverage is what this account's last CURRENT observation said about how much
// of the venue's margin state it carries, or ok=false when UNKNOWN — never
// observed, or observed too long ago.
//
// IT IS BOUND BY THE SAME FRESHNESS RULE AS THE QUANTITIES, and that is the
// point of it living here rather than being remembered by a caller off the fold
// callback. A coverage record from a stale observation describes an observation
// nothing may act on; answering with it would let a gate satisfy its
// completeness check from one poll and its numbers from another.
func (v *View) Coverage(venue, account string) (Coverage, bool) {
	snap, ok := v.currentSnapshot(venue, account)
	if !ok {
		return Coverage{}, false
	}
	return snap.coverage, true
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
	return v.lookup(venue, account, func(s snapshot) *big.Rat { return s.liquidation[venueSymbol].price })
}

func (v *View) lookup(venue, account string, pick func(snapshot) *big.Rat) (Quantity, bool) {
	snap, ok := v.currentSnapshot(venue, account)
	if !ok {
		return Quantity{}, false
	}
	val := pick(snap)
	if val == nil {
		return Quantity{}, false
	}
	return Quantity{value: new(big.Rat).Set(val), observedAt: snap.observedAt}, true
}

// currentSnapshot returns this account's folded state if it has been observed
// AND that observation is still current.
//
// THE FRESHNESS RULE LIVES HERE, ONCE, because every reader of the fold must
// apply the same one. Coverage, the quantity lookups and Liquidations each used
// to carry their own copy of it; a reader that skipped it — or that drifted by
// one comparison — would answer from an observation the rest of the package
// treats as UNKNOWN, and the disagreement surfaces as a margin control passing
// on a feed that has stopped. That is the failure this package exists to make
// impossible, so it must not depend on each reader remembering.
//
// The stale hook fires from here for the same reason: an operator watching it
// sees one event per read of a dead account, whichever reader asked.
func (v *View) currentSnapshot(venue, account string) (snapshot, bool) {
	v.mu.RLock()
	snap, seen := v.byAcct[accountKey{venue: venue, account: account}]
	v.mu.RUnlock()
	if !seen {
		return snapshot{}, false
	}
	if v.maxAge > 0 {
		if age := v.now().UTC().Sub(snap.observedAt); age > v.maxAge {
			if v.onStale != nil {
				v.onStale(venue, account, age)
			}
			return snapshot{}, false
		}
	}
	return snap, true
}

// Liquidation is one open leveraged position from a venue's margin observation.
type Liquidation struct {
	// VenueSymbol is the exchange's own spelling, always present — it is the key
	// the venue reported the position under.
	VenueSymbol string

	// InstrumentID is the Kanz instrument the ADAPTER attributed the symbol to,
	// or empty when it could not. Empty is "unattributed", never "no instrument":
	// a consumer that must pair this price with a mark cannot use the position,
	// and the account's Coverage says why it is missing.
	InstrumentID string

	// Price is the venue's liquidation price for the position, in the
	// instrument's quote currency.
	Price Quantity
}

// Liquidations returns every open position in this account's last CURRENT
// observation, or ok=false when the account's margin state is UNKNOWN.
//
// # Why an enumerator, when LiquidationPrice already answers per symbol
//
// The two have opposite shapes. LiquidationPrice serves a caller that ALREADY
// HOLDS the symbol — a compliance rule evaluating one order — and asking it is
// how that rule checks the instrument in front of it. A risk measure starts from
// a PORTFOLIO and has to find every position that could liquidate under it,
// including the ones nobody thought to ask about. Symbol-by-symbol lookup cannot
// enumerate, so such a caller would have to keep its own list of the symbols an
// account holds: a second answer to "what is in this account", maintained by
// someone who is not the exchange, going stale without saying so.
//
// # ok=false is UNKNOWN; an empty slice is a positive statement
//
// This is the distinction the package turns on. ok=false means never observed or
// no longer current, and NOTHING may be concluded — least of all that the
// account is flat. ok=true with no positions is the exchange saying this account
// holds nothing leveraged, on which an exact-zero liquidation proximity is the
// honest answer rather than a flattering one.
//
// # THE COVERAGE COMES BACK WITH THEM, FROM THE SAME OBSERVATION
//
// An enumerator answers "what is in this account", and that answer is only
// meaningful alongside what the exchange could not tell us — a position missing
// from this slice because the venue did not answer for it is invisible in the
// slice itself. Returning the two together is the same discipline Quantity
// applies to a value and its timestamp: the positions are UNREACHABLE without
// the record that says what was left out of them.
//
// It is also the only way to get both from ONE snapshot. Calling Coverage
// separately re-enters currentSnapshot, so an account that ages out between the
// two calls yields positions from a current observation and no coverage — or, on
// a fold that lands in between, a completeness record describing a different
// observation than the positions came from. The OMS's adapter states the same
// rule for the ratio ("a completeness check satisfied by one observation and a
// number taken from another would be the stale book with an extra step"); this
// makes it structural for the enumerating caller rather than a thing to
// remember.
//
// # What the caller gets
//
// The slice is freshly built and each Price is a copy, so a caller cannot reach
// back into the fold — the same guarantee the quantity lookups give, and it
// matters more here because a slice looks borrowable. It is ordered by venue
// symbol so that two reads of one observation cannot disagree about which
// position is "worst" when two are equally close.
func (v *View) Liquidations(venue, account string) ([]Liquidation, Coverage, bool) {
	snap, ok := v.currentSnapshot(venue, account)
	if !ok {
		return nil, Coverage{}, false
	}
	out := make([]Liquidation, 0, len(snap.liquidation))
	for symbol, pos := range snap.liquidation {
		if pos.price == nil {
			// Defensive: the fold refuses a priceless entry, so this is
			// unreachable today. It stays because the alternative on a future
			// fold that admits one is a Quantity wrapping a nil *big.Rat, which
			// reads as a real liquidation price of zero.
			continue
		}
		out = append(out, Liquidation{
			VenueSymbol:  symbol,
			InstrumentID: pos.instrument,
			Price:        Quantity{value: new(big.Rat).Set(pos.price), observedAt: snap.observedAt},
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].VenueSymbol < out[j].VenueSymbol })
	return out, snap.coverage, true
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

// CoverageStats reports how many accounts have current observations and how
// many of those current observations explicitly carry incomplete coverage.
// Historical gaps are deliberately excluded: incident counters retain that
// history, while this posture answers whether the latest actionable level is
// complete now.
func (v *View) CoverageStats() (current, incomplete int) {
	now := v.now().UTC()
	v.mu.RLock()
	defer v.mu.RUnlock()
	for _, s := range v.byAcct {
		if v.maxAge > 0 && now.Sub(s.observedAt) > v.maxAge {
			continue
		}
		current++
		if !s.coverage.Complete() {
			incomplete++
		}
	}
	return current, incomplete
}

var _ bus.EventHandler = (&View{}).Handle
