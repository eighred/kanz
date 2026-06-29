// Package pricing is the data master's pricing/valuation oversight (MASTER-01d):
// multi-source price arbitration with tolerance and staleness checks, and an
// exception queue whose entries carry a human-override audit trail. It is the
// DATA-05/IBOR-01e break-detection stance applied to prices — two-or-more
// independent views keyed on a shared identity, a deviation beyond tolerance
// surfaced as an exception rather than silently absorbed. Kept in the datamaster
// service (not internal/integrity, whose detectors are envelope/stream-specific).
//
// Prices are float64 here — arbitration is a comparison/selection over candidate
// values (a derived oversight statistic), the EVT-14 "double at the analytics
// edge" stance the risk plane takes via decimalToFloat; the wire PriceCandidate
// and the chosen price stay common.v1.Decimal money.
package pricing

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// Candidate is one source's price for an instrument — an arbitration input.
type Candidate struct {
	Source string
	Price  float64
	AsOf   time.Time
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
	ChosenPrice float64
	At          time.Time
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
	// Chosen is the consensus (median of fresh candidates); valid iff HasPrice.
	Chosen   float64
	HasPrice bool
	// Fresh are the candidates that passed the staleness filter, sorted by source.
	Fresh []Candidate
	// Exceptions are the breaks detected during arbitration (stale/tolerance/
	// missing), each with a deterministic ID for the oversight queue.
	Exceptions []Exception
}

// DefaultTolerance is the relative deviation from consensus beyond which a
// candidate is flagged (5%).
const DefaultTolerance = 0.05

// DefaultStaleness is how old a candidate may be before it is excluded as stale.
const DefaultStaleness = 24 * time.Hour

// Arbitrate selects an instrument's consensus price from its candidates and
// detects breaks. Candidates older than now−staleness are excluded (each raising
// a STALE_PRICE exception); the consensus is the MEDIAN of the fresh candidates
// (robust to a single outlier); any fresh candidate deviating from the consensus
// by more than tolerance (relative) raises a PRICE_TOLERANCE exception. No
// candidates ⇒ MISSING_PRICE; all stale ⇒ no consensus, the stale exceptions
// stand. tolerance/staleness ≤ 0 fall back to the defaults.
func Arbitrate(instrumentID string, candidates []Candidate, tolerance float64, staleness time.Duration, now time.Time) Arbitration {
	if tolerance <= 0 {
		tolerance = DefaultTolerance
	}
	if staleness <= 0 {
		staleness = DefaultStaleness
	}
	res := Arbitration{InstrumentID: instrumentID}

	if len(candidates) == 0 {
		res.Exceptions = append(res.Exceptions, Exception{
			ID:           exID(instrumentID, KindMissingPrice, ""),
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
		if c.AsOf.Before(cutoff) {
			res.Exceptions = append(res.Exceptions, Exception{
				ID:           exID(instrumentID, KindStalePrice, c.Source),
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
		return res // all stale; the stale exceptions stand, no consensus
	}

	consensus := median(fresh)
	res.Chosen = consensus
	res.HasPrice = true

	if consensus != 0 {
		for _, c := range fresh {
			dev := (c.Price - consensus) / consensus
			if dev < 0 {
				dev = -dev
			}
			if dev > tolerance {
				res.Exceptions = append(res.Exceptions, Exception{
					ID:           exID(instrumentID, KindPriceTolerance, c.Source),
					Kind:         KindPriceTolerance,
					InstrumentID: instrumentID,
					Detail:       fmt.Sprintf("source %s price %.4f deviates %.2f%% from consensus %.4f (tolerance %.2f%%)", c.Source, c.Price, dev*100, consensus, tolerance*100),
					Status:       StatusOpen,
					DetectedAt:   now,
				})
			}
		}
	}
	return res
}

// median returns the median price of the candidates (assumed non-empty).
func median(cs []Candidate) float64 {
	v := make([]float64, len(cs))
	for i, c := range cs {
		v[i] = c.Price
	}
	sort.Float64s(v)
	n := len(v)
	if n%2 == 1 {
		return v[n/2]
	}
	return (v[n/2-1] + v[n/2]) / 2
}

// exID builds a deterministic exception id so a re-detected break maps to the
// same queue entry (idempotent detection — the DATA break-detection discipline).
func exID(instrumentID string, kind ExceptionKind, source string) string {
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
// why. Unknown id ⇒ error.
func (q *Queue) Override(id, actor, reason string, chosenPrice float64, at time.Time) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	e, ok := q.byID[id]
	if !ok {
		return fmt.Errorf("pricing: unknown exception %q", id)
	}
	if actor == "" || reason == "" {
		return fmt.Errorf("pricing: override requires actor and reason")
	}
	e.Overrides = append(e.Overrides, Override{Actor: actor, Reason: reason, ChosenPrice: chosenPrice, At: at})
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
