package okx

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

// OKXReconciler is the OKX secondary audit layer — the analog of the Binance
// Reconciler. It polls OKX for the venue truth and, where Kanz's state has
// drifted, emits a correcting FACT (StateHealed / BalanceReconciled). It never
// edits state; audit records the FACTs as operator-facing evidence (#1040).
type OKXReconciler struct {
	rest         *okxREST
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

// OKXReconcilerConfig configures an OKXReconciler.
type OKXReconcilerConfig struct {
	REST     *okxREST
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

func newOKXReconciler(cfg OKXReconcilerConfig) *OKXReconciler {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Venue == "" {
		cfg.Venue = "OKX"
	}
	if cfg.OnReconcileError == nil {
		cfg.OnReconcileError = execution.NewReconcileErrorObserver(nil, nil, cfg.Venue).Observe
	}
	if cfg.CloseTimeout <= 0 {
		cfg.CloseTimeout = execution.DefaultCloseTimeout
	}
	return &OKXReconciler{
		rest: cfg.REST, symbols: cfg.Symbols, expected: cfg.Expected, balances: cfg.Balances,
		closes: cfg.Closes, closeTimeout: cfg.CloseTimeout,
		pub: cfg.Pub, venue: cfg.Venue, tenant: cfg.Tenant, now: cfg.Now,
		onReconcileError: cfg.OnReconcileError,
		onUnknownBalance: cfg.OnUnknownBalance, onCloseUnhealable: cfg.OnCloseUnhealable,
	}
}

// Run polls every interval until ctx is cancelled.
func (r *OKXReconciler) Run(ctx context.Context, interval time.Duration) {
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

// Reconcile runs one pass: heal drifted orders, then reconcile balances.
func (r *OKXReconciler) Reconcile(ctx context.Context) error {
	orderErr := r.reconcileOrders(ctx)
	if errors.Is(orderErr, ErrRateLimited) || ctx.Err() != nil {
		return orderErr
	}
	// An unavailable order must not blind independent balance observation.
	return errors.Join(orderErr, r.reconcileBalances(ctx))
}

// RunHealing observes overdue closes at a bounded cadence until cancellation.
func (r *OKXReconciler) RunHealing(ctx context.Context, tick time.Duration) {
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
func (r *OKXReconciler) HealClosures(ctx context.Context) error {
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
		o, qErr := r.rest.queryOrder(ctx, symbol, ci.OrderID)
		if qErr != nil {
			pass.FailOrder(ci.OrderID, qErr)
			if errors.Is(qErr, ErrRateLimited) || ctx.Err() != nil {
				break
			}
			continue
		}
		if o == nil || o.ClOrdID != ci.OrderID {
			pass.FailOrder(ci.OrderID, execution.ErrReconcileEvidence)
			continue
		}
		if !okxTerminal(o.State) {
			pass.FailOrder(ci.OrderID, execution.ErrCloseUnconfirmed)
			continue
		}
		healed, _, hErr := okxHealedFromQuery(ci, o, r.now())
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
func (r *OKXReconciler) reportUnhealableClose(ci CloseIntent, reason string) {
	if r.onCloseUnhealable != nil {
		r.onCloseUnhealable(ci.OrderID, ci.InstrumentID, reason)
	}
}

// okxTerminal reports whether an OKX order state is terminal.
func okxTerminal(state string) bool {
	return state == "filled" || state == "canceled" || state == "mmp_canceled"
}

// okxHealedFromQuery builds the venue-truth state for a close the venue confirms
// terminal.
//
// The error return exists for the Decimal conversion alone (#94): the bool means
// "there is drift worth emitting", so it cannot also carry "this number could not
// be represented" — reusing it would report a healed order as no-drift and drop
// the correction silently, which is the failure this whole reconciler exists to
// catch.
func okxHealedFromQuery(ci CloseIntent, o *okxOrder, now time.Time) (*orderpb.OrderState, bool, error) {
	filled, parseErr := dec.Exact(o.AccFillSz).Rat()
	if parseErr != nil || filled.Sign() < 0 {
		return nil, false, fmt.Errorf("%w: okx filled quantity", execution.ErrReconcileEvidence)
	}
	// EXACT, NEVER ROUNDED. AccFillSz is an exchange string; a token filled
	// quantity in the trillions wraps through dec.ToProto and this FACT would
	// then heal the order to a filled size that never traded.
	filledD, ok := dec.ToProtoExact(filled)
	if !ok {
		return nil, false, fmt.Errorf("okx: order %s filled quantity %q is not representable as a Decimal: %w",
			ci.OrderID, o.AccFillSz, execution.ErrReconcileEvidence)
	}
	return &orderpb.OrderState{
		OrderId: ci.OrderID, InstrumentId: ci.InstrumentID,
		Status: okxStateToProto(o.State), FilledQuantity: filledD,
		Venue: "OKX", AsOf: timestamppb.New(now.UTC()),
	}, true, nil
}

func (r *OKXReconciler) reconcileOrders(ctx context.Context) error {
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
		truth, err := r.queryByType(ctx, symbol, exp)
		if err != nil {
			pass.FailOrder(exp.GetOrderId(), err)
			if errors.Is(err, ErrRateLimited) || ctx.Err() != nil {
				break
			}
			continue
		}
		if truth == nil {
			continue
		} // queryByType explicitly confirmed a resting conditional order
		if truth.ClOrdID != exp.GetOrderId() && truth.AlgoClOrdID != exp.GetOrderId() {
			pass.FailOrder(exp.GetOrderId(), execution.ErrReconcileEvidence)
			continue
		}
		healed, drift, err := okxHealedState(exp, truth, r.now())
		if err != nil {
			pass.FailOrder(exp.GetOrderId(), err)
			continue
		}
		if drift {
			if err := r.emitStateHealed(ctx, healed, okxDriftReason(exp, truth)); err != nil {
				pass.FailOrder(exp.GetOrderId(), err)
			}
		}
	}
	return pass.Result()
}

func (r *OKXReconciler) reconcileBalances(ctx context.Context) error {
	if r.balances == nil {
		return nil
	}
	bals, err := r.rest.balances(ctx)
	if err != nil {
		return err
	}
	for ccy, cashBal := range bals {
		actual, parseErr := dec.Exact(cashBal).Rat()
		if parseErr != nil {
			return fmt.Errorf("%w: okx balance", execution.ErrReconcileEvidence)
		}
		// UNKNOWN SKIPS, IT DOES NOT COMPARE AGAINST ZERO (#418). Substituting
		// zero for a balance nobody has announced reports every asset the exchange
		// holds as a discrepancy — a break storm on the first run, which teaches an
		// operator to ignore this layer.
		expected, known, why := r.balances.Balance(ccy)
		if !known {
			r.unknownBalance(ccy, why)
			continue
		}
		if actual.Cmp(expected) == 0 {
			continue
		}
		if err := r.emitBalanceReconciled(ctx, ccy, expected, actual); err != nil {
			return err
		}
	}
	return nil
}

// okxHealedState compares venue truth against expected state. The error return
// carries only the Decimal-representation failure (#94) — see okxHealedFromQuery
// for why it cannot share the drift bool.
func okxHealedState(exp *orderpb.OrderState, o *okxOrder, now time.Time) (*orderpb.OrderState, bool, error) {
	exFilled, parseErr := dec.Exact(o.AccFillSz).Rat()
	if parseErr != nil || exFilled.Sign() < 0 {
		return nil, false, fmt.Errorf("%w: okx filled quantity", execution.ErrReconcileEvidence)
	}
	status := okxStateToProto(o.State)
	if status == orderpb.OrderStatus_ORDER_STATUS_UNSPECIFIED {
		return nil, false, execution.ErrReconcileEvidence
	}
	if exFilled.Cmp(dec.FromProto(exp.GetFilledQuantity())) == 0 && status == exp.GetStatus() {
		return nil, false, nil
	}
	ordered := dec.FromProto(exp.GetOrderedQuantity())
	leaves := new(big.Rat).Sub(ordered, exFilled)
	// EXACT, NEVER ROUNDED. These two become the order's filled and leaves sizes
	// in the observed FACT; a wrapped or rounded value would hide the actual
	// discrepancy from the investigating operator.
	filledD, fok := dec.ToProtoExact(exFilled)
	leavesD, lok := dec.ToProtoExact(leaves)
	if !fok || !lok {
		return nil, false, fmt.Errorf("okx: order %s healed quantities are not representable as a Decimal "+
			"(filled=%s leaves=%s): %w", exp.GetOrderId(), exFilled.FloatString(8), leaves.FloatString(8), execution.ErrReconcileEvidence)
	}
	return &orderpb.OrderState{
		OrderId: exp.GetOrderId(), PortfolioId: exp.GetPortfolioId(), InstrumentId: exp.GetInstrumentId(),
		Side: exp.GetSide(), OrderType: exp.GetOrderType(), TimeInForce: exp.GetTimeInForce(),
		OrderedQuantity: exp.GetOrderedQuantity(), LimitPrice: exp.GetLimitPrice(),
		Status: status, FilledQuantity: filledD, LeavesQuantity: leavesD,
		Venue: "OKX", AsOf: timestamppb.New(now.UTC()),
	}, true, nil
}

func (r *OKXReconciler) emitStateHealed(ctx context.Context, state *orderpb.OrderState, reason string) error {
	return r.pub.Publish(ctx, bus.Event{
		Subject: SubjectStateHealed, EventType: SubjectStateHealed,
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

func (r *OKXReconciler) emitBalanceReconciled(ctx context.Context, asset string, expected, actual *big.Rat) error {
	delta := new(big.Rat).Sub(actual, expected)
	// EXACT, NEVER ROUNDED (#94). These three numbers ARE the balance break — the
	// figures an operator reads to decide whether the book or the exchange is
	// wrong. dec.ToProto wraps above ~92.2 billion units at scale 8, which a
	// token balance reaches, and a wrapped Delta does not report a smaller break:
	// it reports a DIFFERENT one, and can turn a real break into an apparent
	// match. Refusing to publish is the only safe failure here.
	expectedD, eok := dec.ToProtoExact(expected)
	actualD, aok := dec.ToProtoExact(actual)
	deltaD, dok := dec.ToProtoExact(delta)
	if !eok || !aok || !dok {
		return fmt.Errorf("okx: %s balance reconciliation is not representable as a Decimal "+
			"(expected=%s actual=%s) — refusing to publish a break with fabricated figures: %w",
			asset, expected.FloatString(8), actual.FloatString(8), execution.ErrReconcileEvidence)
	}
	return r.pub.Publish(ctx, bus.Event{
		Subject: SubjectBalanceRecon, EventType: SubjectBalanceRecon,
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

func okxDriftReason(exp *orderpb.OrderState, o *okxOrder) string {
	return fmt.Sprintf("okx truth accFillSz=%s state=%s vs internal filled=%s status=%v",
		o.AccFillSz, o.State, dec.FromProto(exp.GetFilledQuantity()).FloatString(8), exp.GetStatus())
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
func (r *OKXReconciler) unknownBalance(asset, reason string) {
	if r.onUnknownBalance != nil {
		// NORMALISED HERE, at the one place both reconcilers pass through, so the
		// label set is fixed by execution.NamedBalanceUnknown rather than by
		// whatever string a seam happened to return. A venue adapter must not be
		// able to widen a Prometheus label by writing a new word.
		r.onUnknownBalance(asset, execution.NamedBalanceUnknown(reason))
	}
}

// queryByType asks the venue about an order on the endpoint that order actually
// lives on (#485).
//
// # Why this is not one call
//
// A CONDITIONAL order is invisible to /trade/order — it answers 51603 "Order
// does not exist" for a stop that is resting, live, and perfectly healthy.
// Reconciliation used to take that as a query failure and `continue`, so a stop
// was never reconciled at all: cancelled at the venue by hand, it would sit open
// in this platform's books indefinitely, and nothing would ever disagree.
//
// # Three outcomes, and the middle one is the reason for the nil
//
//   - a REGULAR order, or a triggered stop's resulting order: the venue's truth,
//     healed by the existing path;
//   - a stop still WAITING (state "live" or "pause"): nil, meaning "the venue
//     agrees this is resting". There is nothing to heal, and inventing a regular
//     order shape for it would compare a trigger against a fill;
//   - a stop that FIRED: the algo record no longer describes the live order, so
//     this follows through to the order the trigger created. Fills reach the
//     platform through the user-data stream keyed on algoClOrdId; this is the
//     backstop for a missed stream event.
func (r *OKXReconciler) queryByType(ctx context.Context, instID string, exp *orderpb.OrderState) (*okxOrder, error) {
	if !isAlgoOrder(exp.GetOrderType()) {
		return r.rest.queryOrder(ctx, instID, exp.GetOrderId())
	}
	algo, err := r.rest.queryAlgoOrder(ctx, exp.GetOrderId())
	if err != nil {
		return nil, err
	}
	switch algo.State {
	case "live", "pause":
		return nil, nil
	case "effective":
		return r.rest.queryTriggeredOrder(ctx, instID, exp.GetOrderId())
	case "canceled", "order_failed":
		// "canceled", "order_failed" — the stop will never fire. Presented in the
		// regular order's shape so the one healing path handles it: no fills, and
		// a state the aggregate already knows how to make terminal.
		return &okxOrder{
			ClOrdID: exp.GetOrderId(), State: "canceled",
			Sz: algo.Sz, AccFillSz: "0", AlgoClOrdID: exp.GetOrderId(),
		}, nil
	default:
		return nil, execution.ErrReconcileEvidence
	}
}
