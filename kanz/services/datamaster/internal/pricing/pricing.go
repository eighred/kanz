// Package pricing is the data master's pricing/valuation oversight (MASTER-01d):
// multi-source price arbitration with tolerance and staleness checks, and an
// exception queue whose entries carry a human-override audit trail. It is the
// DATA-05/IBOR-01e break-detection stance applied to prices — two-or-more
// independent views keyed on a shared identity, a deviation beyond tolerance
// surfaced as an exception rather than silently absorbed.
//
// # Prices here are exact (DATA-M8b)
//
// They were float64, defended as "a comparison over candidate values, a derived
// oversight statistic" — the EVT-14 double-at-the-analytics-edge stance. That
// defence was wrong on both halves, and DATA-M8a made it materially worse:
//
//   - The consensus is not a statistic that gets discarded. It is the mark this
//     service exists to publish (feed.NormalizePrice puts it on the wire as a
//     market.v1 Quote), and every consumer treats it as the price of the
//     instrument.
//   - The override price is a NAMED HUMAN'S DECISION, now persisted to an
//     append-only audit trail. A compliance record must say what the human chose,
//     not the nearest binary double to it.
//
// Every value on this path is a sum, a quotient, a median or a comparison of
// exact decimals, and all four of those keep a rational exact. There is no sqrt
// here — nothing that would justify the honest float exception the FRTB capital
// path takes. So: *big.Rat end to end, `internal/dec` for parsing and rendering,
// and rounding introduced in exactly one place (dec.ToProto, at the wire).
package pricing

import (
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"sync"
	"time"

	"github.com/eighred/kanz/internal/dec"
)

// Candidate is one source's price for an instrument — an arbitration input.
//
// InstrumentID is what the candidate is a price OF. Without it a feed's price
// list is unattributed, and a caller holding candidates from several instruments
// has no way to select the ones it is arbitrating: it would price AAPL off MSFT's
// quotes. Callers filter to one instrument before calling Arbitrate.
//
// A nil Price is not a price: the source supplied none, and Arbitrate says so
// rather than voting a zero into the consensus.
type Candidate struct {
	InstrumentID string
	Source       string
	Price        *big.Rat
	AsOf         time.Time
}

// ExceptionKind classifies a data-quality break (mirrors master.v1.ExceptionKind).
type ExceptionKind string

const (
	KindPriceTolerance     ExceptionKind = "PRICE_TOLERANCE"
	KindStalePrice         ExceptionKind = "STALE_PRICE"
	KindIdentifierConflict ExceptionKind = "IDENTIFIER_CONFLICT"
	KindMissingPrice       ExceptionKind = "MISSING_PRICE"
)

// Status tracks an exception through the queue (mirrors master.v1.ExceptionStatus).
type Status string

const (
	StatusOpen       Status = "OPEN"
	StatusOverridden Status = "OVERRIDDEN"
	StatusResolved   Status = "RESOLVED"
)

// Override is the audit record of a human override — appended, never mutated.
type Override struct {
	Actor       string
	Reason      string
	ChosenPrice *big.Rat
	At          time.Time
}

// MarshalJSON renders the chosen price as an exact decimal STRING.
//
// A JSON number is an IEEE-754 double by definition, so serializing the price a
// human chose as one would round it on the way out of the audit trail — the same
// reason the regulatory filings emit decimal strings. big.Rat's own MarshalText
// would emit "a/b", which is exact but is not a price anyone can read.
func (o Override) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Actor       string    `json:"actor"`
		Reason      string    `json:"reason"`
		ChosenPrice string    `json:"chosen_price"`
		At          time.Time `json:"at"`
	}{o.Actor, o.Reason, dec.Str(o.ChosenPrice), o.At})
}

// Exception is a detected break plus its override audit trail.
type Exception struct {
	ID           string
	Kind         ExceptionKind
	InstrumentID string
	Detail       string
	Status       Status
	DetectedAt   time.Time
	Overrides    []Override
}

// Arbitration is the outcome of arbitrating an instrument's price candidates.
type Arbitration struct {
	InstrumentID string
	// Chosen is the consensus (median of fresh candidates); non-nil iff HasPrice.
	Chosen   *big.Rat
	HasPrice bool
	// Fresh are the candidates that passed the staleness filter, sorted by source.
	Fresh []Candidate
	// Exceptions are the breaks detected during arbitration (stale/tolerance/
	// missing), each with a deterministic ID for the oversight queue.
	Exceptions []Exception
}

// DefaultTolerance is the relative deviation from consensus beyond which a
// candidate is flagged (5%). It is a function, not a package var, because a
// *big.Rat is a mutable pointer: a shared one could be scribbled on by any caller
// that did arithmetic in place.
func DefaultTolerance() *big.Rat { return dec.Rat("0.05") }

// DefaultStaleness is how old a candidate may be before it is excluded as stale.
const DefaultStaleness = 24 * time.Hour

// Arbitrate selects an instrument's consensus price from its candidates and
// detects breaks. Candidates older than now−staleness are excluded (each raising
// a STALE_PRICE exception); a candidate carrying no price raises MISSING_PRICE
// against its source rather than voting a zero; the consensus is the MEDIAN of
// the fresh candidates (robust to a single outlier); any fresh candidate
// deviating from the consensus by more than tolerance (relative) raises a
// PRICE_TOLERANCE exception. No candidates ⇒ MISSING_PRICE; all stale ⇒ no
// consensus, the stale exceptions stand. A nil/non-positive tolerance and a
// staleness ≤ 0 fall back to the defaults.
//
// Every comparison here is exact. The tolerance test is written as a
// multiplication — |price − consensus| > tolerance · |consensus| — rather than the
// division it reads as, so a candidate sitting exactly ON the 5% boundary decides
// deterministically instead of on whichever way the last binary rounding fell.
func Arbitrate(instrumentID string, candidates []Candidate, tolerance *big.Rat, staleness time.Duration, now time.Time) Arbitration {
	if tolerance == nil || tolerance.Sign() <= 0 {
		tolerance = DefaultTolerance()
	}
	if staleness <= 0 {
		staleness = DefaultStaleness
	}
	res := Arbitration{InstrumentID: instrumentID}

	if len(candidates) == 0 {
		res.Exceptions = append(res.Exceptions, Exception{
			ID:           ExceptionID(instrumentID, KindMissingPrice, ""),
			Kind:         KindMissingPrice,
			InstrumentID: instrumentID,
			Detail:       "no price candidates",
			Status:       StatusOpen,
			DetectedAt:   now,
		})
		return res
	}

	cutoff := now.Add(-staleness)
	var fresh []Candidate
	for _, c := range candidates {
		if c.Price == nil {
			res.Exceptions = append(res.Exceptions, Exception{
				ID:           ExceptionID(instrumentID, KindMissingPrice, c.Source),
				Kind:         KindMissingPrice,
				InstrumentID: instrumentID,
				Detail:       fmt.Sprintf("source %s supplied no price", c.Source),
				Status:       StatusOpen,
				DetectedAt:   now,
			})
			continue
		}
		if c.AsOf.Before(cutoff) {
			res.Exceptions = append(res.Exceptions, Exception{
				ID:           ExceptionID(instrumentID, KindStalePrice, c.Source),
				Kind:         KindStalePrice,
				InstrumentID: instrumentID,
				Detail:       fmt.Sprintf("source %s price as of %s is older than %s", c.Source, c.AsOf.UTC().Format(time.RFC3339), staleness),
				Status:       StatusOpen,
				DetectedAt:   now,
			})
			continue
		}
		fresh = append(fresh, c)
	}
	sort.Slice(fresh, func(i, j int) bool { return fresh[i].Source < fresh[j].Source })
	res.Fresh = fresh

	if len(fresh) == 0 {
		return res // all stale/priceless; those exceptions stand, no consensus
	}

	consensus := median(fresh)
	res.Chosen = consensus
	res.HasPrice = true

	if consensus.Sign() == 0 {
		return res // a relative deviation from zero is undefined
	}
	limit := new(big.Rat).Mul(tolerance, new(big.Rat).Abs(consensus))
	for _, c := range fresh {
		gap := new(big.Rat).Abs(new(big.Rat).Sub(c.Price, consensus))
		if gap.Cmp(limit) <= 0 {
			continue
		}
		res.Exceptions = append(res.Exceptions, Exception{
			ID:           ExceptionID(instrumentID, KindPriceTolerance, c.Source),
			Kind:         KindPriceTolerance,
			InstrumentID: instrumentID,
			Detail: fmt.Sprintf("source %s price %s deviates %s%% from consensus %s (tolerance %s%%)",
				c.Source, dec.Str(c.Price), percent(gap, consensus), dec.Str(consensus), percent(tolerance, big.NewRat(1, 1))),
			Status:     StatusOpen,
			DetectedAt: now,
		})
	}
	return res
}

// percent renders num/den as a percentage for a human-readable break detail,
// displayed to 2dp. The exact prices it sits beside are rendered exactly; this is
// the one rounded figure in the message and it is prose, not data.
func percent(num, den *big.Rat) string {
	p := new(big.Rat).Quo(num, den)
	return p.Mul(p, big.NewRat(100, 1)).FloatString(2)
}

// median returns the median price of the candidates (assumed non-empty, all
// priced). A median of exact decimals is exact: for an even count it is the mean
// of the two middle values, and a sum and a halving of rationals stay rational.
func median(cs []Candidate) *big.Rat {
	v := make([]*big.Rat, len(cs))
	for i, c := range cs {
		v[i] = c.Price
	}
	sort.Slice(v, func(i, j int) bool { return v[i].Cmp(v[j]) < 0 })
	n := len(v)
	if n%2 == 1 {
		return new(big.Rat).Set(v[n/2])
	}
	sum := new(big.Rat).Add(v[n/2-1], v[n/2])
	return sum.Quo(sum, big.NewRat(2, 1))
}

// ExceptionID builds a deterministic exception id so a re-detected break maps to the
// same queue entry (idempotent detection — the DATA break-detection discipline).
func ExceptionID(instrumentID string, kind ExceptionKind, source string) string {
	if source == "" {
		return fmt.Sprintf("%s:%s", instrumentID, kind)
	}
	return fmt.Sprintf("%s:%s:%s", instrumentID, kind, source)
}

// Queue is the oversight exception queue — the open breaks awaiting review and
// their override audit trail. Goroutine-safe.
type Queue struct {
	mu   sync.Mutex
	byID map[string]*Exception
}

// NewQueue returns an empty exception queue.
func NewQueue() *Queue { return &Queue{byID: map[string]*Exception{}} }

// Add records an exception. Re-adding an id keeps the existing entry (and its
// override history) — detection is idempotent on the deterministic id, so a break
// that reproduces does not wipe its review state.
func (q *Queue) Add(e Exception) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, ok := q.byID[e.ID]; ok {
		return
	}
	cp := e
	q.byID[e.ID] = &cp
}

// AddAll records every exception from an arbitration.
func (q *Queue) AddAll(exs []Exception) {
	for _, e := range exs {
		q.Add(e)
	}
}

// Override appends a human override to an exception and marks it OVERRIDDEN. The
// override history is append-only — the full audit trail of who chose what and
// why. Unknown id ⇒ error. The chosen price must be an exact decimal: this is the
// record of what a named human decided, and it is what a reviewer will be shown.
func (q *Queue) Override(id, actor, reason string, chosenPrice *big.Rat, at time.Time) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	e, ok := q.byID[id]
	if !ok {
		return fmt.Errorf("pricing: unknown exception %q", id)
	}
	if actor == "" || reason == "" {
		return fmt.Errorf("pricing: override requires actor and reason")
	}
	if chosenPrice == nil {
		return fmt.Errorf("pricing: override requires a chosen price")
	}
	e.Overrides = append(e.Overrides, Override{Actor: actor, Reason: reason, ChosenPrice: new(big.Rat).Set(chosenPrice), At: at})
	e.Status = StatusOverridden
	return nil
}

// Get returns an exception by id.
func (q *Queue) Get(id string) (Exception, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	e, ok := q.byID[id]
	if !ok {
		return Exception{}, false
	}
	return *e, true
}

// Open returns the OPEN exceptions, sorted by id for deterministic output.
func (q *Queue) Open() []Exception {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []Exception
	for _, e := range q.byID {
		if e.Status == StatusOpen {
			out = append(out, *e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
