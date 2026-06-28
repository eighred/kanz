// Package alternatives is the private-markets analytics core (ALT-01): the
// commitment-position accounting (committed / called / uncalled / distributed /
// NAV and the J-curve), the private-asset metrics (IRR, TVPI/DPI/RVPI, PME), and
// the proxy-beta mapping that blends an illiquid NAV into the public-factor risk
// view (ALT-01d). It is a pure analytics package OUTSIDE internal/risk (RISK-02),
// so the factor loadings it maps to are an INPUT, not a reach into the risk
// module — the same stance OPT-01/COLL-01 take.
//
// The event-sourced fund-position SERVICE (the durable journal + bus) is
// kanz/services/alternatives; this package holds the fold and the math it tests.
package alternatives

import (
	"math/big"
	"sort"
	"time"
)

// EventType classifies a commitment lifecycle event.
type EventType int

const (
	EventUnspecified EventType = iota
	// EventCommit opens a commitment with its committed amount.
	EventCommit
	// EventCall is a capital call (drawdown) against the commitment.
	EventCall
	// EventDistribution is a return of capital/gains to the investor.
	EventDistribution
	// EventNAVMark is an illiquid valuation of the residual interest.
	EventNAVMark
)

// Event is one commitment lifecycle event — the working shape behind the
// alternatives.v1 messages, folded into a Position. Amounts are exact (*big.Rat).
type Event struct {
	EventID      string
	CommitmentID string
	Type         EventType
	// Amount is the committed amount (Commit), call amount (Call), distribution
	// amount (Distribution), or the residual NAV (NAVMark). Always non-negative.
	Amount *big.Rat
	Date   time.Time
}

// Position is the folded state of one commitment: how much is committed, drawn
// (called), returned (distributed), and the latest residual NAV — plus the dated
// external cashflows (calls out, distributions in) that drive the J-curve and the
// IRR. It is the fold of the journal; replaying the journal reproduces it.
type Position struct {
	CommitmentID string
	Committed    *big.Rat
	Called       *big.Rat
	Distributed  *big.Rat
	NAV          *big.Rat
	NAVDate      time.Time

	// Flows are the dated external cashflows from the investor's perspective:
	// a call is a negative flow (capital paid in), a distribution a positive flow
	// (capital returned). The residual NAV is NOT a flow (it is unrealized) — the
	// metrics add it as a terminal value where the convention requires.
	Flows []CashFlow

	seen map[string]bool
}

// NewPosition returns an empty position for a commitment.
func NewPosition(commitmentID string) *Position {
	return &Position{
		CommitmentID: commitmentID,
		Committed:    new(big.Rat),
		Called:       new(big.Rat),
		Distributed:  new(big.Rat),
		NAV:          new(big.Rat),
		seen:         make(map[string]bool),
	}
}

// Apply folds one event into the position. It is idempotent on EventID, so a
// replay or redelivery is a no-op. An event for another commitment is ignored.
func (p *Position) Apply(e *Event) {
	if e == nil || e.EventID == "" || (e.CommitmentID != "" && e.CommitmentID != p.CommitmentID) {
		return
	}
	if p.seen[e.EventID] {
		return
	}
	p.seen[e.EventID] = true

	amt := orZero(e.Amount)
	switch e.Type {
	case EventCommit:
		p.Committed.Add(p.Committed, amt)
	case EventCall:
		p.Called.Add(p.Called, amt)
		p.Flows = append(p.Flows, CashFlow{Date: e.Date, Amount: ratToFloat(new(big.Rat).Neg(amt))})
	case EventDistribution:
		p.Distributed.Add(p.Distributed, amt)
		p.Flows = append(p.Flows, CashFlow{Date: e.Date, Amount: ratToFloat(amt)})
	case EventNAVMark:
		p.NAV = new(big.Rat).Set(amt)
		p.NAVDate = e.Date
	}
}

// Uncalled is the dry powder still to be funded: committed − called, floored at
// zero (over-calls beyond commitment are not represented as negative uncalled).
func (p *Position) Uncalled() *big.Rat {
	u := new(big.Rat).Sub(p.Committed, p.Called)
	if u.Sign() < 0 {
		return new(big.Rat)
	}
	return u
}

// JCurvePoint is one point on the cumulative-net-cashflow curve.
type JCurvePoint struct {
	Date       time.Time
	Cumulative float64 // running Σ of flows: negative early (calls), recovering as distributions arrive
}

// JCurve returns the cumulative net external cashflow over time — the J-curve:
// it dips negative as capital is called, then recovers (and ideally turns
// positive) as distributions exceed paid-in capital. Flows are taken in date
// order.
func (p *Position) JCurve() []JCurvePoint {
	flows := append([]CashFlow(nil), p.Flows...)
	sort.SliceStable(flows, func(i, j int) bool { return flows[i].Date.Before(flows[j].Date) })
	out := make([]JCurvePoint, 0, len(flows))
	var cum float64
	for _, f := range flows {
		cum += f.Amount
		out = append(out, JCurvePoint{Date: f.Date, Cumulative: cum})
	}
	return out
}

// Replay folds a journal into a fresh position in date order (ties broken by
// event id) — the canonical reconstruction. Replaying the same journal yields the
// same position.
func Replay(commitmentID string, events []*Event) *Position {
	p := NewPosition(commitmentID)
	for _, e := range sortedEvents(events) {
		p.Apply(e)
	}
	return p
}

func sortedEvents(events []*Event) []*Event {
	out := append([]*Event(nil), events...)
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Date.Equal(out[j].Date) {
			return out[i].Date.Before(out[j].Date)
		}
		return out[i].EventID < out[j].EventID
	})
	return out
}

func orZero(r *big.Rat) *big.Rat {
	if r == nil {
		return new(big.Rat)
	}
	return r
}

func ratToFloat(r *big.Rat) float64 {
	f, _ := r.Float64()
	return f
}
