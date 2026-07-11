package ingest

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net"
	"time"

	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
	signalpb "github.com/kanz-eng/kanz-schemas-go/signal/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/kanz-eng/kanz/internal/dec"
	"github.com/kanz-eng/kanz/internal/signal/translate"
)

// Halted reports whether trading is halted (the kill-switch). M1 wires a
// constant false; Milestone 5 binds it to the Halt FACT. Deny-by-default: a
// true result rejects the signal before any order is produced.
type Halted func(fundID string) bool

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
	// Halted is the kill-switch; nil ⇒ never halted.
	Halted Halted
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
		Alloc: opt.Alloc, Publisher: opt.Publisher,
		TenantOf: opt.TenantOf, Now: opt.Now,
	})
	if err != nil {
		return nil, err
	}
	if opt.Halted == nil {
		opt.Halted = func(string) bool { return false }
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
	if err := p.opt.Auth.Authenticate(rawBody, remoteIP, sigHeader, wh.StrategyID, wh.Nonce, p.window); err != nil {
		return nil, err // ErrUnauthorized / ErrReplayed
	}
	if p.opt.Halted(wh.FundID) {
		return nil, ErrHalted
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
	ErrBadRequest   = errors.New("ingest: bad request")
	ErrHalted       = errors.New("ingest: trading halted")
	ErrUnresolvable = errors.New("ingest: could not resolve order size")
)
