// Package translate is the ONE path from an advisory signal to executable order
// commands, shared by every brain that can produce a signal.
//
// Kanz is a hybrid brain: signals arrive from external proprietary strategies
// (TradingView webhooks, via services/webhook-ingest) and from Kanz's own native
// alpha engines (the off-bus L2 book, via pkg/alpha). Both must reach the venues
// through the SAME gates — a second, parallel fan-out would be a second execution
// path that the OMS's pre-trade compliance and risk checks could drift away from.
// So the front ends differ (HMAC + webhook parsing vs. an in-process engine tick)
// and everything downstream of the resolved intent is this package.
//
// The invariant it encodes: A SIGNAL IS ADVISORY INTENT, NEVER A COMMAND.
// Emit records the immutable StrategySignal FACT — the audit root every order
// chains causation to — and then fans out the concrete order.v1.SubmitOrder
// COMMANDS the OMS still applies its gates to. Publishing the FACT alone executes
// nothing: nothing subscribes to it. The commands are what trade.
package translate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"time"

	commandpb "github.com/eighred/kanz/kanz-schemas-go/command/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	signalpb "github.com/eighred/kanz/kanz-schemas-go/signal/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/pkg/bus"
)

// Bus subjects. The command subject mirrors services/oms/internal/order so the
// commands land on the queue the OMS already consumes; kept local so this does
// not import the OMS service package.
const (
	SubjectSignal = "strategy.signal.received" // FACT — the audit root
	SubjectSubmit = "order.order.submit"       // COMMAND (order.v1.SubmitOrder)
	domainSignal  = "signal"
	domainOrder   = "order"
)

// Publisher is the bus publish surface — satisfied by *bus.Producer.
type Publisher interface {
	Publish(ctx context.Context, e bus.Event) error
}

// Intent is a fully-resolved, front-end-neutral trading intent: what a brain
// decided, with the instrument already canonical and the size already parsed.
// The webhook front end builds one from an authenticated alert; a native alpha
// engine builds one from the in-memory book. Everything after this point is
// identical for both.
type Intent struct {
	// SignalID is the deterministic idempotency root. A re-delivered webhook or a
	// re-fired engine tick MUST derive the same id so the fan-out dedups instead of
	// double-trading. Use DeterministicID.
	SignalID string
	// StrategyID identifies the producing strategy and becomes the command issuer
	// principal ("strategy:{id}").
	StrategyID string
	// FundID is the fund whose capital trades; it scopes ownership and allocation.
	FundID string
	// InstrumentID is the canonical Kanz instrument (already resolved).
	InstrumentID string
	// SourceSymbol is the raw upstream ticker, retained for audit. Optional.
	SourceSymbol string

	Action     signalpb.SignalAction
	Size       *big.Rat
	SizeType   signalpb.SizeType
	Leverage   *big.Rat
	MarginMode signalpb.MarginMode

	OrderType  orderpb.OrderType
	LimitPrice *big.Rat // required for LIMIT, nil for MARKET
	// TimeInForce governs the resting behaviour of every leg. A cross-venue
	// arbitrage leg MUST be IOC: an unfilled arb leg that rests turns a hedged,
	// market-neutral pair into naked directional exposure the moment the other leg
	// fills. Zero ⇒ DAY.
	TimeInForce orderpb.TimeInForce

	// Source is the provenance — which brain produced this. Never inferred from the
	// subject or the strategy_id.
	Source   signalpb.SignalSource
	SourceTS *timestamppb.Timestamp
}

// Result summarizes an emitted signal.
type Result struct {
	SignalID string
	OrderIDs []string
}

// Options binds the translator to live platform state.
type Options struct {
	Prices    PriceSource
	Equity    EquitySource
	Positions PositionSource
	Alloc     AllocationPolicy
	Publisher Publisher

	// Gate is the kill-switch, and it is REQUIRED — not because the translator
	// cannot run without one, but because the seam this replaced was optional,
	// defaulted open, and was never wired in any binary. An optional brake is an
	// absent brake. Tests and local dev pass OpenGate; production passes NewGate and
	// lets the lifecycle stream open it.
	Gate *Gate

	// TenantOf maps a fund to its tenant_id; nil ⇒ the fund_id is the tenant.
	TenantOf func(fundID string) string
	Now      func() time.Time
}

// Translator turns an Intent into the audit FACT plus the venue-allocated order
// commands.
type Translator struct {
	opt Options
	now func() time.Time
}

// New validates the required seams and returns the Translator.
func New(opt Options) (*Translator, error) {
	if opt.Prices == nil || opt.Equity == nil || opt.Positions == nil ||
		opt.Alloc == nil || opt.Publisher == nil {
		return nil, errors.New("translate: prices, equity, positions, alloc, and publisher are required")
	}
	if opt.Gate == nil {
		return nil, errors.New("translate: gate is required (use NewGate for production, OpenGate for tests/dev)")
	}
	if opt.TenantOf == nil {
		opt.TenantOf = func(fundID string) string { return fundID }
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	return &Translator{opt: opt, now: opt.Now}, nil
}

// Emit records the StrategySignal FACT, then fans the intent out into one
// SubmitOrder command per allocated venue. All events share
// correlation_id = signal_id, so the durable log threads signal → orders → fills
// as one transaction.
func (t *Translator) Emit(ctx context.Context, in Intent) (*Result, error) {
	// The brake comes before everything — before validation, before the FACT is
	// recorded. A halted system does not even leave an audit trail of intents it
	// refused to act on; it simply does not act. Both brains reach the venues
	// through here, so this single check paralyzes ingest and autonomous execution
	// together.
	if t.opt.Gate.Halted() {
		_, reason, since := t.opt.Gate.State()
		return nil, fmt.Errorf("%w: %s (since %s)", ErrHalted, reason, since.UTC().Format(time.RFC3339))
	}
	if err := in.validate(); err != nil {
		return nil, err
	}
	tenant := t.opt.TenantOf(in.FundID)
	ctx = bus.WithCorrelationID(ctx, in.SignalID)

	if err := t.publishSignal(ctx, in, tenant); err != nil {
		return nil, fmt.Errorf("publish signal: %w", err)
	}
	orderIDs, err := t.fanOut(ctx, in, tenant)
	if err != nil {
		return nil, err
	}
	return &Result{SignalID: in.SignalID, OrderIDs: orderIDs}, nil
}

// validate is the deny-by-default gate: an intent missing an identity, a
// direction, or a positive size never reaches a venue.
func (in Intent) validate() error {
	switch {
	case in.SignalID == "" || in.StrategyID == "" || in.FundID == "" || in.InstrumentID == "":
		return fmt.Errorf("%w: signal_id, strategy_id, fund_id and instrument_id are required", ErrInvalidIntent)
	case in.Action == signalpb.SignalAction_SIGNAL_ACTION_UNSPECIFIED:
		return fmt.Errorf("%w: action must not be UNSPECIFIED", ErrInvalidIntent)
	case in.Source == signalpb.SignalSource_SIGNAL_SOURCE_UNSPECIFIED:
		return fmt.Errorf("%w: source (provenance) must not be UNSPECIFIED", ErrInvalidIntent)
	}
	// A CLOSE carries no size — it flattens whatever position exists.
	if in.Action != signalpb.SignalAction_SIGNAL_ACTION_CLOSE {
		if in.SizeType == signalpb.SizeType_SIZE_TYPE_UNSPECIFIED {
			return fmt.Errorf("%w: size_type must not be UNSPECIFIED", ErrInvalidIntent)
		}
		if in.Size == nil || in.Size.Sign() <= 0 {
			return fmt.Errorf("%w: size must be a positive decimal", ErrInvalidIntent)
		}
	}
	if in.OrderType == orderpb.OrderType_ORDER_TYPE_LIMIT && (in.LimitPrice == nil || in.LimitPrice.Sign() <= 0) {
		return fmt.Errorf("%w: a limit order requires a positive limit price", ErrInvalidIntent)
	}
	return nil
}

// publishSignal records the immutable StrategySignal FACT — the audit root.
func (t *Translator) publishSignal(ctx context.Context, in Intent, tenant string) error {
	leverage := in.Leverage
	if leverage == nil {
		leverage = big.NewRat(1, 1) // spot / unlevered
	}
	size, sizeOK := toDec(in.Size)
	lev, levOK := toDec(leverage)
	if !sizeOK || !levOK {
		return fmt.Errorf("translate: signal %s size or leverage is not representable as a Decimal "+
			"— refusing to record an audit root carrying a number the platform invented", in.SignalID)
	}
	sig := &signalpb.StrategySignal{
		SignalId:     in.SignalID,
		StrategyId:   in.StrategyID,
		FundId:       in.FundID,
		InstrumentId: in.InstrumentID,
		SourceSymbol: in.SourceSymbol,
		Action:       in.Action,
		Size:         size,
		Leverage:     lev,
		MarginMode:   in.MarginMode,
		SizeType:     in.SizeType,
		Source:       in.Source,
		ReceivedTs:   timestamppb.New(t.now().UTC()),
		SourceTs:     in.SourceTS,
	}
	return t.opt.Publisher.Publish(ctx, bus.Event{
		Subject:       SubjectSignal,
		EventType:     SubjectSignal,
		EventClass:    envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion: 1,
		Domain:        domainSignal,
		EventTime:     t.now().UTC(),
		PartitionKey:  in.SignalID,
		TenantID:      tenant,
		Payload:       sig,
	})
}

// fanOut resolves the absolute quantity and emits one SubmitOrder per venue in
// the fund's allocation policy.
func (t *Translator) fanOut(ctx context.Context, in Intent, tenant string) ([]string, error) {
	venues, err := t.opt.Alloc.VenuesFor(in.FundID)
	if err != nil {
		return nil, err // ErrNoAllocation — deny-by-default
	}

	var baseQty *big.Rat
	if in.Action != signalpb.SignalAction_SIGNAL_ACTION_CLOSE {
		baseQty, err = t.resolveQuantity(ctx, in)
		if err != nil {
			return nil, err
		}
	}

	tif := in.TimeInForce
	if tif == orderpb.TimeInForce_TIME_IN_FORCE_UNSPECIFIED {
		tif = orderpb.TimeInForce_TIME_IN_FORCE_DAY
	}

	orderIDs := make([]string, 0, len(venues))
	for _, v := range venues {
		side, qty, err := t.legSizeAndSide(ctx, in, baseQty, v)
		if err != nil {
			return nil, err
		}
		if qty == nil || qty.Sign() <= 0 {
			continue // nothing to do at this venue (e.g. CLOSE with no position)
		}
		orderID := DeterministicID(in.SignalID, v.Venue)
		qtyD, qtyOK := toDec(qty)
		limitD, limitOK := toDec(in.LimitPrice)
		if !qtyOK || !limitOK {
			return nil, fmt.Errorf("translate: order %s quantity or limit price is not representable "+
				"as a Decimal — refusing to submit an order for a size the platform invented", orderID)
		}
		cmd := &orderpb.SubmitOrder{
			Metadata:     &commandpb.CommandMetadata{Issuer: "strategy:" + in.StrategyID, TargetId: orderID},
			OrderId:      orderID,
			PortfolioId:  in.FundID,
			InstrumentId: in.InstrumentID,
			Side:         side,
			Quantity:     qtyD,
			OrderType:    in.OrderType,
			LimitPrice:   limitD,
			TimeInForce:  tif,
			Venue:        v.Venue, // route this leg to its allocated venue
		}
		if err := t.publishCommand(ctx, cmd, orderID, tenant); err != nil {
			return nil, fmt.Errorf("publish order %s: %w", orderID, err)
		}
		orderIDs = append(orderIDs, orderID)
	}
	return orderIDs, nil
}

func (t *Translator) publishCommand(ctx context.Context, cmd *orderpb.SubmitOrder, orderID, tenant string) error {
	return t.opt.Publisher.Publish(ctx, bus.Event{
		Subject:        SubjectSubmit,
		EventType:      SubjectSubmit,
		EventClass:     envelopepb.EventClass_EVENT_CLASS_COMMAND,
		SchemaVersion:  1,
		Domain:         domainOrder,
		EventTime:      t.now().UTC(),
		PartitionKey:   orderID,
		IdempotencyKey: orderID, // deterministic ⇒ a re-fanned command dedups
		TenantID:       tenant,
		Payload:        cmd,
	})
}

// resolveQuantity turns a sized intent into an absolute quantity against live
// fund state, exactly (big.Rat, no float).
func (t *Translator) resolveQuantity(ctx context.Context, in Intent) (*big.Rat, error) {
	switch in.SizeType {
	case signalpb.SizeType_SIZE_TYPE_ABSOLUTE_QTY:
		return new(big.Rat).Set(in.Size), nil
	case signalpb.SizeType_SIZE_TYPE_QUOTE_NOTIONAL:
		price, err := t.price(ctx, in.InstrumentID)
		if err != nil {
			return nil, err
		}
		return new(big.Rat).Quo(in.Size, price), nil // notional / price
	case signalpb.SizeType_SIZE_TYPE_PCT_OF_EQUITY:
		nav, err := t.opt.Equity.Equity(ctx, in.FundID)
		if err != nil {
			return nil, err
		}
		price, err := t.price(ctx, in.InstrumentID)
		if err != nil {
			return nil, err
		}
		// (pct/100 × NAV) / price
		frac := new(big.Rat).Quo(in.Size, big.NewRat(100, 1))
		notional := new(big.Rat).Mul(frac, nav)
		return new(big.Rat).Quo(notional, price), nil
	default:
		return nil, fmt.Errorf("%w: unspecified size_type", ErrInvalidIntent)
	}
}

func (t *Translator) price(ctx context.Context, instrumentID string) (*big.Rat, error) {
	price, err := t.opt.Prices.Price(ctx, instrumentID)
	if err != nil {
		return nil, err
	}
	if price == nil || price.Sign() <= 0 {
		return nil, fmt.Errorf("%w: non-positive price for %s", ErrUnresolvable, instrumentID)
	}
	return price, nil
}

// legSizeAndSide computes one venue leg's side and quantity. BUY/SELL scale the
// base quantity by the venue weight; CLOSE flattens the venue's current position
// (opposite side, full size).
func (t *Translator) legSizeAndSide(ctx context.Context, in Intent, baseQty *big.Rat, v VenueAllocation) (orderpb.Side, *big.Rat, error) {
	if in.Action == signalpb.SignalAction_SIGNAL_ACTION_CLOSE {
		pos, err := t.opt.Positions.Position(ctx, in.FundID, v.Venue, in.InstrumentID)
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
	if in.Action == signalpb.SignalAction_SIGNAL_ACTION_SELL {
		side = orderpb.Side_SIDE_SELL
	}
	return side, new(big.Rat).Mul(baseQty, v.Weight), nil
}

// DeterministicID derives a stable id from its parts (strategy+nonce → signal id;
// signal+venue → order id) so a re-delivered signal dedups rather than
// double-trading.
func DeterministicID(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// toDec converts an exact rational to a common.v1.Decimal; nil stays nil so an
// absent limit price is absent, not zero.
// toDec converts a rational to a Decimal PRESERVING MAGNITUDE (#94).
//
// It fed SubmitOrder.Quantity and LimitPrice — an order actually placed at a
// venue — and the StrategySignal FACT that is the audit root for it, through the
// WRAPPING dec.ToProto. Above roughly 92.2 billion units at scale 8 the
// coefficient wraps, so a size of 1e12 became 77662796314.5224192: an order
// submitted for a quantity nobody asked for, with a signal FACT recording the
// same fabricated number as its justification.
//
// ok=false means the value cannot be represented at all. A nil rational is
// ABSENT, not unrepresentable, and stays nil — LimitPrice is genuinely optional.
func toDec(r *big.Rat) (*commonpb.Decimal, bool) {
	if r == nil {
		return nil, true
	}
	return dec.ToProtoScaled(r)
}

// Translator error sentinels.
var (
	ErrInvalidIntent = errors.New("translate: invalid intent")
	ErrUnresolvable  = errors.New("translate: could not resolve order size")
	// ErrHalted is returned when the kill-switch is closed. Both front ends map it
	// to their own surface (the webhook perimeter answers 423 Locked).
	ErrHalted = errors.New("translate: trading halted")
)
