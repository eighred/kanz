//go:build okx

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

// OKXReconciler is the OKX secondary audit layer — the analog of the Binance
// Reconciler. It polls OKX for the venue truth and, where Kanz's state has
// drifted, emits a correcting FACT (StateHealed / BalanceReconciled). It never
// edits state; the journal folds the FACTs to restore parity bitemporally.
type OKXReconciler struct {
	rest     *okxREST
	symbols  SymbolMapper
	expected ExpectedOrders
	balances ExpectedBalances
	pub      Publisher
	venue    string
	tenant   string
	now      func() time.Time
}

// OKXReconcilerConfig configures an OKXReconciler.
type OKXReconcilerConfig struct {
	REST     *okxREST
	Symbols  SymbolMapper
	Expected ExpectedOrders
	Balances ExpectedBalances
	Pub      Publisher
	Venue    string
	Tenant   string
	Now      func() time.Time
}

func newOKXReconciler(cfg OKXReconcilerConfig) *OKXReconciler {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Venue == "" {
		cfg.Venue = "OKX"
	}
	return &OKXReconciler{
		rest: cfg.REST, symbols: cfg.Symbols, expected: cfg.Expected, balances: cfg.Balances,
		pub: cfg.Pub, venue: cfg.Venue, tenant: cfg.Tenant, now: cfg.Now,
	}
}

// Run polls every interval until ctx is cancelled.
func (r *OKXReconciler) Run(ctx context.Context, interval time.Duration) {
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

// Reconcile runs one pass: heal drifted orders, then reconcile balances.
func (r *OKXReconciler) Reconcile(ctx context.Context) error {
	if err := r.reconcileOrders(ctx); err != nil {
		return err
	}
	return r.reconcileBalances(ctx)
}

func (r *OKXReconciler) reconcileOrders(ctx context.Context) error {
	for _, exp := range r.expected.OpenOrders() {
		instID, ok := r.symbols.Symbol(exp.GetInstrumentId())
		if !ok {
			continue
		}
		o, err := r.rest.queryOrder(ctx, instID, exp.GetOrderId())
		if err != nil {
			if errors.Is(err, ErrRateLimited) {
				return err
			}
			continue
		}
		if healed, drift := okxHealedState(exp, o, r.now()); drift {
			if err := r.emitStateHealed(ctx, healed, okxDriftReason(exp, o)); err != nil {
				return err
			}
		}
	}
	return nil
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
		actual, ok := new(big.Rat).SetString(cashBal)
		if !ok {
			continue
		}
		expected := r.balances.Balance(ccy)
		if expected == nil {
			expected = new(big.Rat)
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

func okxHealedState(exp *orderpb.OrderState, o *okxOrder, now time.Time) (*orderpb.OrderState, bool) {
	exFilled, _ := new(big.Rat).SetString(o.AccFillSz)
	if exFilled == nil {
		exFilled = new(big.Rat)
	}
	status := okxStateToProto(o.State)
	if exFilled.Cmp(dec.FromProto(exp.GetFilledQuantity())) == 0 && status == exp.GetStatus() {
		return nil, false
	}
	ordered := dec.FromProto(exp.GetOrderedQuantity())
	leaves := new(big.Rat).Sub(ordered, exFilled)
	return &orderpb.OrderState{
		OrderId: exp.GetOrderId(), PortfolioId: exp.GetPortfolioId(), InstrumentId: exp.GetInstrumentId(),
		Side: exp.GetSide(), OrderType: exp.GetOrderType(), TimeInForce: exp.GetTimeInForce(),
		OrderedQuantity: exp.GetOrderedQuantity(), LimitPrice: exp.GetLimitPrice(),
		Status: status, FilledQuantity: dec.ToProto(exFilled), LeavesQuantity: dec.ToProto(leaves),
		Venue: "OKX", AsOf: timestamppb.New(now.UTC()),
	}, true
}

func (r *OKXReconciler) emitStateHealed(ctx context.Context, state *orderpb.OrderState, reason string) error {
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

func (r *OKXReconciler) emitBalanceReconciled(ctx context.Context, asset string, expected, actual *big.Rat) error {
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

func okxDriftReason(exp *orderpb.OrderState, o *okxOrder) string {
	return fmt.Sprintf("okx truth accFillSz=%s state=%s vs internal filled=%s status=%v",
		o.AccFillSz, o.State, dec.FromProto(exp.GetFilledQuantity()).FloatString(8), exp.GetStatus())
}
