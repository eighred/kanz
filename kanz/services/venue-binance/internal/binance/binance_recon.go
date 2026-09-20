package binance

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/pkg/bus"
)

// Reconciler is the secondary audit layer (M3.3/3.4): it periodically polls
// Binance for the venue truth and, where Kanz's state has drifted (a fill the
// websocket missed, an unrecorded fee), emits a correcting FACT. The exchange is
// the source of truth; this NEVER edits the ledger — it publishes StateHealed /
// BalanceReconciled as operator-facing discrepancy evidence (#1040).
type Reconciler struct {
	rest         *binanceREST
	symbols      SymbolMapper
	expected     ExpectedOrders
	balances     ExpectedBalances
	closes       PendingCloses
	closeTimeout time.Duration
	pub          Publisher
	venue        string
	tenant       string
	now          func() time.Time
	// onUnknownBalance is called for an asset whose expected balance is UNKNOWN
	// (#418), with the reason it is unknown (#1063). Nil ⇒ silent, which is only
	// right in a test: in production an operator must be able to tell
	// "reconciliation found nothing wrong" from "reconciliation could not check",
	// and those look identical otherwise — which is what both composition roots
	// shipped, because both left this nil for a year.
	onUnknownBalance func(asset, reason string)
	onReconcileError func(context.Context, string, error)
	// onCloseUnhealable is called for EVERY in-flight close this watchdog retains
	// WITHOUT asking the exchange anything (#1036), with the reason. Nil => silent,
	// which is only right in a test.
	//
	// IT IS THE ALERTABLE HALF, and until this existed there was no other. A close
	// the watchdog cannot even attempt was Resolved on exactly the same line as one
	// it had healed against venue truth, so "the seam ran and everything was
	// confirmed" and "the seam has never once reached the exchange" produced the
	// identical silence — the whole class of defect that hid an empty InstrumentID
	// on every close the venue adapter tracked. A drop means an order may still be
	// resting and fillable at the exchange while the book of record says CANCELLED.
	onCloseUnhealable func(orderID, instrumentID, reason string)
}

// ReconcilerConfig configures a Reconciler.
type ReconcilerConfig struct {
	REST     *binanceREST
	Symbols  SymbolMapper
	Expected ExpectedOrders
	Balances ExpectedBalances
	// Closes is the in-flight-close registry the healing loop drains. Nil ⇒ the
	// healing seam is disabled (order/balance reconciliation still runs).
	Closes PendingCloses
	// CloseTimeout is how long a close may stay unconfirmed before the healing
	// loop first queries it. <=0 ⇒ execution.DefaultCloseTimeout (the mandate's
	// trigger).
	CloseTimeout time.Duration
	Pub          Publisher
	Venue        string
	Tenant       string
	Now          func() time.Time
	// OnUnknownBalance is called for every asset this pass could not check, with
	// the reason (#1063). Nil ⇒ the skip is silent, and a silent skip is
	// indistinguishable from a clean comparison. The composition root supplies it
	// from execution.WorkerDeps, where the completeness guard makes omitting it a
	// visible decision rather than a field nobody typed.
	OnUnknownBalance func(asset, reason string)
	// OnReconcileError observes every returned periodic error; production supplies metrics and bounded logs.
	OnReconcileError func(context.Context, string, error)
	// OnCloseUnhealable is called for every in-flight close retained without a venue
	// query, with the reason (#1036). Nil => the drop is silent.
	OnCloseUnhealable func(orderID, instrumentID, reason string)
}

func newReconciler(cfg ReconcilerConfig) *Reconciler {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Venue == "" {
		cfg.Venue = "BINANCE"
	}
	if cfg.OnReconcileError == nil {
		cfg.OnReconcileError = execution.NewReconcileErrorObserver(nil, nil, cfg.Venue).Observe
	}
	if cfg.CloseTimeout <= 0 {
		cfg.CloseTimeout = execution.DefaultCloseTimeout
	}
	return &Reconciler{
		rest: cfg.REST, symbols: cfg.Symbols, expected: cfg.Expected, balances: cfg.Balances,
		closes: cfg.Closes, closeTimeout: cfg.CloseTimeout,
		pub: cfg.Pub, venue: cfg.Venue, tenant: cfg.Tenant, now: cfg.Now,
		onReconcileError: cfg.OnReconcileError,
		onUnknownBalance: cfg.OnUnknownBalance, onCloseUnhealable: cfg.OnCloseUnhealable,
	}
}

// Run polls until cancellation, reporting every returned error to the observer.
// Attempts retain the configured cadence; an error never triggers a tight retry.
func (r *Reconciler) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = execution.DefaultReconcileInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.onReconcileError(ctx, "reconcile", r.Reconcile(ctx))
			if ctx.Err() != nil {
				return
			}
		}
	}
}

// Reconcile runs one pass: heal drifted orders, then reconcile balances. It
// returns the first non-recoverable error; a rate-limit denial aborts the pass
// (back off) rather than partial-polling.
func (r *Reconciler) Reconcile(ctx context.Context) error {
	orderErr := r.reconcileOrders(ctx)
	if errors.Is(orderErr, ErrRateLimited) || ctx.Err() != nil {
		return orderErr
	}
	// An unavailable order must not blind independent balance observation.
	return errors.Join(orderErr, r.reconcileBalances(ctx))
}

// RunHealing observes overdue closes at a bounded cadence until cancellation.
func (r *Reconciler) RunHealing(ctx context.Context, tick time.Duration) {
	if r.closes == nil {
		return
	}
	if tick <= 0 {
		tick = execution.DefaultHealInterval
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.onReconcileError(ctx, "healing", r.HealClosures(ctx))
			if ctx.Err() != nil {
				return
			}
		}
	}
}

// HealClosures publishes terminal evidence only after an authoritative query.
// Working and unknown orders stay owned; timeout never authorizes a new trade.
func (r *Reconciler) HealClosures(ctx context.Context) error {
	if r.closes == nil {
		return nil
	}
	due, err := r.closes.DueCloses(ctx, r.now(), r.closeTimeout)
	if err != nil {
		return err
	}
	pass := &execution.ReconcilePassError{Total: len(due)}
	for _, ci := range due {
		pass.Checked++
		if reason := ci.Unhealable(); reason != "" {
			r.reportUnhealableClose(ci, reason)
			pass.FailOrder(ci.OrderID, execution.ErrReconcileEvidence)
			continue
		}
		symbol, ok := r.symbols.Symbol(ci.InstrumentID)
		if !ok {
			r.reportUnhealableClose(ci, execution.CloseDropUnmappedSymbol)
			pass.FailOrder(ci.OrderID, execution.ErrReconcileEvidence)
			continue
		}
		truth, qErr := r.rest.queryOrder(ctx, symbol, ci.OrderID)
		if qErr != nil {
			pass.FailOrder(ci.OrderID, qErr)
			if errors.Is(qErr, ErrRateLimited) || ctx.Err() != nil {
				break
			}
			continue
		}
		if truth == nil || truth.ClientOrderID != ci.OrderID {
			pass.FailOrder(ci.OrderID, execution.ErrReconcileEvidence)
			continue
		}
		if !binanceTerminal(truth.Status) {
			pass.FailOrder(ci.OrderID, execution.ErrCloseUnconfirmed)
			continue
		}
		healed, hErr := binanceHealedFromQuery(ci, truth, r.venue, r.now())
		if hErr != nil {
			pass.FailOrder(ci.OrderID, hErr)
			continue
		}
		if err := r.emitStateHealed(ctx, healed, "in-flight close confirmed terminal on venue query"); err != nil {
			pass.FailOrder(ci.OrderID, err)
			continue
		}
		if err := r.closes.Resolve(ctx, ci.OrderID); err != nil {
			pass.FailOrder(ci.OrderID, err)
		}
	}
	return pass.Result()
}

// reportUnhealableClose retains ownership and reports why no venue query was possible.
func (r *Reconciler) reportUnhealableClose(ci CloseIntent, reason string) {
	if r.onCloseUnhealable != nil {
		r.onCloseUnhealable(ci.OrderID, ci.InstrumentID, reason)
	}
}

// binanceTerminal reports whether a Binance order status is terminal.
func binanceTerminal(status string) bool {
	switch status {
	case "FILLED", "CANCELED", "EXPIRED", "REJECTED":
		return true
	default:
		return false
	}
}

// binanceHealedFromQuery builds the venue-truth state for a close the venue
// confirms terminal.
// The error return was added for the Decimal conversion (#94): ExecutedQty is an
// exchange string, and a token filled quantity in the trillions wraps through
// dec.ToProto — healing the order to a filled size that never traded.
func binanceHealedFromQuery(ci CloseIntent, truth *orderResponse, venue string, now time.Time) (*orderpb.OrderState, error) {
	filled, parseErr := dec.Exact(truth.ExecutedQty).Rat()
	if parseErr != nil || filled.Sign() < 0 {
		return nil, fmt.Errorf("%w: binance filled quantity", execution.ErrReconcileEvidence)
	}
	filledD, ok := dec.ToProtoExact(filled)
	if !ok {
		return nil, fmt.Errorf("binance: order %s filled quantity %q is not representable as a Decimal: %w",
			ci.OrderID, truth.ExecutedQty, execution.ErrReconcileEvidence)
	}
	return &orderpb.OrderState{
		OrderId: ci.OrderID, InstrumentId: ci.InstrumentID,
		Status: binanceStatusToProto(truth.Status), FilledQuantity: filledD,
		Venue: venue, AsOf: timestamppb.New(now.UTC()),
	}, nil
}

func (r *Reconciler) reconcileOrders(ctx context.Context) error {
	if r.expected == nil {
		return nil
	}
	orders := r.expected.OpenOrders()
	pass := &execution.ReconcilePassError{Total: len(orders)}
	for _, exp := range orders {
		if err := ctx.Err(); err != nil {
			pass.FailOrder(exp.GetOrderId(), err)
			break
		}
		pass.Checked++
		symbol, ok := r.symbols.Symbol(exp.GetInstrumentId())
		if !ok {
			pass.FailOrder(exp.GetOrderId(), execution.ErrReconcileEvidence)
			continue
		}
		truth, err := r.rest.queryOrder(ctx, symbol, exp.GetOrderId())
		if err != nil {
			pass.FailOrder(exp.GetOrderId(), err)
			if errors.Is(err, ErrRateLimited) || ctx.Err() != nil {
				break
			}
			continue
		}
		if truth == nil || truth.ClientOrderID != exp.GetOrderId() {
			pass.FailOrder(exp.GetOrderId(), execution.ErrReconcileEvidence)
			continue
		}
		healed, drift, err := healedState(exp, truth, r.now())
		if err != nil {
			pass.FailOrder(exp.GetOrderId(), err)
			continue
		}
		if drift {
			if err := r.emitStateHealed(ctx, healed, driftReason(exp, truth)); err != nil {
				pass.FailOrder(exp.GetOrderId(), err)
			}
		}
	}
	return pass.Result()
}

func (r *Reconciler) reconcileBalances(ctx context.Context) error {
	if r.balances == nil {
		return nil
	}
	acct, err := r.rest.account(ctx)
	if err != nil {
		return err
	}
	for _, b := range acct.Balances {
		free, freeErr := dec.Exact(b.Free).Rat()
		locked, lockedErr := dec.Exact(b.Locked).Rat()
		if freeErr != nil || lockedErr != nil {
			return fmt.Errorf("%w: binance balance", execution.ErrReconcileEvidence)
		}
		actual := new(big.Rat).Add(free, locked)
		// UNKNOWN SKIPS, IT DOES NOT COMPARE AGAINST ZERO (#418). Substituting
		// zero for a balance nobody has announced reports every asset the exchange
		// holds as a discrepancy — a break storm on the first run, which teaches an
		// operator to ignore this layer.
		expected, known, why := r.balances.Balance(b.Asset)
		if !known {
			r.unknownBalance(b.Asset, why)
			continue
		}
		if actual.Cmp(expected) == 0 {
			continue
		}
		if err := r.emitBalanceReconciled(ctx, b.Asset, expected, actual); err != nil {
			return err
		}
	}
	return nil
}

// healedState builds the venue-truth OrderState (Kanz static fields + exchange
// dynamic fields) and reports whether it drifted from the expected state. truth
// is the Binance order response; exp is Kanz's believed state.
// healedState compares venue truth against expected state. The error return
// carries only the Decimal-representation failure (#94); the bool means "there is
// drift worth emitting" and reusing it would drop a correction silently.
func healedState(exp *orderpb.OrderState, truth *orderResponse, now time.Time) (*orderpb.OrderState, bool, error) {
	exFilled, parseErr := dec.Exact(truth.ExecutedQty).Rat()
	if parseErr != nil || exFilled.Sign() < 0 {
		return nil, false, fmt.Errorf("%w: binance filled quantity", execution.ErrReconcileEvidence)
	}
	status := binanceStatusToProto(truth.Status)
	if status == orderpb.OrderStatus_ORDER_STATUS_UNSPECIFIED {
		return nil, false, execution.ErrReconcileEvidence
	}
	kanzFilled := dec.FromProto(exp.GetFilledQuantity())
	if exFilled.Cmp(kanzFilled) == 0 && status == exp.GetStatus() {
		return nil, false, nil // in parity
	}
	ordered := dec.FromProto(exp.GetOrderedQuantity())
	leaves := new(big.Rat).Sub(ordered, exFilled)
	// EXACT, NEVER ROUNDED: these are observed sizes, not journal commands. The
	// operator must see the precise discrepancy, including sub-scale quantities.
	filledD, fok := dec.ToProtoExact(exFilled)
	leavesD, lok := dec.ToProtoExact(leaves)
	if !fok || !lok {
		return nil, false, fmt.Errorf("binance: order %s healed quantities are not representable as a Decimal "+
			"(filled=%s leaves=%s): %w", exp.GetOrderId(), exFilled.FloatString(8), leaves.FloatString(8), execution.ErrReconcileEvidence)
	}
	healed := &orderpb.OrderState{
		OrderId: exp.GetOrderId(), PortfolioId: exp.GetPortfolioId(), InstrumentId: exp.GetInstrumentId(),
		Side: exp.GetSide(), OrderType: exp.GetOrderType(), TimeInForce: exp.GetTimeInForce(),
		OrderedQuantity: exp.GetOrderedQuantity(), LimitPrice: exp.GetLimitPrice(),
		Status: status, FilledQuantity: filledD, LeavesQuantity: leavesD,
		AsOf: timestamppb.New(now.UTC()),
	}
	return healed, true, nil
}

func (r *Reconciler) emitStateHealed(ctx context.Context, state *orderpb.OrderState, reason string) error {
	return r.pub.Publish(ctx, bus.Event{
		Subject: subjectStateHealed, EventType: subjectStateHealed,
		EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, Domain: "order",
		EventTime: r.now().UTC(), PartitionKey: state.GetOrderId(), TenantID: r.tenant,
		CausationID:  state.GetOrderId(),
		QualityFlags: []envelopepb.QualityFlag{envelopepb.QualityFlag_QUALITY_FLAG_REVISED},
		Payload: &orderpb.StateHealed{
			OrderId: state.GetOrderId(), Venue: r.venue, State: state,
			Reason: reason, DetectedAt: timestamppb.New(r.now().UTC()),
		},
	})
}

func (r *Reconciler) emitBalanceReconciled(ctx context.Context, asset string, expected, actual *big.Rat) error {
	delta := new(big.Rat).Sub(actual, expected)
	// EXACT, NEVER ROUNDED (#94) — the same reasoning as the OKX reconciler. These
	// three numbers ARE the balance break; a wrapped Delta does not understate it,
	// it reports a different break entirely, and can make a real one look like a
	// match. Refusing to publish is the only safe failure.
	expectedD, eok := dec.ToProtoExact(expected)
	actualD, aok := dec.ToProtoExact(actual)
	deltaD, dok := dec.ToProtoExact(delta)
	if !eok || !aok || !dok {
		return fmt.Errorf("binance: %s balance reconciliation is not representable as a Decimal "+
			"(expected=%s actual=%s) — refusing to publish a break with fabricated figures: %w",
			asset, expected.FloatString(8), actual.FloatString(8), execution.ErrReconcileEvidence)
	}
	return r.pub.Publish(ctx, bus.Event{
		Subject: subjectBalanceRecon, EventType: subjectBalanceRecon,
		EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, Domain: "accounting",
		EventTime: r.now().UTC(), PartitionKey: r.tenant + ":" + asset, TenantID: r.tenant,
		QualityFlags: []envelopepb.QualityFlag{envelopepb.QualityFlag_QUALITY_FLAG_REVISED},
		Payload: &accountingpb.BalanceReconciled{
			PortfolioId: r.tenant, Venue: r.venue, Asset: asset,
			Expected: expectedD, Actual: actualD, Delta: deltaD,
			DetectedAt: timestamppb.New(r.now().UTC()),
		},
	})
}

func driftReason(exp *orderpb.OrderState, truth *orderResponse) string {
	return fmt.Sprintf("venue truth executedQty=%s status=%s vs internal filled=%s status=%v",
		truth.ExecutedQty, truth.Status, dec.FromProto(exp.GetFilledQuantity()).FloatString(8), exp.GetStatus())
}

func binanceStatusToProto(s string) orderpb.OrderStatus {
	switch s {
	case "NEW":
		return orderpb.OrderStatus_ORDER_STATUS_ROUTED
	case "PARTIALLY_FILLED":
		return orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED
	case "FILLED":
		return orderpb.OrderStatus_ORDER_STATUS_FILLED
	case "CANCELED":
		return orderpb.OrderStatus_ORDER_STATUS_CANCELLED
	case "EXPIRED":
		return orderpb.OrderStatus_ORDER_STATUS_EXPIRED
	case "REJECTED":
		return orderpb.OrderStatus_ORDER_STATUS_REJECTED
	default:
		return orderpb.OrderStatus_ORDER_STATUS_UNSPECIFIED
	}
}

// unknownBalance reports an asset this adapter cannot check, because Kanz has no
// announced balance for it (#418).
//
// It is a distinct signal from a reconciliation BREAK, and the difference is the
// point: a break means the two sides disagree, this means one side is missing.
// Collapsing them would let "we have never been told what we hold" arrive on the
// same dashboard as "the exchange and our books differ", and only one of those is
// an incident. It is also why this is NOT published as a BalanceReconciled FACT:
// that message carries expected, actual and delta, and the whole condition here
// is that expected does not exist — emitting one means inventing expected=0,
// which is the fabrication #418 removed from this exact loop, folded bitemporally
// into the book of record rather than merely shown on a dashboard.
//
// THE REASON RIDES ALONG (#1063), because "the cash spine has never delivered"
// and "what it delivered has gone stale" are different repairs.
func (r *Reconciler) unknownBalance(asset, reason string) {
	if r.onUnknownBalance != nil {
		// NORMALISED HERE, at the one place both reconcilers pass through, so the
		// label set is fixed by execution.NamedBalanceUnknown rather than by
		// whatever string a seam happened to return. A venue adapter must not be
		// able to widen a Prometheus label by writing a new word.
		r.onUnknownBalance(asset, execution.NamedBalanceUnknown(reason))
	}
}
