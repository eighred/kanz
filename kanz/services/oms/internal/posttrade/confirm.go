// Package posttrade is the OMS post-trade lifecycle (POST-01): it matches OMS-01
// fills to counterparty confirmations (affirm or flag a break), generates
// settlement instructions and tracks their T+N status through a SettlementVenue
// seam, and ages settlement fails into a FACT that feeds the AUTO-01 controller
// and the IBOR-01e custodian reconciliation. It closes the loop OMS-01 opens.
//
// Like the execution layer, it carries its own internal working shapes (the
// settlement.v1 proto is the wire schema, generated-not-committed per EVT-15a) and
// reuses the existing order.v1 SDK (order.v1.Fill) plus the OMS dec helper for
// exact-decimal comparison.
package posttrade

import (
	"math/big"
	"sort"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dec"
)

// Confirmation is a counterparty's trade confirmation — the working shape behind
// settlement.v1.TradeConfirmation. Its terms are matched against the OMS fill.
type Confirmation struct {
	ConfirmationID string
	FillID         string
	InstrumentID   string
	Side           orderpb.Side
	Quantity       *big.Rat
	Price          *big.Rat
	Counterparty   string
	SettlementDate time.Time
}

// MatchTolerance bounds the acceptable difference between a fill and a
// confirmation on quantity and price. A nil/zero bound requires an exact match.
type MatchTolerance struct {
	Quantity *big.Rat
	Price    *big.Rat
}

// Break is a field-level mismatch between a fill and its confirmation — the
// affirmation break a post-trade operations desk investigates. Fields names the
// terms that disagreed.
type Break struct {
	FillID         string
	ConfirmationID string
	Fields         []string
}

// MatchResult is the outcome of matching a fill to a confirmation.
type MatchResult struct {
	// Matched is true when every term agrees within tolerance — the trade may be
	// affirmed. When false, Break carries the disagreeing fields.
	Matched bool
	Break   *Break
}

// MatchFill compares an OMS-01 fill to a counterparty confirmation and reports
// whether they agree within tolerance. A disagreement on instrument, side,
// quantity, or price is a break (the POST-01b break detection); a missing
// confirmation for a fill is the caller's concern (see UnconfirmedFills).
func MatchFill(fill *orderpb.Fill, conf Confirmation, tol MatchTolerance) MatchResult {
	var fields []string
	if fill.GetInstrumentId() != conf.InstrumentID {
		fields = append(fields, "instrument_id")
	}
	if fill.GetSide() != conf.Side {
		fields = append(fields, "side")
	}
	if beyond(dec.FromProto(fill.GetQuantity()), conf.Quantity, tol.Quantity) {
		fields = append(fields, "quantity")
	}
	if beyond(dec.FromProto(fill.GetPrice()), conf.Price, tol.Price) {
		fields = append(fields, "price")
	}
	if len(fields) == 0 {
		return MatchResult{Matched: true}
	}
	return MatchResult{Break: &Break{FillID: fill.GetFillId(), ConfirmationID: conf.ConfirmationID, Fields: fields}}
}

// Reconcile matches a batch of fills against a set of confirmations keyed by
// fill_id and returns the breaks (mismatches) and the fill ids with no
// confirmation at all, both deterministically ordered. A confirmation with no
// fill is reported via Orphans. This is the affirmation pass over a day's trades.
func Reconcile(fills []*orderpb.Fill, confs []Confirmation, tol MatchTolerance) (breaks []Break, unconfirmed []string, orphans []string) {
	byFill := make(map[string]Confirmation, len(confs))
	for _, c := range confs {
		byFill[c.FillID] = c
	}
	seen := make(map[string]bool, len(fills))
	for _, f := range fills {
		seen[f.GetFillId()] = true
		conf, ok := byFill[f.GetFillId()]
		if !ok {
			unconfirmed = append(unconfirmed, f.GetFillId())
			continue
		}
		if res := MatchFill(f, conf, tol); res.Break != nil {
			breaks = append(breaks, *res.Break)
		}
	}
	for _, c := range confs {
		if !seen[c.FillID] {
			orphans = append(orphans, c.ConfirmationID)
		}
	}
	sort.Slice(breaks, func(i, j int) bool { return breaks[i].FillID < breaks[j].FillID })
	sort.Strings(unconfirmed)
	sort.Strings(orphans)
	return breaks, unconfirmed, orphans
}

// beyond reports whether |a − b| exceeds tolerance (a nil/zero tolerance requires
// an exact match).
func beyond(a, b, tolerance *big.Rat) bool {
	if tolerance == nil {
		tolerance = new(big.Rat)
	}
	diff := new(big.Rat).Abs(new(big.Rat).Sub(a, b))
	return diff.Cmp(tolerance) > 0
}
