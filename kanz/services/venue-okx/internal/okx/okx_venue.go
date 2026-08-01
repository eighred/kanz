package okx

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/venueadapter/exchangeauth"
)

// OKXVenue implements the execution.Venue seam against OKX v5 spot (compiled
// only under -tags okx). It maps a SubmitOrder-derived OrderState onto a signed
// OKX place-order, stamping our deterministic order_id as clOrdId for
// exchange-side idempotency. OKX place-order does not return fills inline, so
// Execute queries the order for its true fill state; a filled/partial order
// returns the aggregate fill, a resting limit returns none. On any ambiguous
// failure it recovers via query by clOrdId — never a fabricated fill.
type OKXVenue struct {
	mic     string
	account string
	rest    *okxREST
	symbols SymbolMapper
	now     func() time.Time
}

// NewOKXVenueFromSettings assembles the rate bucket + signed REST client + venue.
//
// mode is an explicit ARGUMENT rather than a VenueSettings field because
// VenueSettings lives in internal/execution, which exchangeauth imports — putting
// a typed OKXTradingMode there would be an import cycle, and putting an untyped
// string there would move the "is this a real mode?" check away from the compiler
// and back into a runtime string comparison. Passing it here keeps the type, and
// keeps the decision visible at the composition root where it belongs (#147).
func NewOKXVenueFromSettings(s VenueSettings, mode exchangeauth.OKXTradingMode) *OKXVenue {
	budget := s.WeightBudget
	if budget <= 0 {
		budget = 60 // OKX place-order default: 60 requests / 2s per instrument family
	}
	bucket := NewWeightBucket(budget, 2*time.Second, nil)
	rest := newOKXREST(okxRestConfig{
		BaseURL: s.BaseURL, APIKey: s.APIKey, APISecret: s.APISecret, Passphrase: s.Passphrase,
		Bucket: bucket, OnThrottle: s.OnThrottle,
		HTTPClient: NewExchangeHTTPClient(s.DNSTTL),
		Mode:       mode,
	})
	mic := s.MIC
	if mic == "" {
		mic = "OKX"
	}
	return &OKXVenue{mic: mic, account: s.Account, rest: rest, symbols: StaticSymbolMap(s.Symbols), now: time.Now}
}

// MIC returns the venue code.
func (v *OKXVenue) MIC() string { return v.mic }

// Account is the exchange account this adapter's API credential belongs to — the
// collateral pool every fill it produces settles against.
func (v *OKXVenue) Account() string { return v.account }

var _ Venue = (*OKXVenue)(nil)

// OKX cancel sCodes that mean "the order is not working at the venue": it never
// existed here, was already cancelled, or already completed. For a CANCEL each is
// a CONFIRMED withdrawal, not a failure — the close has landed and nothing is in
// flight. Surfacing them as errors would send an already-closed order to the
// healing watchdog.
const (
	okxCancelOrderNotExist = 51400
	okxCancelAlreadyDone   = 51401
	okxCancelCompleted     = 51402
)

// CancelOrder withdraws a working order at OKX, satisfying the Closer seam. Any
// other error is surfaced verbatim — an ambiguous timeout must NEVER be read as a
// successful cancel; the OMS leaves the close in flight and the healing watchdog
// resolves it against venue truth.
func (v *OKXVenue) CancelOrder(ctx context.Context, st *orderpb.OrderState) error {
	if st == nil {
		return errors.New("okx: nil order state")
	}
	instID, ok := v.symbols.Symbol(st.GetInstrumentId())
	if !ok {
		return fmt.Errorf("okx: no symbol mapping for %s", st.GetInstrumentId())
	}
	_, err := v.rest.cancelOrder(ctx, instID, st.GetOrderId())
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		switch apiErr.Code {
		case okxCancelOrderNotExist, okxCancelAlreadyDone, okxCancelCompleted:
			return nil
		}
	}
	return err
}

var _ Closer = (*OKXVenue)(nil)

// Execute places st on OKX and returns the fills observed by an immediate query.
func (v *OKXVenue) Execute(ctx context.Context, st *orderpb.OrderState) ([]*orderpb.Fill, error) {
	if st == nil {
		return nil, errors.New("okx: nil order state")
	}
	instID, ok := v.symbols.Symbol(st.GetInstrumentId())
	if !ok {
		return nil, fmt.Errorf("okx: no symbol mapping for %s", st.GetInstrumentId())
	}
	body, err := okxOrderBody(st, instID)
	if err != nil {
		return nil, err
	}

	if _, err := v.rest.placeOrder(ctx, body); err != nil {
		if errors.Is(err, ErrRateLimited) {
			return nil, err // budget exhausted — back off + alert
		}
		// Idempotency recovery: a duplicate clOrdId / ambiguous timeout may mean
		// the order landed. Query by clOrdId; if present, adopt its state.
		if o, qErr := v.rest.queryOrder(ctx, instID, st.GetOrderId()); qErr == nil {
			return v.fills(o, st, instID)
		}
		return nil, err
	}
	// Placed OK — query for the true fill state (OKX returns no inline fills).
	o, err := v.rest.queryOrder(ctx, instID, st.GetOrderId())
	if err != nil {
		return nil, nil // accepted but not yet queryable — treat as resting; the
		// user-data stream / reconciliation heals the fill (never fabricate).
	}
	return v.fills(o, st, instID)
}

func okxOrderBody(st *orderpb.OrderState, instID string) (map[string]string, error) {
	side, err := okxSide(st.GetSide())
	if err != nil {
		return nil, err
	}
	body := map[string]string{
		"instId":  instID,
		"tdMode":  "cash", // spot
		"side":    side,
		"clOrdId": st.GetOrderId(), // deterministic ⇒ exchange-side idempotency
		"sz":      FormatDec(st.GetOrderedQuantity()),
	}
	switch st.GetOrderType() {
	case orderpb.OrderType_ORDER_TYPE_MARKET:
		body["ordType"] = "market"
		body["tgtCcy"] = "base_ccy" // size in base units, consistent with our quantity
	case orderpb.OrderType_ORDER_TYPE_LIMIT:
		if dec.IsZero(st.GetLimitPrice()) {
			return nil, errors.New("okx: limit order requires a positive limit price")
		}
		body["ordType"] = "limit"
		body["px"] = FormatDec(st.GetLimitPrice())
	default:
		return nil, fmt.Errorf("okx: unsupported order type %v (spot market/limit only)", st.GetOrderType())
	}
	return body, nil
}

// fills builds the aggregate fill from an OKX order's cumulative filled size and
// average price. A zero accFillSz (a resting limit) yields no fills.
//
// The error return is for the Decimal conversions (#94). A nil slice already
// means "no fill happened", so it cannot also mean "the fill could not be read" —
// reporting an unreadable fill as no fill is how a real execution goes unbooked.
func (v *OKXVenue) fills(o *okxOrder, st *orderpb.OrderState, instID string) ([]*orderpb.Fill, error) {
	acc, ok := new(big.Rat).SetString(o.AccFillSz)
	if !ok || acc.Sign() <= 0 {
		return nil, nil
	}
	execID := o.OrdID
	if execID == "" {
		execID = st.GetOrderId()
	}
	qty, qok := ParseDec(o.AccFillSz)
	px, pok := ParseDec(o.AvgPx)
	if !qok || !pok {
		return nil, fmt.Errorf("okx: order %s fill is not representable as a Decimal (accFillSz=%q avgPx=%q)",
			st.GetOrderId(), o.AccFillSz, o.AvgPx)
	}
	fee, feeOK := okxFee(o)
	if !feeOK {
		return nil, fmt.Errorf("okx: order %s fee %q %s is not representable as a Decimal",
			st.GetOrderId(), o.Fee, o.FeeCcy)
	}
	return []*orderpb.Fill{{
		FillId:           instID + "-" + execID,
		OrderId:          st.GetOrderId(),
		InstrumentId:     st.GetInstrumentId(),
		Side:             st.GetSide(),
		Quantity:         qty,
		Price:            px,
		Fee:              fee,
		Venue:            v.mic,
		VenueExecutionId: execID,
		ExecutedAt:       timestamppb.New(v.now().UTC()),
	}}, nil
}

// okxFee reads the fee OKX charged. A nil Money means NO FEE — so ok=false is a
// separate answer for "there is a fee and it could not be read" (#94). Collapsing
// the two would drop a real cost silently, which understates what the trade cost.
func okxFee(o *okxOrder) (*commonpb.Money, bool) {
	if o.Fee == "" || o.Fee == "0" {
		return nil, true
	}
	amt, ok := ParseDec(o.Fee)
	if !ok {
		return nil, false
	}
	// OKX reports fee as a negative number when charged; store its magnitude.
	if r := dec.FromProto(amt); r.Sign() < 0 {
		neg, nok := dec.ToProtoScaled(r.Neg(r))
		if !nok {
			return nil, false
		}
		amt = neg
	}
	return &commonpb.Money{Amount: amt, CurrencyCode: o.FeeCcy}, true
}

func okxSide(s orderpb.Side) (string, error) {
	switch s {
	case orderpb.Side_SIDE_BUY:
		return "buy", nil
	case orderpb.Side_SIDE_SELL:
		return "sell", nil
	default:
		return "", fmt.Errorf("okx: invalid side %v", s)
	}
}
