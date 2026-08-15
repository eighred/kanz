// Package spotsource resolves an underlying's spot price from the MODEL-01b
// market-data store, satisfying compute.SpotProvider — the seam the Greeks layer
// and the full-revaluation path consume, and which had ZERO production
// implementations (#509). It is the counterpart of internal/risk/termsource: one
// resolves an option's contract terms, this one resolves the underlying it is
// struck on, and RegisterGreeks needs both before it can be wired at all.
package spotsource

import (
	"context"
	"math"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	"github.com/eighred/kanz/internal/marketdata/store"
	"github.com/eighred/kanz/internal/risk/compute"
)

// PriceStore is the one read this provider needs from the market-data store —
// the point-in-time spot read of store.Store (MODEL-01b).
//
// NARROWED ON PURPOSE. *store.Postgres, *store.Memory and store.Store all
// satisfy it, so the composition root passes its real store and a test passes
// three lines of fake without a database — and the provider cannot grow a Put
// or a History call without this interface changing in the diff. It is declared
// at the consumer, which is where Go puts an interface.
type PriceStore interface {
	// LatestAsOf bounds BOTH bitemporal axes at asOf (ObservationTime <= asOf
	// AND KnowledgeTime <= asOf), which is what makes a replayed valuation
	// reproduce what was knowable then. See the store package doc.
	LatestAsOf(ctx context.Context, instrumentID string, kind store.PriceKind, asOf time.Time) (store.Observation, bool, error)
}

// DefaultKind is the mark this provider reads when the deployment does not say.
//
// # Why the kind must be pinned at construction, and what it costs
//
// compute.SpotProvider is Spot(ctx, instrumentID, asOf) — there is no kind
// parameter and no way for a caller to ask for a different mark. So one kind is
// chosen here, once, for every option the book holds.
//
// IF A DEPLOYMENT STORES ITS PRICES UNDER A DIFFERENT KIND, EVERY OPTION
// SILENTLY GETS NO SPOT. store.LatestAsOf filters `kind = $2` exactly (it does
// not fall back to "any kind" the way store.Query does), so a venue whose ticks
// land as PriceKindLast or PriceKindSettlement while this reads Close matches
// zero rows for every instrument — and the consequence is not an error, it is
// SkipNoSpot on every position, a Gamma of zero, and a book that looks like it
// holds no options. That failure is total and uniform, which is exactly the kind
// this repository has shipped before (#257). Two things make it survivable: the
// unresolved observer fires once per instrument per call, so the miss is
// counted rather than silent, and WithPriceKind exists to correct it without a
// code change.
//
// Close is the default because it is what the estate actually writes: the
// market-data ingestion path stamps PriceKindClose (internal/marketdata/
// ingest.go), returns.DefaultReturnKind is Close, and dataset.NewMaterializer
// defaults its spot read to Close. Reading anything else by default would make
// this provider disagree with the returns and volatility it is priced alongside
// — the spot in the Greeks and the spot in the vol estimate must be the same
// series or the sensitivities are computed off two different markets.
const DefaultKind = store.PriceKindClose

// DefaultMaxAge bounds how old a mark may be, measured at the valuation time,
// and still be used as spot.
//
// # Why there is a bound at all
//
// store.LatestAsOf has NO lower bound on ObservationTime. A close from 2019 is a
// perfectly valid answer to "the price we believed was current as of today" if
// nothing has been ingested since. Priced through Black-Scholes that produces a
// full set of Greeks — a Delta, a Gamma, a Vega, all finite, all plausible, all
// wrong. A GREEK COMPUTED OFF A YEARS-OLD SPOT IS WORSE THAN NO GREEK, because
// no Greek is reported (SkipNoSpot) and a confidently wrong one is not; it flows
// into limit checks and hedge sizing as if it were measured.
//
// # Where the number comes from
//
// The series read is a daily close (see DefaultKind), so the bound must clear
// the longest gap a HEALTHY daily series routinely contains and still trip on a
// feed that has stopped. The longest routine gap on a western venue is a weekend
// bracketed by holidays on both sides: Thursday's close is the current mark on
// the following Tuesday when Friday and Monday are both closed — Good Friday
// plus Easter Monday is exactly that shape, and so is a Friday-observed
// Christmas followed by a Monday-observed Boxing Day. That is 5 calendar days.
// One calendar week is the smallest round bound above it, so a healthy feed
// never trips this and a feed that has missed a full trading week always does.
//
// WHAT IT DOES NOT CLEAR, AND WHO MUST RAISE IT: Lunar New Year and Golden Week
// close their venues for 9-10 calendar days. A book on those venues will see
// every option on them skipped for a week under this default. That is a
// deliberate trade — the miss is reported through the unresolved observer with
// reason ReasonStale and the measured age, so the operator raises the bound on
// evidence, which is strictly better than the alternative this replaces, where a
// mark of any age at all was used and nothing said so.
const DefaultMaxAge = 7 * 24 * time.Hour

// The reasons a spot did not resolve.
//
// A SMALL CLOSED SET, NOT FREE TEXT, for the same reason compute's OnSkip
// reasons are: the caller counts by this string and a metric label must not be
// whatever a future edit writes.
//
// These exist because greeks.go collapses all of them into one SkipNoSpot and
// says so explicitly — "which of the three it was is knowable only inside the
// provider, and that is where it is distinguished". This is that place. A
// deployment that has never loaded prices (ReasonNoObservation), one whose feed
// died last month (ReasonStale), one reading the wrong kind (also
// ReasonNoObservation, on every instrument at once) and one with a database
// problem (ReasonStoreError) are four different operational faults with four
// different fixes, and upstream they are one number.
const (
	// ReasonNoObservation: the store holds no mark for this instrument under the
	// configured kind, at or before the valuation time. Either the underlying was
	// never ingested, or the valuation time predates its first mark, or the
	// deployment's prices are stored under a different PriceKind — see
	// DefaultKind. If this fires for EVERY instrument, suspect the kind first.
	ReasonNoObservation = "no_observation"
	// ReasonStale: a mark exists and was already older than the configured
	// bound at the valuation time. The age is passed to the observer.
	ReasonStale = "stale"
	// ReasonUnusablePrice: a mark exists, is fresh, and cannot be priced with —
	// a nil Decimal, zero, negative, or non-finite after conversion. This is a
	// data defect in the store rather than a gap in it, and it is the one reason
	// here that says something is WRONG rather than MISSING.
	ReasonUnusablePrice = "unusable_price"
	// ReasonStoreError: the read itself failed. See Spot's doc for why this
	// still returns ok=false and where the loud signal lives.
	ReasonStoreError = "store_error"
	// ReasonNoAsOf: the caller passed a zero valuation time. Refused before the
	// store is touched — see Spot.
	ReasonNoAsOf = "no_as_of"
)

// Provider resolves spot from the market-data store. Construct with FromStore.
//
// # The missing case is a counter, not an error, and that is forced
//
// compute.SpotProvider returns (float64, bool) with no error channel, so every
// failure below — no mark, a stale mark, an unusable price, a database that
// cannot be read — collapses to the same false. greeks.go then reports
// SkipNoSpot and the position contributes ZERO to Delta, Gamma, Vega, Theta and
// Rho. That zero is the dangerous direction: a book's measured convexity
// shrinks, and a Gamma of zero is indistinguishable from a book holding no
// options at all.
//
// Widening the seam would touch every consumer and is a change worth taking on
// its own evidence rather than smuggled in with the first implementation (the
// same ruling termsource made). So the resolution is made OBSERVABLE instead:
// WithUnresolvedObserver fires on every one of those paths with the reason that
// distinguishes them.
//
// UNLIKE termsource's onMissing, EVERY FIRING HERE IS A REAL GAP. OptionTerms
// answers false for every share on the book, so its observer cannot tell a
// missing load from a stock. Spot is called only after terms have already
// resolved — greeks.go asks for a spot only for an instrument it has established
// is an option — so an unresolved spot always means an option that will not be
// priced. There is no noise floor to subtract: this counter climbing above zero
// is a defect, always.
type Provider struct {
	store     PriceStore
	kind      store.PriceKind
	maxAge    time.Duration
	bounded   bool
	unbounded bool // WithoutStalenessBound was passed; resolved in FromStore

	onUnresolved func(instrumentID, reason string, age time.Duration)
}

// Compile-time assertion that this satisfies the seam it exists for. Without it
// a signature drift in compute.SpotProvider would be discovered at the
// composition root rather than here.
var _ compute.SpotProvider = (*Provider)(nil)

// Option customizes a Provider.
type Option func(*Provider)

// WithPriceKind reads a mark other than DefaultKind. THE ONE OPTION A
// MIS-CONFIGURED DEPLOYMENT NEEDS — see DefaultKind for what reading the wrong
// one does.
func WithPriceKind(k store.PriceKind) Option {
	return func(p *Provider) { p.kind = k }
}

// WithMaxAge overrides DefaultMaxAge. A non-positive duration is REFUSED by
// FromStore rather than quietly meaning "unbounded": unbounded is a real
// decision with a real cost (see DefaultMaxAge) and must be spelled, not
// arrived at by a zero-valued config field.
func WithMaxAge(d time.Duration) Option {
	return func(p *Provider) { p.maxAge, p.bounded = d, true }
}

// WithoutStalenessBound accepts a mark of any age — the pre-#509 behaviour,
// which is to say a 2019 close pricing today's Greeks with nothing to indicate
// it. Legitimate for a backfill or a historical replay whose valuation times are
// themselves old; wrong for a live risk engine. It wins over WithMaxAge
// regardless of the order the options are passed in.
func WithoutStalenessBound() Option {
	return func(p *Provider) { p.unbounded = true }
}

// WithUnresolvedObserver sets the hook invoked whenever Spot answers false —
// the "this option will contribute nothing to every Greek" signal. reason is one
// of the Reason* constants; age is the mark's age at the valuation time and is
// meaningful only for ReasonStale, zero otherwise.
//
// Nil (the default) means the degradation is unobserved, which is the state
// before this type existed. Wire it at the composition root.
func WithUnresolvedObserver(fn func(instrumentID, reason string, age time.Duration)) Option {
	return func(p *Provider) { p.onUnresolved = fn }
}

// FromStore returns a spot provider over s.
//
// IT RETURNS AN ERROR RATHER THAN A WORKING-LOOKING PROVIDER, because both
// refusals below produce the same symptom at runtime — every option on the book
// skipped, forever, uniformly — and that symptom is indistinguishable from a
// book with no options in it. A misconfiguration must surface on the first
// event, and the first event here is startup.
func FromStore(s PriceStore, opts ...Option) (*Provider, error) {
	if s == nil {
		return nil, errNilStore
	}
	p := &Provider{store: s, kind: DefaultKind, maxAge: DefaultMaxAge, bounded: true}
	for _, o := range opts {
		if o != nil {
			o(p)
		}
	}
	if p.unbounded {
		// Resolved after every option has run, so the opt-out is order-independent:
		// a config that both sets a bound and disables bounding gets no bound
		// whichever order the options arrive in, rather than depending on the
		// order a caller happened to list them.
		p.bounded = false
	}
	if !isKnownKind(p.kind) {
		// PriceKindUnspecified is the dangerous one and the reason this check
		// exists: store.Observation.validate REFUSES to persist an unspecified
		// kind, so a provider configured with it matches zero rows for every
		// instrument that will ever exist. A kind outside the enum does the same.
		return nil, errUnknownKind
	}
	if p.bounded && p.maxAge <= 0 {
		return nil, errNonPositiveMaxAge
	}
	return p, nil
}

// Spot resolves the instrument's spot price as of a point in time. ok=false
// means "this option cannot be priced" — see the type doc for what that costs
// and how to observe it.
//
// STALENESS IS MEASURED ON THE OBSERVATION AXIS AGAINST asOf, NEVER AGAINST THE
// WALL CLOCK. The question is "how old was this mark at the valuation time",
// which is the only form that survives replay: a wall-clock rule would declare
// every mark in a 2019 backtest stale and return no Greeks for the entire
// history, while accepting a 2019 mark for a valuation dated today. The provider
// therefore needs no clock and stays deterministic under test.
func (p *Provider) Spot(ctx context.Context, instrumentID string, asOf time.Time) (float64, bool) {
	if p == nil || p.store == nil {
		return 0, false
	}
	if asOf.IsZero() {
		// REFUSED BEFORE THE STORE IS TOUCHED. A zero asOf means "latest of
		// everything" to store.LatestAsOf — no knowledge horizon — so a vendor
		// correction stamped after the valuation would leak backward into the
		// number, which is precisely what the bitemporal contract exists to
		// prevent. It also leaves staleness undefined, since there is no
		// reference point to measure age from. A portfolio with no AsOf is a
		// portfolio no event has been applied to; pricing it off unbounded
		// knowledge would be a silent future leak, so it gets no spot instead.
		p.report(instrumentID, ReasonNoAsOf, 0)
		return 0, false
	}
	obs, ok, err := p.store.LatestAsOf(ctx, instrumentID, p.kind, asOf)
	if err != nil {
		// A STORE ERROR ALSO RETURNS false, and that is forced rather than
		// chosen: the seam has no error channel. It is not silent, though — a
		// database that cannot be read fails the readiness Ping (store.Store.Ping,
		// wired to the risk engine's probe) long before it fails here, so the loud
		// signal exists upstream of this call. The counter below is what
		// distinguishes it from an empty store, which the probe cannot see.
		p.report(instrumentID, ReasonStoreError, 0)
		return 0, false
	}
	if !ok {
		p.report(instrumentID, ReasonNoObservation, 0)
		return 0, false
	}
	if age := asOf.Sub(obs.ObservationTime); p.bounded && age > p.maxAge {
		p.report(instrumentID, ReasonStale, age)
		return 0, false
	}
	px, okPx := decimalToFloat(obs.Price)
	if !okPx || px <= 0 || math.IsInf(px, 0) || math.IsNaN(px) {
		// REJECTED HERE EVEN THOUGH greeks.go ALSO REJECTS spot <= 0, and the
		// duplication is deliberate. Upstream the rejection is anonymous — it
		// lands in the same SkipNoSpot bucket as "no provider wired" and "no mark
		// at all" — and only this layer can say that a mark EXISTS and is
		// unusable, which is a stored-data defect (a bad vendor tick, a sign
		// error, an overflowing exponent) rather than an ingestion gap, and needs
		// a different fix. It also keeps the seam's contract honest for every
		// other consumer of SpotProvider: ok=true never means -3.0.
		//
		// This duplicates a GUARD, not an implementation. The conversion itself
		// lives in one place per plane, which is what "one implementation per
		// concept" is protecting.
		p.report(instrumentID, ReasonUnusablePrice, 0)
		return 0, false
	}
	return px, true
}

// report fires the unresolved hook if the caller asked to hear about them.
func (p *Provider) report(instrumentID, reason string, age time.Duration) {
	if p.onUnresolved != nil {
		p.onUnresolved(instrumentID, reason, age)
	}
}

// decimalToFloat converts an exact common.v1.Decimal to the float the pricing
// layer works in. ok is false for a nil decimal, so an absent price is refused
// rather than read as 0.
//
// THE DECIMAL BOUNDARY IS HERE. Prices stay exact common.v1.Decimal on the wire
// and in the store (double is banned for prices, EVT-10); Greeks are
// float-derived analytics computed by Black-Scholes, so the lossy conversion is
// confined to the analytics plane where a float64 input is the contract — the
// same argument, and the same arithmetic, as dataset.decimalToFloat and
// termsource.decimalToFloat. Converting anywhere else would put a float on a
// path that is supposed to be exact.
func decimalToFloat(d *commonpb.Decimal) (float64, bool) {
	if d == nil {
		return 0, false
	}
	return float64(d.GetCoefficient()) * math.Pow10(int(d.GetExponent())), true
}

// isKnownKind reports whether k is a PriceKind the store can actually hold.
// Unspecified is excluded deliberately — see FromStore.
func isKnownKind(k store.PriceKind) bool {
	return k >= store.PriceKindClose && k <= store.PriceKindLast
}

// Construction errors. Named so a composition root can assert on them and a test
// does not match on prose.
var (
	errNilStore          = constErr("spotsource: nil price store — every option would go unpriced")
	errUnknownKind       = constErr("spotsource: price kind is unspecified or unknown — it would match no stored observation for any instrument")
	errNonPositiveMaxAge = constErr("spotsource: max age must be positive; use WithoutStalenessBound to accept a mark of any age")
)

type constErr string

func (e constErr) Error() string { return string(e) }
