package ingest

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	signalpb "github.com/eighred/kanz/kanz-schemas-go/signal/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/signal/translate"
)

// Options configures a Pipeline. The seams default to deny-by-default / error
// when a required one is nil.
type Options struct {
	Auth      *Authenticator
	Symbols   SymbolResolver
	Prices    PriceSource
	Equity    EquitySource
	Positions PositionSource
	Alloc     AllocationPolicy
	Publisher Publisher

	// TenantOf maps a fund to its tenant_id; nil ⇒ the fund_id is the tenant.
	TenantOf func(fundID string) string
	// Gate is the kill-switch — the SAME *translate.Gate the native alpha runner
	// holds, so one trip paralyzes both channels. Required: the local
	// `func(fundID) bool` seam this replaced defaulted to constant false and was
	// never wired in cmd/, which left the brake disconnected in production.
	Gate *translate.Gate
	// MaxQuantity bounds the RESOLVED base-asset quantity of one alert. It is handed
	// straight to the translator because THIS LAYER CANNOT APPLY IT: the perimeter
	// holds a bare `size` whose unit SizeType has not yet decided.
	//
	// It replaced a `MaxSize *big.Rat` compared against that bare size, which is why
	// a cap of 10 meaning "10 BTC" admitted {"size":"9","size_type":"pct_of_equity"}
	// and fanned out 1,800 BTC (#240). The type change is the point: translate.Qty
	// cannot be compared against Intent.Size, so the old check no longer compiles.
	MaxQuantity translate.Qty
	// RequireSignalTS refuses an alert carrying no `ts`. Off by default; see
	// translate.Options.RequireSignalTS for why the absence is counted rather
	// than refused until an operator arms it.
	RequireSignalTS bool

	// OnUnstampedSignal is called for every alert that arrives with no `ts`, with
	// the strategy that sent it. Nil ⇒ not counted.
	//
	// IT TAKES THE STRATEGY ID because the fix is per template: an operator needs
	// to know WHICH strategies to update before RequireSignalTS can be armed
	// without taking those strategies offline. A bare count would say the estate
	// has a problem without saying whose.
	OnUnstampedSignal func(strategyID string)

	// MaxSignalAge bounds how old an alert may be when it is acted on (#416).
	// Handed to the translator because BOTH BRAINS must share the bound: a check
	// here would leave the native alpha path unbounded, which is how the
	// resolved-quantity cap came to be at this layer and wrong (#240).
	MaxSignalAge time.Duration
	// MaxLeverage bounds the leverage a signal may ask for; nil ⇒ no bound.
	//
	// SUBORDINATE to a hard refusal: translate rejects ANY leverage != 1, because
	// order.v1.SubmitOrder cannot carry leverage and accepting it wrote a levered
	// position onto the audit root that no venue was asked for (#240). So a
	// max_leverage above 1 currently binds on nothing. It stays because it is the
	// bound that becomes live again the day leverage reaches the venue adapters.
	MaxLeverage *big.Rat
	// ReplayWindow bounds the nonce cache; 0 ⇒ 5m.
	ReplayWindow time.Duration

	Now func() time.Time
}

// Pipeline is the TradingView webhook FRONT END: it authenticates an alert,
// parses and validates it, and hands the resolved intent to the shared
// translator, which records the StrategySignal FACT and fans out the SubmitOrder
// commands.
//
// The fan-out itself lives in internal/signal/translate because the native alpha
// engines produce signals too, and both brains must reach the venues through the
// same gates — a second fan-out here would be a second execution path the OMS's
// pre-trade compliance and risk checks could drift away from.
type Pipeline struct {
	opt    Options
	tr     *translate.Translator
	window time.Duration
	now    func() time.Time
}

// NewPipeline validates the required seams and returns the Pipeline.
func NewPipeline(opt Options) (*Pipeline, error) {
	if opt.Auth == nil || opt.Symbols == nil {
		return nil, errors.New("ingest: auth and symbols are required")
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	// DEFAULTED HERE TOO, not only in config.Load, and for the reason the replay
	// window below is: a binary that wires this pipeline and forgets the field
	// would otherwise run UNBOUNDED, and the whole point of #416 is that an
	// unbounded deployment is the dangerous one. Zero means "not set", not
	// "disabled" — config.Load is where an explicit 0 turns the bound off.
	maxAge := opt.MaxSignalAge
	if maxAge <= 0 {
		maxAge = 2 * time.Minute
	}
	tr, err := translate.New(translate.Options{
		Prices: opt.Prices, Equity: opt.Equity, Positions: opt.Positions,
		MaxSignalAge:    maxAge,
		RequireSignalTS: opt.RequireSignalTS,
		Alloc:           opt.Alloc, Publisher: opt.Publisher, Gate: opt.Gate,
		MaxQuantity: opt.MaxQuantity,
		TenantOf:    opt.TenantOf, Now: opt.Now,
	})
	if err != nil {
		return nil, err
	}
	w := opt.ReplayWindow
	if w <= 0 {
		w = 5 * time.Minute
	}
	return &Pipeline{opt: opt, tr: tr, window: w, now: opt.Now}, nil
}

// Result summarizes a processed signal.
type Result = translate.Result

// Process runs the full ingest path for one webhook: authenticate → validate →
// (translator) record the StrategySignal FACT → resolve size → fan out N
// SubmitOrder commands. All events share correlation_id = signal_id, so the
// durable log threads signal → orders → fills as one transaction.
func (p *Pipeline) Process(ctx context.Context, rawBody []byte, remoteIP net.IP, sigHeader string) (*Result, error) {
	wh, err := parseWebhook(rawBody)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	// Authenticate CLAIMS the nonce; it does not consume it. The claim is a lease, and
	// this function owns settling it.
	if err := p.opt.Auth.Authenticate(ctx, rawBody, remoteIP, sigHeader, wh.StrategyID, wh.Nonce); err != nil {
		return nil, err // ErrUnauthorized / ErrReplayed / ErrNonceStoreUnavailable
	}

	res, err := p.decide(ctx, wh)

	// SETTLE THE NONCE (EXEC-M17).
	//
	// COMMIT when the platform reached a VERDICT on this alert — it emitted it, or it
	// deliberately refused it (halted, malformed, oversized). A redelivery of a decided
	// alert is a replay, and re-firing it is a double trade.
	//
	// RELEASE when the platform FAILED TO DECIDE — the broker was down, the size could
	// not be priced. Nobody acted on the signal, so a redelivery must be free to retry
	// it. This used to burn the nonce regardless, which meant a transient broker error
	// made a live trading signal PERMANENTLY unreplayable — the loss the three-phase
	// lease exists to prevent. The retry is safe by construction: signal_id is
	// DeterministicID(strategy, nonce), so it re-derives the same idempotency keys and
	// the broker collapses anything that did land.
	if decided(err) {
		p.opt.Auth.CommitNonce(ctx, wh.StrategyID, wh.Nonce)
	} else {
		p.opt.Auth.ReleaseNonce(ctx, wh.StrategyID, wh.Nonce)
	}
	return res, err
}

// decided reports whether err represents a VERDICT on the alert, as opposed to a
// failure to reach one.
//
// A HALT is a verdict, and deliberately so: an alert that fired during a halt is stale
// by the time the halt clears, and re-firing it into a moved market is worse than
// dropping it. So a halt keeps burning the nonce, exactly as it did before.
func decided(err error) bool {
	return err == nil || errors.Is(err, ErrHalted) || errors.Is(err, ErrBadRequest)
}

// decide runs everything after authentication: the halt gate, validation, size
// resolution, and the fan-out. Its error decides whether the nonce is held or freed.
func (p *Pipeline) decide(ctx context.Context, wh *Webhook) (*Result, error) {
	// The brake, at the perimeter: reject before parsing, before any work. The
	// translator checks the same gate again on Emit — that is not redundant, it is
	// the point. The gate can trip in the microseconds between here and there, and
	// the check that matters is the one closest to the venue.
	if p.opt.Gate.Halted() {
		_, reason, _ := p.opt.Gate.State()
		return nil, fmt.Errorf("%w: %s", ErrHalted, reason)
	}

	action, err := parseAction(wh.Action)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	sizeType, err := parseSizeType(wh.SizeType)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	marginMode, err := parseMarginMode(wh.MarginMode)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	instrumentID, ok := p.opt.Symbols.Resolve(wh.Symbol)
	if !ok {
		return nil, fmt.Errorf("%w: unknown symbol %q", ErrBadRequest, wh.Symbol) // allowed-asset gate
	}
	sizeRat, err := dec.ParseRat(wh.Size)
	if err != nil || sizeRat.Sign() <= 0 {
		return nil, fmt.Errorf("%w: size must be a positive decimal", ErrBadRequest)
	}
	leverageRat, err := dec.ParseRat(wh.Leverage)
	if err != nil || leverageRat.Sign() <= 0 {
		return nil, fmt.Errorf("%w: leverage must be a positive decimal", ErrBadRequest)
	}
	// Only the leverage bound is applied here. The SIZE bound moved into the
	// translator, where the unit is known — see Options.MaxQuantity (#240).
	if err := p.sanity(leverageRat); err != nil {
		return nil, err
	}
	orderType, limitPrice, err := p.orderPricing(wh)
	if err != nil {
		return nil, err
	}
	// The alert's own time. Refused if it is present and unparseable, rather than
	// quietly becoming nil — see parseTS (#416).
	sourceTS, err := parseTS(wh.TS)
	if err != nil {
		return nil, err
	}
	// AN ALERT WITH NO TIME IS ONE WHOSE AGE NOTHING CAN JUDGE. It is accepted
	// (see translate.Options.MaxSignalAge) and it is NOT silent: counted per
	// strategy, so the templates that still need updating are nameable before
	// WEBHOOK_INGEST_REQUIRE_SIGNAL_TS is armed on them.
	if sourceTS == nil && p.opt.OnUnstampedSignal != nil {
		p.opt.OnUnstampedSignal(wh.StrategyID)
	}

	res, err := p.tr.Emit(ctx, translate.Intent{
		SignalID:     translate.DeterministicID(wh.StrategyID, wh.Nonce),
		StrategyID:   wh.StrategyID,
		FundID:       wh.FundID,
		InstrumentID: instrumentID,
		SourceSymbol: wh.Symbol,
		Action:       action,
		Size:         sizeRat,
		SizeType:     sizeType,
		Leverage:     leverageRat,
		MarginMode:   marginMode,
		OrderType:    orderType,
		LimitPrice:   limitPrice,
		TimeInForce:  orderpb.TimeInForce_TIME_IN_FORCE_DAY,
		Source:       signalpb.SignalSource_SIGNAL_SOURCE_TRADINGVIEW_WEBHOOK,
		SourceTS:     sourceTS,
	})
	if err != nil {
		return nil, mapTranslateErr(err)
	}
	return res, nil
}

// mapTranslateErr re-maps the translator's sentinels onto the pipeline's, which
// the HTTP server maps to status codes.
func mapTranslateErr(err error) error {
	switch {
	case errors.Is(err, translate.ErrInvalidIntent), errors.Is(err, translate.ErrSizeExceedsMax):
		// Both are a VERDICT on the alert — the caller sent something this platform
		// will not act on — so both answer 400 and both BURN the nonce (see decided).
		return fmt.Errorf("%w: %v", ErrBadRequest, err)
	case errors.Is(err, translate.ErrUnresolvable):
		return fmt.Errorf("%w: %v", ErrUnresolvable, err)
	default:
		return err
	}
}

func (p *Pipeline) orderPricing(wh *Webhook) (orderpb.OrderType, *big.Rat, error) {
	switch wh.OrderType {
	case "market":
		return orderpb.OrderType_ORDER_TYPE_MARKET, nil, nil
	case "limit":
		lp, err := dec.ParseRat(wh.LimitPrice)
		if err != nil || lp.Sign() <= 0 {
			return 0, nil, fmt.Errorf("%w: limit order requires a positive limit_price", ErrBadRequest)
		}
		return orderpb.OrderType_ORDER_TYPE_LIMIT, lp, nil
	default:
		return 0, nil, fmt.Errorf("%w: unknown order_type %q", ErrBadRequest, wh.OrderType)
	}
}

// sanity applies the bounds this layer can apply. There is no size bound here:
// `size` at the perimeter is a bare number whose unit SizeType has not yet
// decided, and bounding it was the defect (#240).
func (p *Pipeline) sanity(leverage *big.Rat) error {
	if p.opt.MaxLeverage != nil && leverage.Cmp(p.opt.MaxLeverage) > 0 {
		return fmt.Errorf("%w: leverage exceeds the configured maximum", ErrBadRequest)
	}
	return nil
}

// parseTS reads the alert's own timestamp.
//
// A MALFORMED ts IS AN ERROR, NOT A nil (#416). It used to return nil for both
// "absent" and "2026-13-45T99:99Z", which made the two indistinguishable one
// layer up — and once a freshness bound exists, that is a bypass: a sender that
// cannot pass the age check sends a ts the parser rejects, gets nil, and the
// bound has nothing to judge. The translator refuses a nil under a bound, so the
// two now differ only in the message, but the difference is worth keeping: an
// operator debugging refused alerts needs "your ts is unparseable" and "you sent
// no ts" to be different sentences.
func parseTS(s string) (*timestamppb.Timestamp, error) {
	if s == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil, fmt.Errorf("%w: ts %q is not RFC3339", ErrBadRequest, s)
	}
	return timestamppb.New(t.UTC()), nil
}

// Pipeline error sentinels, mapped to HTTP status by the server.
var (
	ErrBadRequest = errors.New("ingest: bad request")
	// ErrHalted aliases the translator's sentinel rather than declaring a second
	// one: the gate trips at the perimeter AND inside Emit, and the server's
	// errors.Is must answer 423 Locked no matter which of the two rejected.
	ErrHalted       = translate.ErrHalted
	ErrUnresolvable = errors.New("ingest: could not resolve order size")
)
