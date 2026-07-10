//go:build okx

package execution

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/kanz-eng/kanz/services/oms/internal/dec"
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
	rest    *okxREST
	symbols SymbolMapper
	now     func() time.Time
}

// NewOKXVenueFromSettings assembles the rate bucket + signed REST client + venue.
func NewOKXVenueFromSettings(s VenueSettings) *OKXVenue {
	budget := s.WeightBudget
	if budget <= 0 {
		budget = 60 // OKX place-order default: 60 requests / 2s per instrument family
	}
	bucket := newWeightBucket(budget, 2*time.Second, nil)
	rest := newOKXREST(okxRestConfig{
		BaseURL: s.BaseURL, APIKey: s.APIKey, APISecret: s.APISecret, Passphrase: s.Passphrase,
		Bucket: bucket, OnThrottle: s.OnThrottle,
		HTTPClient: newExchangeHTTPClient(s.DNSTTL),
	})
	mic := s.MIC
	if mic == "" {
		mic = "OKX"
	}
	return &OKXVenue{mic: mic, rest: rest, symbols: StaticSymbolMap(s.Symbols), now: time.Now}
}

// MIC returns the venue code.
func (v *OKXVenue) MIC() string { return v.mic }

var _ Venue = (*OKXVenue)(nil)

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
			return v.fills(o, st, instID), nil
		}
		return nil, err
	}
	// Placed OK — query for the true fill state (OKX returns no inline fills).
	o, err := v.rest.queryOrder(ctx, instID, st.GetOrderId())
	if err != nil {
		return nil, nil // accepted but not yet queryable — treat as resting; the
		// user-data stream / reconciliation heals the fill (never fabricate).
	}
	return v.fills(o, st, instID), nil
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
		"sz":      formatDec(st.GetOrderedQuantity()),
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
		body["px"] = formatDec(st.GetLimitPrice())
	default:
		return nil, fmt.Errorf("okx: unsupported order type %v (spot market/limit only)", st.GetOrderType())
	}
	return body, nil
}

// fills builds the aggregate fill from an OKX order's cumulative filled size and
// average price. A zero accFillSz (a resting limit) yields no fills.
func (v *OKXVenue) fills(o *okxOrder, st *orderpb.OrderState, instID string) []*orderpb.Fill {
	acc, ok := new(big.Rat).SetString(o.AccFillSz)
	if !ok || acc.Sign() <= 0 {
		return nil
	}
	execID := o.OrdID
	if execID == "" {
		execID = st.GetOrderId()
	}
	return []*orderpb.Fill{{
		FillId:           instID + "-" + execID,
		OrderId:          st.GetOrderId(),
		InstrumentId:     st.GetInstrumentId(),
		Side:             st.GetSide(),
		Quantity:         parseDec(o.AccFillSz),
		Price:            parseDec(o.AvgPx),
		Fee:              okxFee(o),
		Venue:            v.mic,
		VenueExecutionId: execID,
		ExecutedAt:       timestamppb.New(v.now().UTC()),
	}}
}

func okxFee(o *okxOrder) *commonpb.Money {
	if o.Fee == "" || o.Fee == "0" {
		return nil
	}
	// OKX reports fee as a negative number when charged; store its magnitude.
	amt := parseDec(o.Fee)
	if r := dec.FromProto(amt); r.Sign() < 0 {
		amt = dec.ToProto(r.Neg(r))
	}
	return &commonpb.Money{Amount: amt, CurrencyCode: o.FeeCcy}
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
