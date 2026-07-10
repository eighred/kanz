package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net"
	"time"

	commandpb "github.com/kanz-eng/kanz-schemas-go/command/v1"
	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
	signalpb "github.com/kanz-eng/kanz-schemas-go/signal/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/kanz-eng/kanz/pkg/bus"
	"github.com/kanz-eng/kanz/services/webhook-ingest/internal/dec"
)

// Bus subjects. The command subject mirrors services/oms/internal/order so the
// commands land on the queue the OMS already consumes; kept local so ingest
// does not import the OMS service package.
const (
	subjectSignal = "strategy.signal.received" // FACT
	subjectSubmit = "order.order.submit"       // COMMAND (order.v1.SubmitOrder)
	domainSignal  = "signal"
	domainOrder   = "order"
)

// Publisher is the bus publish surface — satisfied by *bus.Producer.
type Publisher interface {
	Publish(ctx context.Context, e bus.Event) error
}

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

// Pipeline authenticates a webhook, records the signal, resolves its size, and
// fans it out into SubmitOrder commands.
type Pipeline struct {
	opt    Options
	window time.Duration
	now    func() time.Time
}

// NewPipeline validates the required seams and returns the Pipeline.
func NewPipeline(opt Options) (*Pipeline, error) {
	if opt.Auth == nil || opt.Symbols == nil || opt.Prices == nil || opt.Equity == nil ||
		opt.Positions == nil || opt.Alloc == nil || opt.Publisher == nil {
		return nil, errors.New("ingest: auth, symbols, prices, equity, positions, alloc, and publisher are required")
	}
	if opt.TenantOf == nil {
		opt.TenantOf = func(fundID string) string { return fundID }
	}
	if opt.Halted == nil {
		opt.Halted = func(string) bool { return false }
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	w := opt.ReplayWindow
	if w <= 0 {
		w = 5 * time.Minute
	}
	return &Pipeline{opt: opt, window: w, now: opt.Now}, nil
}

// Result summarizes a processed signal.
type Result struct {
	SignalID string
	OrderIDs []string
}

// Process runs the full ingest path for one webhook: authenticate → validate →
// record the StrategySignal FACT → resolve size → fan out N SubmitOrder
// commands. All events share correlation_id = signal_id, so the durable log
// threads signal → orders → fills as one transaction.
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

	signalID := deterministicID(wh.StrategyID, wh.Nonce)
	tenant := p.opt.TenantOf(wh.FundID)
	// Correlate the whole fan-out (and, via the OMS consumer, the downstream
	// fills) under the signal id.
	ctx = bus.WithCorrelationID(ctx, signalID)

	if err := p.publishSignal(ctx, wh, signalID, instrumentID, action, sizeType, marginMode, sizeRat, leverageRat, tenant); err != nil {
		return nil, fmt.Errorf("publish signal: %w", err)
	}

	orderIDs, err := p.fanOut(ctx, wh, signalID, instrumentID, action, sizeType, sizeRat, tenant)
	if err != nil {
		return nil, err
	}
	return &Result{SignalID: signalID, OrderIDs: orderIDs}, nil
}

// publishSignal records the immutable StrategySignal FACT — the audit root.
func (p *Pipeline) publishSignal(ctx context.Context, wh *Webhook, signalID, instrumentID string, action signalpb.SignalAction, sizeType signalpb.SizeType, marginMode signalpb.MarginMode, size, leverage *big.Rat, tenant string) error {
	sig := &signalpb.StrategySignal{
		SignalId:     signalID,
		StrategyId:   wh.StrategyID,
		FundId:       wh.FundID,
		InstrumentId: instrumentID,
		SourceSymbol: wh.Symbol,
		Action:       action,
		Size:         dec.ToProto(size),
		Leverage:     dec.ToProto(leverage),
		MarginMode:   marginMode,
		SizeType:     sizeType,
		ReceivedTs:   timestamppb.New(p.now().UTC()),
		SourceTs:     parseTS(wh.TS),
	}
	return p.opt.Publisher.Publish(ctx, bus.Event{
		Subject:       subjectSignal,
		EventType:     subjectSignal,
		EventClass:    envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion: 1,
		Domain:        domainSignal,
		EventTime:     p.now().UTC(),
		PartitionKey:  signalID,
		TenantID:      tenant,
		Payload:       sig,
	})
}

// fanOut resolves the absolute quantity and emits one SubmitOrder per venue in
// the fund's allocation policy.
func (p *Pipeline) fanOut(ctx context.Context, wh *Webhook, signalID, instrumentID string, action signalpb.SignalAction, sizeType signalpb.SizeType, sizeRat *big.Rat, tenant string) ([]string, error) {
	venues, err := p.opt.Alloc.VenuesFor(wh.FundID)
	if err != nil {
		return nil, err // ErrNoAllocation — deny-by-default
	}

	var baseQty *big.Rat
	if action != signalpb.SignalAction_SIGNAL_ACTION_CLOSE {
		baseQty, err = p.resolveQuantity(ctx, sizeType, sizeRat, wh.FundID, instrumentID)
		if err != nil {
			return nil, err
		}
	}

	orderType, limitPrice, err := p.orderPricing(wh)
	if err != nil {
		return nil, err
	}

	orderIDs := make([]string, 0, len(venues))
	for _, v := range venues {
		side, qty, err := p.legSizeAndSide(ctx, action, baseQty, v, wh.FundID, instrumentID)
		if err != nil {
			return nil, err
		}
		if qty == nil || qty.Sign() <= 0 {
			continue // nothing to do at this venue (e.g. CLOSE with no position)
		}
		orderID := deterministicID(signalID, v.Venue)
		cmd := &orderpb.SubmitOrder{
			Metadata:     &commandpb.CommandMetadata{Issuer: "strategy:" + wh.StrategyID, TargetId: orderID},
			OrderId:      orderID,
			PortfolioId:  wh.FundID,
			InstrumentId: instrumentID,
			Side:         side,
			Quantity:     dec.ToProto(qty),
			OrderType:    orderType,
			LimitPrice:   limitPrice,
			TimeInForce:  orderpb.TimeInForce_TIME_IN_FORCE_DAY,
		}
		if err := p.publishCommand(ctx, cmd, orderID, tenant); err != nil {
			return nil, fmt.Errorf("publish order %s: %w", orderID, err)
		}
		orderIDs = append(orderIDs, orderID)
	}
	return orderIDs, nil
}

func (p *Pipeline) publishCommand(ctx context.Context, cmd *orderpb.SubmitOrder, orderID, tenant string) error {
	return p.opt.Publisher.Publish(ctx, bus.Event{
		Subject:        subjectSubmit,
		EventType:      subjectSubmit,
		EventClass:     envelopepb.EventClass_EVENT_CLASS_COMMAND,
		SchemaVersion:  1,
		Domain:         domainOrder,
		EventTime:      p.now().UTC(),
		PartitionKey:   orderID,
		IdempotencyKey: orderID, // deterministic ⇒ a re-fanned command dedups
		TenantID:       tenant,
		Payload:        cmd,
	})
}

// resolveQuantity turns a sized signal into an absolute quantity against live
// fund state, exactly (big.Rat, no float).
func (p *Pipeline) resolveQuantity(ctx context.Context, sizeType signalpb.SizeType, size *big.Rat, fundID, instrumentID string) (*big.Rat, error) {
	switch sizeType {
	case signalpb.SizeType_SIZE_TYPE_ABSOLUTE_QTY:
		return new(big.Rat).Set(size), nil
	case signalpb.SizeType_SIZE_TYPE_QUOTE_NOTIONAL:
		price, err := p.opt.Prices.Price(ctx, instrumentID)
		if err != nil {
			return nil, err
		}
		if price.Sign() <= 0 {
			return nil, fmt.Errorf("%w: non-positive price for %s", ErrUnresolvable, instrumentID)
		}
		return new(big.Rat).Quo(size, price), nil // notional / price
	case signalpb.SizeType_SIZE_TYPE_PCT_OF_EQUITY:
		nav, err := p.opt.Equity.Equity(ctx, fundID)
		if err != nil {
			return nil, err
		}
		price, err := p.opt.Prices.Price(ctx, instrumentID)
		if err != nil {
			return nil, err
		}
		if price.Sign() <= 0 {
			return nil, fmt.Errorf("%w: non-positive price for %s", ErrUnresolvable, instrumentID)
		}
		// (pct/100 × NAV) / price
		frac := new(big.Rat).Quo(size, big.NewRat(100, 1))
		notional := new(big.Rat).Mul(frac, nav)
		return new(big.Rat).Quo(notional, price), nil
	default:
		return nil, fmt.Errorf("%w: unspecified size_type", ErrBadRequest)
	}
}

// legSizeAndSide computes one venue leg's side and quantity. BUY/SELL scale the
// base quantity by the venue weight; CLOSE flattens the venue's current
// position (opposite side, full size).
func (p *Pipeline) legSizeAndSide(ctx context.Context, action signalpb.SignalAction, baseQty *big.Rat, v VenueAllocation, fundID, instrumentID string) (orderpb.Side, *big.Rat, error) {
	if action == signalpb.SignalAction_SIGNAL_ACTION_CLOSE {
		pos, err := p.opt.Positions.Position(ctx, fundID, v.Venue, instrumentID)
		if err != nil {
			return orderpb.Side_SIDE_UNSPECIFIED, nil, err
		}
		if pos.Sign() == 0 {
			return orderpb.Side_SIDE_UNSPECIFIED, nil, nil // flat — skip
		}
		if pos.Sign() > 0 {
			return orderpb.Side_SIDE_SELL, pos, nil // long → sell to flatten
		}
		return orderpb.Side_SIDE_BUY, new(big.Rat).Abs(pos), nil // short → buy to flatten
	}
	side := orderpb.Side_SIDE_BUY
	if action == signalpb.SignalAction_SIGNAL_ACTION_SELL {
		side = orderpb.Side_SIDE_SELL
	}
	return side, new(big.Rat).Mul(baseQty, v.Weight), nil
}

func (p *Pipeline) orderPricing(wh *Webhook) (orderpb.OrderType, *commonpb.Decimal, error) {
	switch wh.OrderType {
	case "market":
		return orderpb.OrderType_ORDER_TYPE_MARKET, nil, nil
	case "limit":
		lp, err := dec.ParseRat(wh.LimitPrice)
		if err != nil || lp.Sign() <= 0 {
			return 0, nil, fmt.Errorf("%w: limit order requires a positive limit_price", ErrBadRequest)
		}
		return orderpb.OrderType_ORDER_TYPE_LIMIT, dec.ToProto(lp), nil
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

// deterministicID derives a stable id from its parts (signal id, order id) so a
// re-delivered webhook dedups rather than duplicating.
func deterministicID(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
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
