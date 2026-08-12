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

	// MaxQuantity bounds the RESOLVED base-asset quantity of one signal, checked
	// after SizeType has been applied and BEFORE the venue split. The unset Qty ⇒
	// no bound.
	//
	// It lives here, not at the webhook perimeter, because the perimeter does not
	// know the unit: it holds a bare `size` whose meaning resolveQuantity decides.
	// The bound webhook-ingest used to apply to that bare number let a
	// percent-of-equity signal walk straight past a quantity cap (#240). Both
	// brains size through this function, so one bound now covers both.
	//
	// A CLOSE is deliberately NOT bounded: it flattens an existing position, and a
	// cap that can refuse a flatten is a cap that can trap a fund in a position it
	// asked to exit.
	MaxQuantity Qty

	// MaxSignalAge bounds how old a signal may be when it is acted on. Zero ⇒ NO
	// BOUND, which is what this platform did until #416.
	//
	// THE FAILURE IT PREVENTS. A TradingView alert carries the time the strategy
	// fired it, and nothing compared that to now. The 5-minute replay window at
	// the webhook perimeter is NONCE DEDUP, not age: it stops the same alert being
	// replayed, and says nothing about a first delivery that took thirty minutes.
	// So an alert delayed by a webhook retry, a network partition or a paused pod
	// was acted on as if it were current — at the size the strategy chose for a
	// price that has since moved. The strategy's decision was about a market that
	// no longer exists, and nothing on the path noticed.
	//
	// IT BOUNDS BOTH DIRECTIONS, with the same tolerance. A timestamp far in the
	// FUTURE is not harmless: "now minus then" is negative, so a future-dated
	// signal would pass an age check forever, and a sender whose clock is wrong is
	// a sender whose age we cannot judge at all. Refusing both is one rule to
	// reason about instead of two.
	//
	// A MISSING TIMESTAMP IS NOT REFUSED BY THIS FIELD ALONE — see
	// RequireSignalTS. The reasoning is worth stating, because the opposite is
	// the tempting answer: a bound a sender can skip by omitting a field looks
	// like no bound at all.
	//
	// It is not, HERE, because of who the senders are. Every alert is
	// HMAC-authenticated with a per-strategy secret, so a party able to omit `ts`
	// is a party that could equally have sent a FRESH one — refusing the omission
	// buys nothing against them. And the failure this exists to stop is not a
	// forged alert: it is a DELAYED one, a webhook retry or a partition or a
	// paused pod, and a delayed alert carries the timestamp TradingView put on it.
	// The bound catches exactly that.
	//
	// What refusing WOULD do is break every strategy whose alert template omits a
	// field the webhook contract still calls "advisory only". So the absence is
	// counted and named instead, and RequireSignalTS turns it into a refusal once
	// the templates carry it — the OMS_REQUIRE_VERIFIED_ACCOUNT stance, unchanged.
	MaxSignalAge time.Duration

	// RequireSignalTS refuses a signal that carries no source timestamp at all.
	//
	// OFF BY DEFAULT, and armed with the strategies in hand. Every alert without a
	// timestamp is one whose age nothing can judge, so the estate should end up
	// here — but a control that refuses every strategy nobody has updated yet is a
	// trading outage, and this platform arms that kind of control deliberately
	// rather than by default. Until then the perimeter counts them, so "how much
	// of our flow is unjudgeable" is a number rather than a silence.
	RequireSignalTS bool

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
	// AUDIT THE VENUE WEIGHTS AT STARTUP, not per signal (#240). An allocation whose
	// weights do not sum to 1 is a multiplier on every trade the fund ever makes:
	// legs written as whole percents (60 / 40 — the natural mistake, since every
	// other percentage on this surface is whole-percent) fan a 2 BTC signal out as
	// 120 + 80 BTC, and a duplicated leg (0.6 / 0.6) quietly adds 20% exposure
	// forever. Neither the policy nor fanOut noticed; the multiply was unconditional.
	//
	// A policy that can enumerate its funds is audited here, which covers the static
	// map every binary and test actually wires. A future dynamic policy cannot be
	// audited at startup and MUST validate its own rows with ValidateAllocation
	// before returning them.
	if set, ok := opt.Alloc.(AllocationSet); ok {
		for fundID, legs := range set.Allocations() {
			if err := ValidateAllocation(fundID, legs); err != nil {
				return nil, err
			}
		}
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
	// HOW OLD IS THIS DECISION? (#416)
	//
	// Checked here, beside the kill-switch and before the FACT is recorded,
	// because a stale signal must leave no audit root claiming the platform acted
	// on it — the same reasoning the size bound above documents.
	if err := t.freshEnough(in); err != nil {
		return nil, err
	}
	tenant := t.opt.TenantOf(in.FundID)
	ctx = bus.WithCorrelationID(ctx, in.SignalID)

	// RESOLVE AND BOUND BEFORE THE FACT IS RECORDED.
	//
	// The size bound can only be applied in the resolved unit (#240), which means it
	// cannot run at the perimeter — it has to run here. Running it here after
	// publishSignal would leave a StrategySignal FACT with no orders chained to it and
	// no explanation on the bus, which reads exactly like a lost fan-out. So
	// everything that can REFUSE the signal now runs first, and the audit root is
	// written only for signals the platform is actually going to act on.
	venues, err := t.opt.Alloc.VenuesFor(in.FundID)
	if err != nil {
		return nil, err // ErrNoAllocation — deny-by-default
	}
	var baseQty Qty
	if in.Action != signalpb.SignalAction_SIGNAL_ACTION_CLOSE {
		baseQty, err = t.resolveQuantity(ctx, in)
		if err != nil {
			return nil, err
		}
		if err := t.enforceMaxQuantity(in, baseQty); err != nil {
			return nil, err
		}
	}

	if err := t.publishSignal(ctx, in, tenant); err != nil {
		return nil, fmt.Errorf("publish signal: %w", err)
	}
	orderIDs, err := t.fanOut(ctx, in, tenant, venues, baseQty)
	if err != nil {
		return nil, err
	}
	return &Result{SignalID: in.SignalID, OrderIDs: orderIDs}, nil
}

// enforceMaxQuantity applies the configured bound to the RESOLVED quantity — the
// only unit in which the comparison means anything.
func (t *Translator) enforceMaxQuantity(in Intent, q Qty) error {
	if !t.opt.MaxQuantity.IsSet() || !q.IsSet() || q.Cmp(t.opt.MaxQuantity) <= 0 {
		return nil
	}
	return fmt.Errorf("%w: signal %s resolves to %s %s — above the configured maximum of %s. "+
		"The raw size was %s interpreted as %s; the bound is applied to the RESOLVED "+
		"base-asset quantity, never to the raw size",
		ErrSizeExceedsMax, in.SignalID, q.RatString(), in.InstrumentID,
		t.opt.MaxQuantity.RatString(), ratString(in.Size), in.SizeType)
}

func ratString(r *big.Rat) string {
	if r == nil {
		return "unset"
	}
	return r.RatString()
}

// validate is the deny-by-default gate: an intent missing an identity, a
// direction, or a positive size never reaches a venue.
// freshEnough refuses a signal whose source timestamp is outside
// Options.MaxSignalAge in either direction.
//
// UNBOUNDED IS STILL A CHOICE, not an oversight: a zero MaxSignalAge preserves
// the behaviour every caller had before this existed, so adding the field does
// not silently start refusing live traffic. The deployments set a real bound;
// see webhook-ingest's config, where the DEFAULT is a bound rather than zero.
func (t *Translator) freshEnough(in Intent) error {
	max := t.opt.MaxSignalAge
	if max <= 0 {
		return nil
	}
	if in.SourceTS == nil {
		if !t.opt.RequireSignalTS {
			// Unjudgeable, and allowed on purpose (see MaxSignalAge). The perimeter
			// counts these; this function's job is not to make that decision twice.
			return nil
		}
		return fmt.Errorf("%w: no source timestamp, and this deployment requires one — the age of "+
			"this decision cannot be established, so it cannot be shown to be current",
			ErrStaleSignal)
	}
	age := t.opt.Now().Sub(in.SourceTS.AsTime())
	if age > max {
		return fmt.Errorf("%w: fired %s ago, bound is %s. The strategy decided against a market "+
			"that has moved since; acting now would place its ORIGINAL size at a price it never saw",
			ErrStaleSignal, age.Round(time.Second), max)
	}
	if -age > max {
		return fmt.Errorf("%w: timestamped %s in the FUTURE, bound is %s. A clock that far out makes "+
			"the age of every signal from this sender unjudgeable — and a future-dated one would "+
			"never expire", ErrStaleSignal, (-age).Round(time.Second), max)
	}
	return nil
}

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
	// LEVERAGE IS REFUSED, NOT DROPPED (#240).
	//
	// It used to be parsed, bounds-checked against max_leverage, and written onto the
	// StrategySignal FACT — the immutable audit root — after which fanOut built an
	// order.v1.SubmitOrder that carries neither leverage nor margin_mode, because the
	// proto has neither field. A strategy asking for 10x got an UNLEVERED SPOT ORDER
	// while the audit trail permanently asserted a 10x position the fund never held.
	//
	// Refusing is not the end state; plumbing leverage to the venue adapters is (a
	// proto change, two adapters, and margin semantics). It is what stops the audit
	// trail lying TODAY, and it is reversible: delete this block when SubmitOrder can
	// actually carry it. A nil Leverage is ABSENT, which publishSignal already records
	// as 1 — unlevered — so it is accepted.
	if in.Leverage != nil && in.Leverage.Cmp(oneRat) != 0 {
		return fmt.Errorf("%w: leverage %s is not executable — this platform submits UNLEVERED "+
			"orders only (order.v1.SubmitOrder carries no leverage field), and accepting it would "+
			"record a levered position on the audit root that no venue was ever asked for. Send "+
			"leverage=1 and size the exposure yourself", ErrInvalidIntent, in.Leverage.RatString())
	}
	if in.MarginMode != signalpb.MarginMode_MARGIN_MODE_UNSPECIFIED {
		return fmt.Errorf("%w: margin_mode %s is not executable — same reason as leverage: "+
			"order.v1.SubmitOrder carries no margin mode, so the order placed would be spot "+
			"while the audit root claimed margin", ErrInvalidIntent, in.MarginMode)
	}
	return nil
}

// oneRat is unlevered — the only leverage this platform can actually execute.
var oneRat = big.NewRat(1, 1)

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

// fanOut emits one SubmitOrder per venue in the fund's allocation policy. The
// venues and the resolved base quantity are resolved by Emit — everything that can
// REFUSE the signal runs before the audit FACT is written.
func (t *Translator) fanOut(ctx context.Context, in Intent, tenant string, venues []VenueAllocation, baseQty Qty) ([]string, error) {
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
		if !qty.IsSet() || qty.Sign() <= 0 {
			continue // nothing to do at this venue (e.g. CLOSE with no position)
		}
		orderID := DeterministicID(in.SignalID, v.Venue)
		qtyD, qtyOK := toDec(qty.Rat())
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
		// MT-02 (#360): the WIRE subject carries the tenant so the broker can
		// route this command into that tenant's NATS account; event_type stays
		// the logical name. Without it a signal-originated order is delivered
		// into __system__ and the tenant's OMS never sees it — while the SAME
		// order placed through the gateway routes correctly. Two paths, one of
		// them silently wrong, is worse than neither working.
		//
		// `tenant` is TenantOf(in.FundID), resolved per signal — NOT a
		// service-level default. One webhook endpoint serves many funds, so a
		// per-service tenant would be the wrong one for all but a single fund.
		Subject:        bus.TenantRoutedSubject(tenant, SubjectSubmit),
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

// resolveQuantity turns a sized intent into an absolute base-asset quantity
// against live fund state, exactly (big.Rat, no float).
//
// This is the ONLY function that knows what Intent.Size means, and therefore the
// only place a Qty can come from. Any bound on the size has to be applied to what
// this returns — see Options.MaxQuantity.
func (t *Translator) resolveQuantity(ctx context.Context, in Intent) (Qty, error) {
	switch in.SizeType {
	case signalpb.SizeType_SIZE_TYPE_ABSOLUTE_QTY:
		return NewQty(in.Size), nil
	case signalpb.SizeType_SIZE_TYPE_QUOTE_NOTIONAL:
		price, err := t.price(ctx, in.InstrumentID)
		if err != nil {
			return Qty{}, err
		}
		return NewQty(new(big.Rat).Quo(in.Size, price)), nil // notional / price
	case signalpb.SizeType_SIZE_TYPE_PCT_OF_EQUITY:
		nav, err := t.opt.Equity.Equity(ctx, in.FundID)
		if err != nil {
			return Qty{}, err
		}
		price, err := t.price(ctx, in.InstrumentID)
		if err != nil {
			return Qty{}, err
		}
		// (pct/100 × NAV) / price
		frac := new(big.Rat).Quo(in.Size, big.NewRat(100, 1))
		notional := new(big.Rat).Mul(frac, nav)
		return NewQty(new(big.Rat).Quo(notional, price)), nil
	default:
		return Qty{}, fmt.Errorf("%w: unspecified size_type", ErrInvalidIntent)
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
func (t *Translator) legSizeAndSide(ctx context.Context, in Intent, baseQty Qty, v VenueAllocation) (orderpb.Side, Qty, error) {
	if in.Action == signalpb.SignalAction_SIGNAL_ACTION_CLOSE {
		pos, err := t.opt.Positions.Position(ctx, in.FundID, v.Venue, in.InstrumentID)
		if err != nil {
			return orderpb.Side_SIDE_UNSPECIFIED, Qty{}, err
		}
		if pos.Sign() == 0 {
			return orderpb.Side_SIDE_UNSPECIFIED, Qty{}, nil // flat — skip
		}
		if pos.Sign() > 0 {
			return orderpb.Side_SIDE_SELL, NewQty(pos), nil // long → sell to flatten
		}
		return orderpb.Side_SIDE_BUY, NewQty(new(big.Rat).Abs(pos)), nil // short → buy to flatten
	}
	side := orderpb.Side_SIDE_BUY
	if in.Action == signalpb.SignalAction_SIGNAL_ACTION_SELL {
		side = orderpb.Side_SIDE_SELL
	}
	// The weight is dimensionless and was audited at startup to sum to 1 across the
	// fund's legs (ValidateAllocation) — this multiply is a SPLIT, not a scale, and
	// unvalidated weights turned it into one (#240).
	return side, baseQty.Scale(v.Weight), nil
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
	// ErrSizeExceedsMax is returned when the RESOLVED quantity is above
	// Options.MaxQuantity. Distinct from ErrInvalidIntent because the intent is
	// well-formed — it is the platform that refuses to act on it at that size; the
	// webhook perimeter still maps both to 400.
	ErrSizeExceedsMax = errors.New("translate: resolved size exceeds the configured maximum")
	// ErrHalted is returned when the kill-switch is closed. Both front ends map it
	// to their own surface (the webhook perimeter answers 423 Locked).
	ErrHalted = errors.New("translate: trading halted")
	// ErrStaleSignal is returned when the intent's source timestamp is outside
	// Options.MaxSignalAge. Distinct from ErrInvalidIntent because the intent is
	// well-formed and was legitimate WHEN IT WAS SENT — what the platform refuses
	// is acting on it now.
	ErrStaleSignal = errors.New("translate: signal is too old to act on")
)
