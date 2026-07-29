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
	// MaxSize / MaxLeverage bound the sanity gate; nil ⇒ no bound.
	MaxSize     *big.Rat
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
	tr, err := translate.New(translate.Options{
		Prices: opt.Prices, Equity: opt.Equity, Positions: opt.Positions,
		Alloc: opt.Alloc, Publisher: opt.Publisher, Gate: opt.Gate,
		TenantOf: opt.TenantOf, Now: opt.Now,
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
	if err := p.sanity(sizeRat, leverageRat); err != nil {
		return nil, err
	}
	orderType, limitPrice, err := p.orderPricing(wh)
	if err != nil {
		return nil, err
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
		SourceTS:     parseTS(wh.TS),
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
	case errors.Is(err, translate.ErrInvalidIntent):
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

func (p *Pipeline) sanity(size, leverage *big.Rat) error {
	if p.opt.MaxSize != nil && size.Cmp(p.opt.MaxSize) > 0 {
		return fmt.Errorf("%w: size exceeds the configured maximum", ErrBadRequest)
	}
	if p.opt.MaxLeverage != nil && leverage.Cmp(p.opt.MaxLeverage) > 0 {
		return fmt.Errorf("%w: leverage exceeds the configured maximum", ErrBadRequest)
	}
	return nil
}

func parseTS(s string) *timestamppb.Timestamp {
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil
	}
	return timestamppb.New(t.UTC())
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
