package binance

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/dec"
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
	account string
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
	// Account is the exchange account the REST credential belongs to.
	Account string
}

// NewBinanceVenue builds the venue. MIC defaults to "BINANCE".
func NewBinanceVenue(cfg BinanceConfig) *BinanceVenue {
	if cfg.MIC == "" {
		cfg.MIC = "BINANCE"
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &BinanceVenue{mic: cfg.MIC, account: cfg.Account, rest: cfg.REST, symbols: cfg.Symbols, now: cfg.Now}
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
	return NewBinanceVenue(BinanceConfig{MIC: s.MIC, Account: s.Account, Symbols: StaticSymbolMap(s.Symbols), REST: rest})
}

// MIC returns the venue code.
func (v *BinanceVenue) MIC() string { return v.mic }

// Account is the exchange account this adapter's API credential belongs to — the
// collateral pool every fill it produces settles against.
func (v *BinanceVenue) Account() string { return v.account }

// Instruments reports every pair this adapter is configured to trade at Binance,
// which is what a picker offers and what an operator reads to answer "what can
// this deployment actually trade?" (#406).
//
// It is the CONFIGURED set — this adapter's own symbol map — never the
// exchange's catalogue. A pair this deployment holds no mapping for cannot be
// routed, and offering it would produce a refusal at admission that an operator
// reads as a platform fault.
func (v *BinanceVenue) Instruments() []InstrumentSymbol {
	if v.symbols == nil {
		return nil
	}
	return v.symbols.Instruments()
}

// OrderTypes declares what this connector can translate for binance, which the OMS
// reads through Describe and enforces at ADMISSION (#405).
//
// ALL FOUR SPOT ORDER TYPES, deliberately the exact set the switch in
// orderParams accepts (#405).
//
// STOP and STOP_LIMIT joined MARKET and LIMIT once OrderState carried
// stop_price: the trigger was validated at admission and then discarded, so
// there was nothing for this connector to send and declaring the capability
// would have been a lie. They map to Binance STOP_LOSS and STOP_LOSS_LIMIT on
// the SAME endpoint as the other two, so cancel, query and reconciliation —
// which all address an order by our deterministic clientOrderId — are unchanged
// by this.
//
// WHAT IS NOT EXPRESSIBLE, said here because the absence is otherwise invisible:
// Binance's TAKE_PROFIT / TAKE_PROFIT_LIMIT are the mirror instruction — trigger
// when the price moves IN FAVOUR — and order.v1 has no order type for them. A
// stop is never silently mapped to one. Binance itself refuses a stop whose
// trigger sits on the wrong side of the market, which is the loud failure that
// case deserves.
//
// KEEP THIS IN STEP WITH THAT SWITCH. TestDeclaredOrderTypesMatchTranslation
// walks the whole enum and fails if the two ever disagree, in either direction.
func (v *BinanceVenue) OrderTypes() []orderpb.OrderType {
	return []orderpb.OrderType{
		orderpb.OrderType_ORDER_TYPE_MARKET,
		orderpb.OrderType_ORDER_TYPE_LIMIT,
		orderpb.OrderType_ORDER_TYPE_STOP,
		orderpb.OrderType_ORDER_TYPE_STOP_LIMIT,
	}
}

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
	return v.fills(resp, st)
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
		tif, err := binanceTimeInForce(st.GetTimeInForce())
		if err != nil {
			return nil, err
		}
		p.Set("type", "LIMIT")
		p.Set("timeInForce", tif)
		p.Set("price", formatDec(st.GetLimitPrice()))
	case orderpb.OrderType_ORDER_TYPE_STOP:
		// STOP_LOSS: becomes a MARKET order once stopPrice trades, which is what
		// order.v1's ORDER_TYPE_STOP means. No price, no timeInForce — Binance
		// rejects both on this type.
		if dec.IsZero(st.GetStopPrice()) {
			return nil, errors.New("binance: stop order requires a positive stop price")
		}
		p.Set("type", "STOP_LOSS")
		p.Set("stopPrice", formatDec(st.GetStopPrice()))
	case orderpb.OrderType_ORDER_TYPE_STOP_LIMIT:
		// STOP_LOSS_LIMIT: becomes a LIMIT order at price once stopPrice trades.
		if dec.IsZero(st.GetStopPrice()) {
			return nil, errors.New("binance: stop-limit order requires a positive stop price")
		}
		if dec.IsZero(st.GetLimitPrice()) {
			return nil, errors.New("binance: stop-limit order requires a positive limit price")
		}
		tif, err := binanceTimeInForce(st.GetTimeInForce())
		if err != nil {
			return nil, err
		}
		p.Set("type", "STOP_LOSS_LIMIT")
		p.Set("stopPrice", formatDec(st.GetStopPrice()))
		p.Set("price", formatDec(st.GetLimitPrice()))
		p.Set("timeInForce", tif)
	default:
		return nil, fmt.Errorf("binance: unsupported order type %v", st.GetOrderType())
	}
	return p, nil
}

// binanceTimeInForce maps order.v1.TimeInForce onto Binance Spot's, and REFUSES
// the ones it cannot express.
//
// # The silent substitution this replaces (#405 family)
//
// Every LIMIT order this connector has ever placed was sent with a hard-coded
// timeInForce=GTC, whatever the trader asked for. That is the fourth instance of
// the defect #240 and #405 document — "a field the perimeter accepts and the wire
// never carries" — and it is the worst of the four, because the other three
// produced an order that did nothing while this one produces an order that does
// the WRONG THING:
//
//   - IOC means "fill what is available right now and cancel the rest". Sent as
//     GTC it RESTS at the exchange instead, so a trader who asked not to hold
//     exposure is holding it, indefinitely, and nothing anywhere says so.
//   - FOK means "fill all of it or none". Sent as GTC it can rest partially
//     filled — the single outcome that instruction exists to forbid.
//
// # DAY and GTD are refused rather than approximated
//
// Binance Spot has no session concept, so DAY has no meaning there, and it has no
// good-til-date parameter at all. Both are REFUSED here.
//
// A refusal at the connector is not where this belongs — it is an order the OMS
// admitted, stored and announced, which is exactly the shape #405 exists to end,
// one field over. Admission gates on order TYPE (the venue declares what it can
// place, via OrderTypeDeclarer) and has no equivalent for time-in-force. That gap
// is filed; until it closes, refusing loudly here is strictly better than the
// silent substitution above, because a wrong instruction that executes is worse
// than a right one that does not.
func binanceTimeInForce(tif orderpb.TimeInForce) (string, error) {
	switch tif {
	case orderpb.TimeInForce_TIME_IN_FORCE_GTC,
		// UNSPECIFIED is treated as GTC deliberately, and only here. Admission
		// requires time_in_force, so an order reaching this connector without one
		// came from a path that predates that rule; GTC is what such an order was
		// already being sent as, so this is the one substitution that changes no
		// existing behaviour.
		orderpb.TimeInForce_TIME_IN_FORCE_UNSPECIFIED:
		return "GTC", nil
	case orderpb.TimeInForce_TIME_IN_FORCE_IOC:
		return "IOC", nil
	case orderpb.TimeInForce_TIME_IN_FORCE_FOK:
		return "FOK", nil
	default:
		return "", fmt.Errorf("binance: spot cannot express time-in-force %v — it has no trading "+
			"session (DAY) and no good-til-date parameter (GTD); placing this as GTC would rest "+
			"an order the trader asked to expire", tif)
	}
}

// fills converts a Binance order response's fills into order.v1.Fills.
//
// The error return is for the Decimal conversions (#94). An empty slice already
// means "this order filled nothing", so it cannot also mean "a fill could not be
// read" — reporting an unreadable execution as no execution is how a real trade
// goes unbooked while the request looks successful.
func (v *BinanceVenue) fills(resp *orderResponse, st *orderpb.OrderState) ([]*orderpb.Fill, error) {
	out := make([]*orderpb.Fill, 0, len(resp.Fills))
	for _, f := range resp.Fills {
		qty, qok := parseDec(f.Qty)
		px, pok := parseDec(f.Price)
		if !qok || !pok {
			return nil, fmt.Errorf("binance: order %s fill %d is not representable as a Decimal (qty=%q price=%q)",
				st.GetOrderId(), f.TradeID, f.Qty, f.Price)
		}
		feeMoney, feeOK := fee(f)
		if !feeOK {
			return nil, fmt.Errorf("binance: order %s fill %d commission %q %s is not representable as a Decimal",
				st.GetOrderId(), f.TradeID, f.Commission, f.CommissionAsset)
		}
		out = append(out, &orderpb.Fill{
			FillId:           fmt.Sprintf("%s-%d", resp.Symbol, f.TradeID),
			OrderId:          st.GetOrderId(),
			InstrumentId:     st.GetInstrumentId(),
			Side:             st.GetSide(),
			Quantity:         qty,
			Price:            px,
			Fee:              feeMoney,
			Venue:            v.mic,
			VenueExecutionId: strconv.FormatInt(f.TradeID, 10),
			ExecutedAt:       timestamppb.New(v.now().UTC()),
		})
	}
	return out, nil
}

// fee reads the commission on one fill. nil Money means NO FEE, so ok=false is a
// separate answer for "there is a commission and it could not be read" (#94).
func fee(f orderFill) (*commonpb.Money, bool) {
	if f.Commission == "" {
		return nil, true
	}
	amt, ok := parseDec(f.Commission)
	if !ok {
		return nil, false
	}
	return &commonpb.Money{Amount: amt, CurrencyCode: f.CommissionAsset}, true
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

// TimeInForce declares which time-in-force instructions this connector can
// express, and it is the exact set the translation switch accepts (#486).
//
// GTC, IOC AND FOK ONLY. Binance Spot has no trading session, so DAY has no meaning
// there, and no good-til-date parameter at all — both are REFUSED rather than
// approximated, because sending a resting order for one somebody asked to expire
// is the silent substitution this whole change exists to end.
//
// Declaring them is what moves that refusal from the venue to ADMISSION, where
// an order that can never be placed is rejected before it is stored and
// announced. That is what supported_order_types did for #405; this is the same
// gate one field over.
//
// KEEP THIS IN STEP WITH THE TRANSLATION. TestDeclaredTimeInForceMatchesTranslation
// walks the whole enum and fails if the two ever disagree, in either direction.
func (v *BinanceVenue) TimeInForce() []orderpb.TimeInForce {
	return []orderpb.TimeInForce{
		orderpb.TimeInForce_TIME_IN_FORCE_GTC,
		orderpb.TimeInForce_TIME_IN_FORCE_IOC,
		orderpb.TimeInForce_TIME_IN_FORCE_FOK,
	}
}
