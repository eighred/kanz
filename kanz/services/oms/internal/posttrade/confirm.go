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
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dec"
)

// Confirmation is a counterparty's trade confirmation — the working shape behind
// settlement.v1.TradeConfirmation. Its terms are matched against the OMS fill.
type Confirmation struct {
	ConfirmationID string
	FillID         string
	VenueAccountID string
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
// whether their account, fill identity and terms agree within tolerance. Missing
// required evidence is a break. This compares one pair; batch callers must use
// Reconcile so conflicting confirmations cannot be hidden by pair selection.
func MatchFill(fill *orderpb.Fill, conf Confirmation, tol MatchTolerance) MatchResult {
	var fields []string
	if strings.TrimSpace(conf.ConfirmationID) == "" {
		fields = append(fields, "confirmation_id")
	}
	if strings.TrimSpace(fill.GetFillId()) == "" || fill.GetFillId() != conf.FillID {
		fields = append(fields, "fill_id")
	}
	if strings.TrimSpace(fill.GetVenueAccountId()) == "" || fill.GetVenueAccountId() != conf.VenueAccountID {
		fields = append(fields, "venue_account_id")
	}
	if strings.TrimSpace(conf.Counterparty) == "" {
		fields = append(fields, "counterparty")
	}
	if conf.SettlementDate.IsZero() {
		fields = append(fields, "settlement_date")
	}
	if strings.TrimSpace(fill.GetInstrumentId()) == "" || fill.GetInstrumentId() != conf.InstrumentID {
		fields = append(fields, "instrument_id")
	}
	if (fill.GetSide() != orderpb.Side_SIDE_BUY && fill.GetSide() != orderpb.Side_SIDE_SELL) || fill.GetSide() != conf.Side {
		fields = append(fields, "side")
	}
	quantity, quantityOK := dec.FromProtoChecked(fill.GetQuantity())
	if fill.GetQuantity() == nil || !quantityOK || quantity.Sign() <= 0 || conf.Quantity == nil || conf.Quantity.Sign() <= 0 || beyond(quantity, conf.Quantity, tol.Quantity) {
		fields = append(fields, "quantity")
	}
	price, priceOK := dec.FromProtoChecked(fill.GetPrice())
	if fill.GetPrice() == nil || !priceOK || conf.Price == nil || beyond(price, conf.Price, tol.Price) {
		fields = append(fields, "price")
	}
	if len(fields) == 0 {
		return MatchResult{Matched: true}
	}
	return MatchResult{Break: &Break{FillID: fill.GetFillId(), ConfirmationID: conf.ConfirmationID, Fields: fields}}
}

// Reconcile matches a batch of fills against a set of confirmations keyed by
// fill_id and returns the breaks (mismatches) and the fill ids with no
// confirmation at all, both deterministically ordered. Exact redeliveries are
// collapsed; distinct evidence for one fill disputes every candidate. The caller
// must supply one authenticated tenant's evidence and retain it for resolution.
// A confirmation with no fill is reported via orphans.
func Reconcile(fills []*orderpb.Fill, confs []Confirmation, tol MatchTolerance) (breaks []Break, unconfirmed []string, orphans []string) {
	// This is a single-tenant evidence set supplied by the authenticated caller.
	// There is no source-authorized amendment protocol in this contract: a second
	// distinct confirmation is a dispute, never an implicit supersession. Keep
	// all evidence so the desk can resolve it before any instruction is emitted.
	byFill := make(map[string][]Confirmation, len(confs))
	type identity struct{ counterparty, id string }
	identities := make(map[identity]Confirmation, len(confs))
	identityConflict := make(map[identity]bool)
	deliveries := make(map[confirmationKey]bool, len(confs))
	for _, c := range confs {
		key := identity{c.Counterparty, c.ConfirmationID}
		if previous, ok := identities[key]; ok && !sameConfirmation(previous, c) {
			identityConflict[key] = true
		}
		identities[key] = c
		delivery := confirmationEvidenceKey(c)
		if !deliveries[delivery] {
			byFill[c.FillID] = append(byFill[c.FillID], c)
			deliveries[delivery] = true
		}
	}
	fillEvidence := make(map[string]*orderpb.Fill, len(fills))
	fillConflict := make(map[string]bool)
	for _, f := range fills {
		if previous, ok := fillEvidence[f.GetFillId()]; ok && !proto.Equal(previous, f) {
			fillConflict[f.GetFillId()] = true
		}
		fillEvidence[f.GetFillId()] = f
	}
	seen := make(map[string]bool, len(fills))
	for _, f := range fills {
		if seen[f.GetFillId()] {
			continue
		}
		seen[f.GetFillId()] = true
		candidates, ok := byFill[f.GetFillId()]
		if !ok {
			unconfirmed = append(unconfirmed, f.GetFillId())
			continue
		}
		for _, conf := range candidates {
			switch {
			case fillConflict[f.GetFillId()]:
				breaks = append(breaks, Break{FillID: f.GetFillId(), ConfirmationID: conf.ConfirmationID, Fields: []string{"conflicting_fills"}})
			case len(candidates) > 1 || identityConflict[identity{conf.Counterparty, conf.ConfirmationID}]:
				breaks = append(breaks, Break{FillID: f.GetFillId(), ConfirmationID: conf.ConfirmationID, Fields: []string{"conflicting_confirmations"}})
			default:
				if res := MatchFill(f, conf, tol); res.Break != nil {
					breaks = append(breaks, *res.Break)
				}
			}
		}
	}
	for fillID, candidates := range byFill {
		if !seen[fillID] {
			for _, c := range candidates {
				orphans = append(orphans, c.ConfirmationID)
			}
		}
	}
	sort.Slice(breaks, func(i, j int) bool {
		if breaks[i].FillID != breaks[j].FillID {
			return breaks[i].FillID < breaks[j].FillID
		}
		return breaks[i].ConfirmationID < breaks[j].ConfirmationID
	})
	sort.Strings(unconfirmed)
	sort.Strings(orphans)
	return breaks, unconfirmed, orphans
}

// A comparable semantic key avoids quadratic scans when a broken source sends
// many versions for one fill. Rationals are normalized; location and monotonic
// clock metadata are not economic differences in a settlement timestamp.
type confirmationKey struct {
	id, fill, account, instrument, counterparty, quantity, price string
	side                                                         orderpb.Side
	date                                                         time.Time
}

func confirmationEvidenceKey(c Confirmation) confirmationKey {
	return confirmationKey{c.ConfirmationID, c.FillID, c.VenueAccountID, c.InstrumentID,
		c.Counterparty, ratKey(c.Quantity), ratKey(c.Price), c.Side, c.SettlementDate.Round(0).UTC()}
}

func ratKey(r *big.Rat) string {
	if r == nil {
		return "missing"
	}
	return r.RatString()
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

func sameConfirmation(a, b Confirmation) bool {
	return a.ConfirmationID == b.ConfirmationID && a.FillID == b.FillID &&
		a.VenueAccountID == b.VenueAccountID && a.InstrumentID == b.InstrumentID &&
		a.Side == b.Side && equalRat(a.Quantity, b.Quantity) && equalRat(a.Price, b.Price) &&
		a.Counterparty == b.Counterparty && a.SettlementDate.Equal(b.SettlementDate)
}

func equalRat(a, b *big.Rat) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Cmp(b) == 0
}
