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
	"github.com/eighred/kanz/internal/orderid"
	"github.com/eighred/kanz/internal/venueadapter/exchangeauth"
)

// OKXVenue implements the execution.Venue seam against OKX v5 spot (compiled
// only under -tags okx). It maps a SubmitOrder-derived OrderState onto a signed
// OKX place-order, stamping our deterministic order_id as clOrdId for
// exchange-side idempotency. OKX place-order does not return fills inline, so
// Execute queries the order for its true fill state and then, only if it traded,
// for the individual TRADES behind it — one fill per trade, named
// "<instId>-<tradeId>", which is what the user-data stream names them (#923). A
// resting limit returns none. On any ambiguous failure it recovers via query by
// clOrdId — never a fabricated fill.
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
// okxDefaultWeightBudget and okxDefaultWeightWindow are /trade/order's published
// limit: 60 requests per 2 seconds per instrument family. It is the DEFAULT for
// the shared bucket and, before #933, was the single number standing in for every
// endpoint's limit.
const (
	okxDefaultWeightBudget = 60
	okxDefaultWeightWindow = 2 * time.Second
)

func NewOKXVenueFromSettings(s VenueSettings, mode exchangeauth.OKXTradingMode) *OKXVenue {
	budget := s.WeightBudget
	if budget <= 0 {
		budget = okxDefaultWeightBudget
	}
	// ONE BUCKET PER OKX RATE-LIMIT FAMILY (#933), not one bucket for the venue.
	// OKX meters PER ENDPOINT, so fills-history spending placement budget was a
	// self-imposed throttle the venue does not actually impose — see okx_buckets.go
	// for why only the families with a verified limit are split out.
	buckets := newOKXBuckets(NewWeightBucket(budget, okxDefaultWeightWindow, nil), nil)
	rest := newOKXREST(okxRestConfig{
		BaseURL: s.BaseURL, APIKey: s.APIKey, APISecret: s.APISecret, Passphrase: s.Passphrase,
		Buckets: buckets, OnThrottle: s.OnThrottle,
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

// Instruments reports every pair this adapter is configured to trade at OKX,
// which is what a picker offers and what an operator reads to answer "what can
// this deployment actually trade?" (#406).
//
// It is the CONFIGURED set — this adapter's own symbol map — never the
// exchange's catalogue. A pair this deployment holds no mapping for cannot be
// routed, and offering it would produce a refusal at admission that an operator
// reads as a platform fault.
func (v *OKXVenue) Instruments() []InstrumentSymbol {
	if v.symbols == nil {
		return nil
	}
	return v.symbols.Instruments()
}

// ALL FOUR SPOT ORDER TYPES, deliberately the exact set okxOrderBody accepts
// (#405, #485).
//
// STOP and STOP_LIMIT are placed as CONDITIONAL orders on /trade/order-algo —
// a separate product with its own endpoints for placement, query and cancel.
// What makes them workable is that our own id addresses them at every one:
// algoClOrdId is accepted on placement, resolves the order on query without an
// instId, and cancels it with no algoId round trip.
//
// THE TRIGGERED ORDER IS NOT THE ALGO ORDER, and that is the part that had to be
// measured rather than assumed. When a stop fires, OKX creates a regular order
// with a clOrdId OF ITS OWN; ours survives on algoClOrdId, which is why
// okx_userdata.go reads that field first. Declaring these types before that was
// true would have produced stops that rest correctly and whose fills nobody
// could book.
//
// KEEP THIS IN STEP WITH THAT SWITCH. TestDeclaredOrderTypesMatchTranslation
// walks the whole enum and fails if the two ever disagree, in either direction.
func (v *OKXVenue) OrderTypes() []orderpb.OrderType {
	return []orderpb.OrderType{
		orderpb.OrderType_ORDER_TYPE_MARKET,
		orderpb.OrderType_ORDER_TYPE_LIMIT,
		orderpb.OrderType_ORDER_TYPE_STOP,
		orderpb.OrderType_ORDER_TYPE_STOP_LIMIT,
	}
}

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
	// WITHDRAWN FROM THE ENDPOINT IT LIVES ON (#485). A conditional order is not
	// visible to /trade/cancel-order at all — that endpoint would answer "order
	// does not exist", which this connector reads as a CONFIRMED withdrawal, so a
	// stop would be reported cancelled while still resting at the exchange.
	// Derived from the order type rather than remembered, so a cancel arriving
	// minutes later on another pod still reaches the right endpoint.
	cancel := v.rest.cancelOrder
	if isAlgoOrder(st.GetOrderType()) {
		cancel = v.rest.cancelAlgoOrder
	}
	_, err := cancel(ctx, instID, st.GetOrderId())
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
	// AND THE COLLATERAL REGIME THIS CONNECTOR CANNOT EXPRESS (#417). The OMS
	// refuses this at admission from MarginModes() above, so reaching here means
	// the declaration and the wire have drifted — or that something placed an
	// order without going through admission. Both are worth failing loudly for:
	// placing it anyway is a live position whose regime the fund's records get
	// wrong, and no downstream record could tell.
	if m := st.GetMarginMode(); !marginModeSupported(m) {
		return nil, fmt.Errorf("okx: cannot work an order under %v — this connector places "+
			"SPOT (tdMode cash) orders only, so placing it would leave the position unlevered while the "+
			"audit root records margin", m)
	}
	// A STOP GOES TO A DIFFERENT PRODUCT ENTIRELY (#485), and returns no fills:
	// a conditional order RESTS until its trigger fires, and the order that
	// eventually fills is one OKX creates then. Its fills reach this platform
	// through the user-data stream keyed on algoClOrdId, and through
	// reconciliation — never from this call, which is why it returns nil rather
	// than querying for a fill that cannot exist yet.
	if isAlgoOrder(st.GetOrderType()) {
		algoBody, aerr := okxAlgoBody(st, instID)
		if aerr != nil {
			return nil, aerr
		}
		if _, aerr := v.rest.placeAlgoOrder(ctx, algoBody); aerr != nil {
			if errors.Is(aerr, ErrRateLimited) {
				return nil, aerr
			}
			// Idempotency recovery, same shape as the regular path: an ambiguous
			// failure may mean the order landed. Ask by OUR id — no instId needed
			// on this endpoint — and treat a resting stop as placed.
			if _, qErr := v.rest.queryAlgoOrder(ctx, st.GetOrderId()); qErr == nil {
				return nil, nil
			}
			return nil, aerr
		}
		return nil, nil
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
			return v.tradedFills(ctx, o, st, instID)
		}
		return nil, err
	}
	// Placed OK — query for the true fill state (OKX returns no inline fills),
	// then, only if it traded, for the individual trades behind it (#923).
	o, err := v.rest.queryOrder(ctx, instID, st.GetOrderId())
	if err != nil {
		return nil, nil // accepted but not yet queryable — treat as resting; the
		// user-data stream / reconciliation heals the fill (never fabricate).
	}
	return v.tradedFills(ctx, o, st, instID)
}

func okxOrderBody(st *orderpb.OrderState, instID string) (map[string]string, error) {
	side, err := okxSide(st.GetSide())
	if err != nil {
		return nil, err
	}
	// THE ORDER ID IS THE VENUE'S CLIENT ORDER ID, AND OKX IS THE STRICTEST JUDGE
	// OF IT: letters and digits only, at most 32 characters. An id that breaks
	// either rule comes back as `51000 Parameter clOrdId error`, which tells an
	// operator nothing about which of their 36 characters was the problem.
	//
	// Refusing here names the rule. It is a backstop rather than the fix — the
	// gateway now MINTS ids that satisfy it (internal/orderid) — but a client may
	// supply its own order id, and this is the only place that knows OKX's rule.
	if err := orderid.Valid(st.GetOrderId()); err != nil {
		return nil, fmt.Errorf("okx: %w", err)
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
		ordType, terr := okxLimitOrdType(st.GetTimeInForce())
		if terr != nil {
			return nil, terr
		}
		body["ordType"] = ordType
		body["px"] = FormatDec(st.GetLimitPrice())
	default:
		return nil, fmt.Errorf("okx: unsupported order type %v", st.GetOrderType())
	}
	return body, nil
}

// tradedFills reports the EXECUTIONS behind an order OKX has already answered
// about — one fill per trade, never a synthesized aggregate.
//
// # Why the order response is not enough (#923)
//
// GET /api/v5/trade/order reports accFillSz and avgPx and no trade list; OKX
// returns no inline fills on the POST either. Synthesizing one cumulative fill
// from those two numbers was the obvious shortcut and it is unsafe in both
// directions, because a fill's IDENTITY is what the order aggregate
// (order_fills) and the position book (position_fills) dedup on:
//
//   - "<instId>-<ordId>" is a STABLE NAME OVER A GROWING QUANTITY. An order that
//     was 40% done when its aggregate fill was folded comes back from a later
//     query as the same id carrying 100. The OMS skips a fill_id it already
//     holds — correctly; that is what stops a double count — so the order stayed
//     recorded at 40% forever, with nothing able to notice.
//   - ordId AND tradeId ARE DIFFERENT OKX IDENTIFIER SPACES, and the private
//     user-data stream names its fills "<instId>-<tradeId>". So one execution
//     arrived under two names, and folding both double-counts it — or overfills
//     past leaves and freezes the order in quarantine.
//
// So the real trades are fetched and built by the SAME v.fills the query path
// uses, and it mints the id the websocket mints. This is the Binance connector's
// shape (binance_query.go's tradesFor), one venue over.
func (v *OKXVenue) tradedFills(ctx context.Context, o *okxOrder, st *orderpb.OrderState, instID string) ([]*orderpb.Fill, error) {
	acc, ok := new(big.Rat).SetString(o.AccFillSz)
	if !ok || acc.Sign() <= 0 {
		// NOTHING TRADED, so nothing is asked. A resting limit is the common case
		// and it must not spend six units of the weight budget to learn that.
		return nil, nil
	}
	if o.OrdID == "" {
		// OKX CONTRADICTED ITSELF: it says this order traded and did not name the
		// order it traded on. fills-history has no clOrdId form, so there is no
		// second way to ask. A nil slice here would report a real execution as no
		// execution, which is the exact failure #94's error return exists for.
		return nil, fmt.Errorf("okx: order %s traded %s but OKX named no ordId for it, so this "+
			"adapter cannot read which trades made it up", st.GetOrderId(), o.AccFillSz)
	}
	trades, err := v.rest.fillsHistory(ctx, instID, o.OrdID)
	if err != nil {
		// Including ErrRateLimited: the question could not be finished, and a
		// half-read set of fills is worse than no answer at all.
		return nil, err
	}
	return v.fills(trades, st, instID)
}

// fills converts OKX's own execution records into order.v1.Fills — ONE PER
// TRADE, named "<instId>-<tradeId>", byte for byte what okx_userdata.go mints
// for the same trade off the private websocket.
//
// IT IS THE ONE PLACE AN OKX FILL IS CONSTRUCTED on the synchronous path, so the
// placement path and the query path cannot drift apart into two id spaces the
// way the placement path and the user-data stream did (#923).
//
// The error return is for the Decimal conversions (#94). An empty slice already
// means "this order filled nothing", so it cannot also mean "a fill could not be
// read" — reporting an unreadable execution as no execution is how a real trade
// goes unbooked while the request looks successful.
func (v *OKXVenue) fills(trades []okxFill, st *orderpb.OrderState, instID string) ([]*orderpb.Fill, error) {
	out := make([]*orderpb.Fill, 0, len(trades))
	for _, t := range trades {
		if sz, szOK := new(big.Rat).SetString(t.FillSz); szOK && sz.Sign() <= 0 {
			// NOT AN EXECUTION. OKX reports fee-only rows on this endpoint for
			// some instrument types; a zero-size row is not a trade and must not
			// become a zero-quantity fill in the position book.
			//
			// ONLY A SIZE OKX SENT AND THIS CONNECTOR COULD READ IS SKIPPED. An
			// UNREADABLE size falls through to ParseDec below and becomes an
			// ERROR — skipping it would report a real execution as no execution,
			// which is the failure #94's error return exists for.
			continue
		}
		qty, qok := ParseDec(t.FillSz)
		px, pok := ParseDec(t.FillPx)
		if !qok || !pok {
			return nil, fmt.Errorf("okx: order %s trade %s is not representable as a Decimal "+
				"(fillSz=%q fillPx=%q)", st.GetOrderId(), t.TradeID, t.FillSz, t.FillPx)
		}
		feeMoney, feeOK := okxFee(t.Fee, t.FeeCcy)
		if !feeOK {
			return nil, fmt.Errorf("okx: order %s trade %s fee %q %s is not representable as a Decimal",
				st.GetOrderId(), t.TradeID, t.Fee, t.FeeCcy)
		}
		inst := t.InstID
		if inst == "" {
			// The instrument this adapter asked about. The fill_id must be the
			// venue-side instrument id either way, because that is the half of the
			// name the user-data stream reads off its own push.
			inst = instID
		}
		out = append(out, &orderpb.Fill{
			FillId:           inst + "-" + t.TradeID,
			OrderId:          st.GetOrderId(),
			InstrumentId:     st.GetInstrumentId(),
			Side:             st.GetSide(),
			Quantity:         qty,
			Price:            px,
			Fee:              feeMoney,
			Venue:            v.mic,
			VenueExecutionId: t.TradeID,
			// THE TRADE'S OWN INSTANT, NOT THIS ONE. The connector clock is right
			// on the placement path (the trade just happened) and wrong on the
			// recovery path: an order adopted an hour after it traded would be
			// timestamped an hour late in every execution-quality measurement that
			// reads it.
			ExecutedAt: timestamppb.New(okxMillis(t.TS, v.now)),
		})
	}
	return out, nil
}

// okxFee reads a fee OKX charged. A nil Money means NO FEE — so ok=false is a
// separate answer for "there is a fee and it could not be read" (#94). Collapsing
// the two would drop a real cost silently, which understates what the trade cost.
//
// ONE IMPLEMENTATION for both ingress paths: the REST trade record spells it
// fee/feeCcy and the websocket push spells it fillFee/fillFeeCcy, but the sign
// convention and the failure rule are one concept, and two copies of them is how
// a repair reaches one path only.
func okxFee(fee, ccy string) (*commonpb.Money, bool) {
	if fee == "" || fee == "0" {
		return nil, true
	}
	amt, ok := ParseDec(fee)
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
	return &commonpb.Money{Amount: amt, CurrencyCode: ccy}, true
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

// okxLimitOrdType maps order.v1.TimeInForce onto OKX's ordType for a priced
// order, and REFUSES the ones it cannot express.
//
// OKX CARRIES TIME-IN-FORCE IN ordType ITSELF rather than a separate parameter:
// "limit" rests (good-til-cancelled), "ioc" fills what is available and cancels
// the rest, "fok" fills entirely or not at all. Different spelling from Binance,
// identical defect underneath — this connector sent "limit" for every priced
// order whatever the trader asked for.
//
// THAT IS THE FOURTH INSTANCE OF #240/#405's DEFECT FAMILY and the worst of them.
// The earlier three produced an order that did NOTHING; this one produces an
// order that does the WRONG THING. An IOC sent as a resting limit stays at the
// exchange, so a trader who asked to hold no exposure is holding it, and a FOK
// can rest PARTIALLY FILLED — the one outcome that instruction exists to forbid.
//
// DAY and GTD are refused rather than approximated: OKX spot has no trading
// session and no good-til-date parameter on this endpoint, and resting an order
// somebody asked to expire is the same silent substitution one value over. The
// refusal lands after admission, which is #405's shape and is tracked; it is
// still strictly better than executing the wrong instruction.
func okxLimitOrdType(tif orderpb.TimeInForce) (string, error) {
	switch tif {
	case orderpb.TimeInForce_TIME_IN_FORCE_GTC,
		// UNSPECIFIED is treated as GTC deliberately, and only here: admission
		// requires a time-in-force, so an order arriving without one predates that
		// rule and was already being sent as a resting limit. This substitution
		// changes no existing behaviour.
		orderpb.TimeInForce_TIME_IN_FORCE_UNSPECIFIED:
		return "limit", nil
	case orderpb.TimeInForce_TIME_IN_FORCE_IOC:
		return "ioc", nil
	case orderpb.TimeInForce_TIME_IN_FORCE_FOK:
		return "fok", nil
	default:
		return "", fmt.Errorf("okx: spot cannot express time-in-force %v — it has no trading "+
			"session (DAY) and no good-til-date parameter (GTD); placing this as a resting limit "+
			"would keep an order the trader asked to expire", tif)
	}
}

// isAlgoOrder reports whether an order is placed as a CONDITIONAL order rather
// than a regular one (#485).
//
// IT IS DERIVED FROM THE ORDER TYPE, NOT REMEMBERED. Placement, query and cancel
// each need to know which endpoint an order lives on, and they are three
// separate calls that can happen minutes apart on different pods. Deriving it
// from the OrderState — which every one of them already holds — means there is
// no fourth thing to keep in sync, and no way for a cancel to reach the wrong
// endpoint because a flag was lost.
func isAlgoOrder(t orderpb.OrderType) bool {
	return t == orderpb.OrderType_ORDER_TYPE_STOP || t == orderpb.OrderType_ORDER_TYPE_STOP_LIMIT
}

// okxAlgoBody maps a stop onto OKX conditional-order parameters.
//
// # ordType "trigger", not "conditional"
//
// Both exist. "conditional" is oriented around attaching a stop-loss and/or
// take-profit pair (slTriggerPx/slOrdPx, tpTriggerPx/tpOrdPx); "trigger" is a
// single level and a single resulting order, which is exactly what order.v1's
// ORDER_TYPE_STOP and ORDER_TYPE_STOP_LIMIT mean. Using the pair form for a
// one-sided instruction would leave half the message unused and invite somebody
// to fill it in with the other side, which order.v1 has no way to express.
//
// # orderPx -1 is the market instruction
//
// OKX spells "become a MARKET order when the trigger fires" as orderPx = -1, and
// a price otherwise. That is the whole difference between STOP and STOP_LIMIT
// here, and it is why a STOP must NOT carry a limit price: sending one silently
// converts it into a stop-limit that can fail to fill in exactly the fast market
// the stop was placed for.
//
// # The direction is the venue's to enforce
//
// OKX decides which way a trigger fires by comparing triggerPx to the market at
// placement, and refuses a level that is already crossed. order.v1 has no field
// for direction and this connector invents none: a stop on the wrong side of the
// market is refused by the exchange, loudly, which is the right failure for an
// instruction nobody can satisfy.
func okxAlgoBody(st *orderpb.OrderState, instID string) (map[string]string, error) {
	side, err := okxSide(st.GetSide())
	if err != nil {
		return nil, err
	}
	if err := orderid.Valid(st.GetOrderId()); err != nil {
		return nil, fmt.Errorf("okx: %w", err)
	}
	if dec.IsZero(st.GetStopPrice()) {
		return nil, errors.New("okx: a stop order requires a positive trigger price")
	}
	body := map[string]string{
		"instId": instID,
		"tdMode": "cash", // spot
		"side":   side,
		// OUR ID, AND IT IS WHAT SURVIVES THE TRIGGER. The order that eventually
		// fills is created by OKX with a clOrdId of its own; this is the field it
		// carries ours on, and the field okx_userdata.go reads first.
		"algoClOrdId": st.GetOrderId(),
		"ordType":     "trigger",
		"sz":          FormatDec(st.GetOrderedQuantity()),
		"triggerPx":   FormatDec(st.GetStopPrice()),
	}
	switch st.GetOrderType() {
	case orderpb.OrderType_ORDER_TYPE_STOP:
		body["orderPx"] = "-1" // market on trigger
	case orderpb.OrderType_ORDER_TYPE_STOP_LIMIT:
		if dec.IsZero(st.GetLimitPrice()) {
			return nil, errors.New("okx: a stop-limit order requires a positive limit price")
		}
		body["orderPx"] = FormatDec(st.GetLimitPrice())
	default:
		return nil, fmt.Errorf("okx: %v is not a conditional order type", st.GetOrderType())
	}
	return body, nil
}

// TimeInForce declares which time-in-force instructions this connector can
// express, and it is the exact set the translation switch accepts (#486).
//
// GTC, IOC AND FOK ONLY. OKX spot has no trading session, so DAY has no meaning
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
// MarginModes is which collateral regimes this connector can express (#417).
//
// CASH ONLY, AND THE WIRE IS WHY. Every order this connector builds hardcodes
// `"tdMode": "cash"` — okx_venue.go's place and amend bodies and okx_rest.go's
// query — which is OKX's spot regime. Cross and isolated are a different tdMode
// AND a different instrument family (SWAP/FUTURES rather than SPOT), so this is
// not a flag that could be flipped: the connector has no code path that could
// place them, and declaring them would promise the OMS a translation that does
// not exist.
//
// Declaring CASH rather than leaving this empty is the point. Empty means "did
// not say" and the OMS admits anything; saying CASH is what makes an order
// asking for cross margin refused at ADMISSION, naming this venue, instead of
// being placed as spot with the audit root claiming leverage.
func (v *OKXVenue) MarginModes() []orderpb.MarginMode {
	return []orderpb.MarginMode{orderpb.MarginMode_MARGIN_MODE_UNSPECIFIED}
}

// marginModeSupported reports whether this connector can express a collateral
// regime on the wire. It is the TRANSLATOR half of the capability contract, and
// it exists as a named function for the same reason okxLimitOrdType does: the
// declaration above and the refusal in Execute are two statements of one fact,
// and nothing in the type system ties them together. Split across a declaration
// and an inline `!= UNSPECIFIED`, the only way to check they agree was to read
// both and believe it (#742).
//
// tdMode is the OKX field this would set. The connector sends "cash"
// unconditionally, so UNSPECIFIED — which IS spot — is the only regime it can
// honestly claim. Adding cross or isolated here without also sending the
// matching tdMode would place a spot order while the audit root recorded margin.
func marginModeSupported(m orderpb.MarginMode) bool {
	return m == orderpb.MarginMode_MARGIN_MODE_UNSPECIFIED
}

func (v *OKXVenue) TimeInForce() []orderpb.TimeInForce {
	return []orderpb.TimeInForce{
		orderpb.TimeInForce_TIME_IN_FORCE_GTC,
		orderpb.TimeInForce_TIME_IN_FORCE_IOC,
		orderpb.TimeInForce_TIME_IN_FORCE_FOK,
	}
}
