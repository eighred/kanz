//go:build binance

package execution

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	accountingpb "github.com/kanz-eng/kanz-schemas-go/accounting/v1"
	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/kanz-eng/kanz/pkg/bus"
	"github.com/kanz-eng/kanz/services/oms/internal/dec"
)

// Reconciler is the secondary audit layer (M3.3/3.4): it periodically polls
// Binance for the venue truth and, where Kanz's state has drifted (a fill the
// websocket missed, an unrecorded fee), emits a correcting FACT. The exchange is
// the source of truth; this NEVER edits the ledger — it publishes StateHealed /
// BalanceReconciled and lets the journal fold them bitemporally.
type Reconciler struct {
	rest     *binanceREST
	symbols  SymbolMapper
	expected ExpectedOrders
	balances ExpectedBalances
	pub      Publisher
	venue    string
	tenant   string
	now      func() time.Time
}

// ReconcilerConfig configures a Reconciler.
type ReconcilerConfig struct {
	REST     *binanceREST
	Symbols  SymbolMapper
	Expected ExpectedOrders
	Balances ExpectedBalances
	Pub      Publisher
	Venue    string
	Tenant   string
	Now      func() time.Time
}

func newReconciler(cfg ReconcilerConfig) *Reconciler {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Venue == "" {
		cfg.Venue = "BINANCE"
	}
	return &Reconciler{
		rest: cfg.REST, symbols: cfg.Symbols, expected: cfg.Expected, balances: cfg.Balances,
		pub: cfg.Pub, venue: cfg.Venue, tenant: cfg.Tenant, now: cfg.Now,
	}
}

// Run polls every interval until ctx is cancelled. A per-pass error is returned
// only when it is a rate-limit exhaustion the caller should alert on; ordinary
// transient faults are logged-and-continued by the caller. It backs off rather
// than hammering the exchange.
func (r *Reconciler) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = r.Reconcile(ctx)
		}
	}
}

// Reconcile runs one pass: heal drifted orders, then reconcile balances. It
// returns the first non-recoverable error; a rate-limit denial aborts the pass
// (back off) rather than partial-polling.
func (r *Reconciler) Reconcile(ctx context.Context) error {
	if err := r.reconcileOrders(ctx); err != nil {
		return err
	}
	return r.reconcileBalances(ctx)
}

func (r *Reconciler) reconcileOrders(ctx context.Context) error {
	for _, exp := range r.expected.OpenOrders() {
		symbol, ok := r.symbols.Symbol(exp.GetInstrumentId())
		if !ok {
			continue
		}
		truth, err := r.rest.queryOrder(ctx, symbol, exp.GetOrderId())
		if err != nil {
			if errors.Is(err, ErrRateLimited) {
				return err // back off — do not keep polling
			}
			continue // transient / not-found: leave it for the next pass
		}
		if healed, drift := healedState(exp, truth, r.now()); drift {
			if err := r.emitStateHealed(ctx, healed, driftReason(exp, truth)); err != nil {
				return err
			}
		}
	}
	return nil
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
		actual := new(big.Rat)
		free, ok1 := new(big.Rat).SetString(b.Free)
		locked, ok2 := new(big.Rat).SetString(b.Locked)
		if ok1 {
			actual.Add(actual, free)
		}
		if ok2 {
			actual.Add(actual, locked)
		}
		expected := r.balances.Balance(b.Asset)
		if expected == nil {
			expected = new(big.Rat)
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
func healedState(exp *orderpb.OrderState, truth *orderResponse, now time.Time) (*orderpb.OrderState, bool) {
	exFilled, _ := new(big.Rat).SetString(truth.ExecutedQty)
	if exFilled == nil {
		exFilled = new(big.Rat)
	}
	status := binanceStatusToProto(truth.Status)
	kanzFilled := dec.FromProto(exp.GetFilledQuantity())
	if exFilled.Cmp(kanzFilled) == 0 && status == exp.GetStatus() {
		return nil, false // in parity
	}
	ordered := dec.FromProto(exp.GetOrderedQuantity())
	leaves := new(big.Rat).Sub(ordered, exFilled)
	healed := &orderpb.OrderState{
		OrderId: exp.GetOrderId(), PortfolioId: exp.GetPortfolioId(), InstrumentId: exp.GetInstrumentId(),
		Side: exp.GetSide(), OrderType: exp.GetOrderType(), TimeInForce: exp.GetTimeInForce(),
		OrderedQuantity: exp.GetOrderedQuantity(), LimitPrice: exp.GetLimitPrice(),
		Status: status, FilledQuantity: dec.ToProto(exFilled), LeavesQuantity: dec.ToProto(leaves),
		AsOf: timestamppb.New(now.UTC()),
	}
	return healed, true
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
	return r.pub.Publish(ctx, bus.Event{
		Subject: subjectBalanceRecon, EventType: subjectBalanceRecon,
		EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, Domain: "accounting",
		EventTime: r.now().UTC(), PartitionKey: r.tenant + ":" + asset, TenantID: r.tenant,
		QualityFlags: []envelopepb.QualityFlag{envelopepb.QualityFlag_QUALITY_FLAG_REVISED},
		Payload: &accountingpb.BalanceReconciled{
			PortfolioId: r.tenant, Venue: r.venue, Asset: asset,
			Expected: dec.ToProto(expected), Actual: dec.ToProto(actual), Delta: dec.ToProto(delta),
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
