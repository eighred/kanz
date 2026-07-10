//go:build binance

package execution

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"strconv"
	"time"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/kanz-eng/kanz/services/oms/internal/dec"
)

// SymbolMapper maps a canonical Kanz instrument_id to the exchange's symbol
// (e.g. "BTC-USD" -> "BTCUSDT"). An unmapped instrument cannot be traded here.
type SymbolMapper interface {
	Symbol(instrumentID string) (string, bool)
}

// StaticSymbolMap is a fixed instrument→symbol map.
type StaticSymbolMap map[string]string

func (m StaticSymbolMap) Symbol(id string) (string, bool) { s, ok := m[id]; return s, ok }

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

// VenueSettings is the public configuration the composition root supplies to
// build a BinanceVenue without touching the internal REST client / rate bucket.
type VenueSettings struct {
	MIC          string
	BaseURL      string // e.g. https://testnet.binance.vision
	APIKey       string
	APISecret    string
	Symbols      map[string]string // instrument_id -> exchange symbol
	WeightBudget int               // per-minute REST weight budget (default 1200)
	OnThrottle   func()            // structural-alert hook on budget exhaustion
	DNSTTL       time.Duration     // DNS cache TTL for the bypass dialer (default 5m)
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
		HTTPClient: newBinanceHTTPClient(s.DNSTTL), // DNS-bypass dialer on the hot path
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

// formatDec renders a common.v1.Decimal as a plain string for the Binance API.
// NOTE: exchange LOT_SIZE/PRICE_FILTER precision rounding (from exchangeInfo) is
// a hardening item — testnet tolerates unrounded values; production binds the
// symbol filters before send.
func formatDec(d *commonpb.Decimal) string {
	r := dec.FromProto(d)
	s := r.FloatString(8)
	// trim trailing zeros / dangling point
	for len(s) > 0 && s[len(s)-1] == '0' {
		s = s[:len(s)-1]
	}
	if len(s) > 0 && s[len(s)-1] == '.' {
		s = s[:len(s)-1]
	}
	if s == "" {
		s = "0"
	}
	return s
}

// parseDec parses a Binance decimal string into an exact common.v1.Decimal.
func parseDec(s string) *commonpb.Decimal {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return &commonpb.Decimal{}
	}
	return dec.ToProto(r)
}
