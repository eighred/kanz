//go:build binance

package execution

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/kanz-eng/kanz/services/oms/internal/dec"
)

// BinanceVenue implements the execution.Venue seam against Binance Spot (compiled
// only under -tags binance). It maps a SubmitOrder-derived OrderState onto a
// signed Binance new-order, stamping our deterministic order_id as the
// newClientOrderId so a retried/timed-out placement resolves to the ORIGINAL
// order rather than double-executing. Immediate fills (MARKET, marketable LIMIT)
// come back inline and are returned to the router; a resting LIMIT returns no
// fills here and is filled asynchronously by the user-data stream (Phase 3.2).
type BinanceVenue struct {
	mic     string
	rest    *binanceREST
	symbols SymbolMapper
	now     func() time.Time
}

// BinanceConfig configures a BinanceVenue.
type BinanceConfig struct {
	MIC     string // venue code stamped on routing + fills (e.g. "BINANCE")
	Symbols SymbolMapper
	REST    *binanceREST
	Now     func() time.Time
}

// NewBinanceVenue builds the venue. MIC defaults to "BINANCE".
func NewBinanceVenue(cfg BinanceConfig) *BinanceVenue {
	if cfg.MIC == "" {
		cfg.MIC = "BINANCE"
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &BinanceVenue{mic: cfg.MIC, rest: cfg.REST, symbols: cfg.Symbols, now: cfg.Now}
}

// NewBinanceVenueFromSettings assembles the rate bucket + signed REST client +
// venue from public settings — the composition-root entry point.
func NewBinanceVenueFromSettings(s VenueSettings) *BinanceVenue {
	budget := s.WeightBudget
	if budget <= 0 {
		budget = 1200 // Binance Spot default request-weight budget per minute
	}
	bucket := newWeightBucket(budget, time.Minute, nil)
	rest := newBinanceREST(restConfig{
		BaseURL: s.BaseURL, APIKey: s.APIKey, APISecret: s.APISecret,
		Bucket: bucket, OnThrottle: s.OnThrottle,
		HTTPClient: newExchangeHTTPClient(s.DNSTTL), // DNS-bypass dialer on the hot path
	})
	return NewBinanceVenue(BinanceConfig{MIC: s.MIC, Symbols: StaticSymbolMap(s.Symbols), REST: rest})
}

// MIC returns the venue code.
func (v *BinanceVenue) MIC() string { return v.mic }

var _ Venue = (*BinanceVenue)(nil)

// Execute places st on Binance and returns the immediate fills. On any ambiguous
// placement failure it recovers the true order state via the deterministic
// clientOrderId (idempotency) — it NEVER fabricates a fill: a genuinely
// unplaced/rejected order returns the error and the order rests or is refused.
func (v *BinanceVenue) Execute(ctx context.Context, st *orderpb.OrderState) ([]*orderpb.Fill, error) {
	if st == nil {
		return nil, errors.New("binance: nil order state")
	}
	symbol, ok := v.symbols.Symbol(st.GetInstrumentId())
	if !ok {
		return nil, fmt.Errorf("binance: no symbol mapping for %s", st.GetInstrumentId())
	}
	params, err := orderParams(st, symbol)
	if err != nil {
		return nil, err
	}

	resp, err := v.rest.newOrder(ctx, params)
	if err != nil {
		// Idempotency recovery: an ambiguous failure (timeout, duplicate) may
		// mean the order actually landed. Query by our deterministic
		// clientOrderId; if it exists, adopt its true state, else surface the
		// original error — never invent a fill.
		if errors.Is(err, ErrRateLimited) {
			return nil, err // budget exhausted — order rests; caller backs off + alerts
		}
		q, qErr := v.rest.queryOrder(ctx, symbol, st.GetOrderId())
		if qErr != nil {
			return nil, err
		}
		resp = q
	}
	return v.fills(resp, st), nil
}

// binanceUnknownOrder is Binance's -2011 "Unknown order sent". For a CANCEL this
// is a confirmed withdrawal, not a failure: the order is not working at the venue
// (already filled, expired, or withdrawn by an earlier attempt), so the close has
// landed and nothing is left in flight. Surfacing it as an error would send an
// already-closed order to the healing watchdog.
const binanceUnknownOrder = -2011

// CancelOrder withdraws a working order at Binance, satisfying the Closer seam.
// It addresses the order by our deterministic clientOrderId, so a retried cancel
// resolves to the original order. Any other error is surfaced verbatim — an
// ambiguous timeout must NEVER be read as a successful cancel; the OMS leaves the
// close in flight and the healing watchdog resolves it against venue truth.
func (v *BinanceVenue) CancelOrder(ctx context.Context, st *orderpb.OrderState) error {
	if st == nil {
		return errors.New("binance: nil order state")
	}
	symbol, ok := v.symbols.Symbol(st.GetInstrumentId())
	if !ok {
		return fmt.Errorf("binance: no symbol mapping for %s", st.GetInstrumentId())
	}
	_, err := v.rest.cancelOrder(ctx, symbol, st.GetOrderId())
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Code == binanceUnknownOrder {
		return nil
	}
	return err
}

var _ Closer = (*BinanceVenue)(nil)

// orderParams maps an OrderState onto Binance Spot new-order parameters,
// stamping newClientOrderId = order_id.
func orderParams(st *orderpb.OrderState, symbol string) (url.Values, error) {
	side, err := binanceSide(st.GetSide())
	if err != nil {
		return nil, err
	}
	p := url.Values{
		"symbol":           {symbol},
		"side":             {side},
		"newClientOrderId": {st.GetOrderId()}, // deterministic ⇒ exchange-side idempotency
		"quantity":         {formatDec(st.GetOrderedQuantity())},
	}
	switch st.GetOrderType() {
	case orderpb.OrderType_ORDER_TYPE_MARKET:
		p.Set("type", "MARKET")
	case orderpb.OrderType_ORDER_TYPE_LIMIT:
		if dec.IsZero(st.GetLimitPrice()) {
			return nil, errors.New("binance: limit order requires a positive limit price")
		}
		p.Set("type", "LIMIT")
		p.Set("timeInForce", "GTC")
		p.Set("price", formatDec(st.GetLimitPrice()))
	default:
		// Spot-first: STOP/STOP_LIMIT map to Binance STOP_LOSS_LIMIT later; a
		// perps structural layout follows. Reject cleanly for now.
		return nil, fmt.Errorf("binance: unsupported order type %v (spot MARKET/LIMIT only)", st.GetOrderType())
	}
	return p, nil
}

// fills converts a Binance order response's fills into order.v1.Fills.
func (v *BinanceVenue) fills(resp *orderResponse, st *orderpb.OrderState) []*orderpb.Fill {
	out := make([]*orderpb.Fill, 0, len(resp.Fills))
	for _, f := range resp.Fills {
		out = append(out, &orderpb.Fill{
			FillId:           fmt.Sprintf("%s-%d", resp.Symbol, f.TradeID),
			OrderId:          st.GetOrderId(),
			InstrumentId:     st.GetInstrumentId(),
			Side:             st.GetSide(),
			Quantity:         parseDec(f.Qty),
			Price:            parseDec(f.Price),
			Fee:              fee(f),
			Venue:            v.mic,
			VenueExecutionId: strconv.FormatInt(f.TradeID, 10),
			ExecutedAt:       timestamppb.New(v.now().UTC()),
		})
	}
	return out
}

func fee(f orderFill) *commonpb.Money {
	if f.Commission == "" {
		return nil
	}
	return &commonpb.Money{Amount: parseDec(f.Commission), CurrencyCode: f.CommissionAsset}
}

func binanceSide(s orderpb.Side) (string, error) {
	switch s {
	case orderpb.Side_SIDE_BUY:
		return "BUY", nil
	case orderpb.Side_SIDE_SELL:
		return "SELL", nil
	default:
		return "", fmt.Errorf("binance: invalid side %v", s)
	}
}
