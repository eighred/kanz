package binance

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

	"github.com/kanz-eng/kanz/internal/dec"
	"github.com/kanz-eng/kanz/pkg/bus"
)

// Reconciler is the secondary audit layer (M3.3/3.4): it periodically polls
// Binance for the venue truth and, where Kanz's state has drifted (a fill the
// websocket missed, an unrecorded fee), emits a correcting FACT. The exchange is
// the source of truth; this NEVER edits the ledger — it publishes StateHealed /
// BalanceReconciled and lets the journal fold them bitemporally.
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
	// loop force-clears it. <=0 ⇒ 1500ms (the mandate's trigger).
	CloseTimeout time.Duration
	Pub          Publisher
	Venue        string
	Tenant       string
	Now          func() time.Time
}

func newReconciler(cfg ReconcilerConfig) *Reconciler {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Venue == "" {
		cfg.Venue = "BINANCE"
	}
	if cfg.CloseTimeout <= 0 {
		cfg.CloseTimeout = 1500 * time.Millisecond
	}
	return &Reconciler{
		rest: cfg.REST, symbols: cfg.Symbols, expected: cfg.Expected, balances: cfg.Balances,
		closes: cfg.Closes, closeTimeout: cfg.CloseTimeout,
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

// RunHealing drives the In-Flight Certainty watchdog on a fast tick (independent
// of the slower order/balance cron): every tick it force-resolves any close that
// has stayed unconfirmed past CloseTimeout so the ledger never freezes. No-op
// when no close registry is configured. The Binance half of the shared seam —
// identical semantics to OKXReconciler.RunHealing.
func (r *Reconciler) RunHealing(ctx context.Context, tick time.Duration) {
	if r.closes == nil {
		return
	}
	if tick <= 0 {
		tick = 500 * time.Millisecond
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = r.HealClosures(ctx)
		}
	}
}

// HealClosures resolves every close past the timeout. For each: query the venue
// by clOrdId; if the venue confirms it terminal, emit a StateHealed carrying that
// truth. If the order is still working or the venue is unresponsive, force-clear
// it (StateHealed → CANCELLED) and sweep any residual exposure with an aggressive
// market order, then reconcile balances so a BalanceReconciled FACT re-anchors
// the ledger from real venue truth — never a fabricated balance.
func (r *Reconciler) HealClosures(ctx context.Context) error {
	if r.closes == nil {
		return nil
	}
	swept := false
	for _, ci := range r.closes.DueCloses(r.now(), r.closeTimeout) {
		symbol, ok := r.symbols.Symbol(ci.InstrumentID)
		if !ok {
			r.closes.Resolve(ci.OrderID) // untradeable here — stop watching it
			continue
		}
		truth, qErr := r.rest.queryOrder(ctx, symbol, ci.OrderID)
		if qErr == nil && binanceTerminal(truth.Status) {
			// Venue confirms the close landed — adopt its truth and stop.
			if err := r.emitStateHealed(ctx, binanceHealedFromQuery(ci, truth, r.venue, r.now()),
				"in-flight close confirmed terminal on venue query"); err != nil {
				return err
			}
			r.closes.Resolve(ci.OrderID)
			continue
		}
		// Stuck or unresponsive: force-clear and sweep so nothing freezes.
		if err := r.forceSweep(ctx, ci, symbol); err != nil {
			return err
		}
		swept = true
		r.closes.Resolve(ci.OrderID)
	}
	if swept {
		// Re-anchor balances from real venue truth after the sweep(s).
		return r.reconcileBalances(ctx)
	}
	return nil
}

// forceSweep force-clears a stuck close and flattens whatever exposure it left
// open. The sweep is best-effort: its failure is recorded in the reason but must
// not block the force-clear — the ledger must progress. The sweep's REAL fills
// arrive via the user-data stream / the next balance pass; none is fabricated.
// A close carrying no residual exposure (a cancelled resting order — see
// CloseIntent) force-clears without a sweep.
func (r *Reconciler) forceSweep(ctx context.Context, ci CloseIntent, symbol string) error {
	reason := "in-flight close timeout (>" + r.closeTimeout.String() + "); force-cleared"
	if ci.Leaves != nil && ci.Leaves.Sign() > 0 && ci.SweepSide != orderpb.Side_SIDE_UNSPECIFIED {
		side, sErr := binanceSide(ci.SweepSide)
		if sErr == nil {
			qty := formatDec(dec.ToProto(ci.Leaves))
			if _, err := r.rest.sweepMarket(ctx, symbol, side, qty, "heal-"+ci.OrderID); err != nil {
				reason += "; sweep error: " + err.Error()
			} else {
				reason += "; swept residual " + ci.Leaves.FloatString(8) + " via market " + side
			}
		}
	}
	cleared := &orderpb.OrderState{
		OrderId: ci.OrderID, InstrumentId: ci.InstrumentID,
		Status: orderpb.OrderStatus_ORDER_STATUS_CANCELLED,
		Venue:  r.venue, AsOf: timestamppb.New(r.now().UTC()),
	}
	return r.emitStateHealed(ctx, cleared, reason)
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
func binanceHealedFromQuery(ci CloseIntent, truth *orderResponse, venue string, now time.Time) *orderpb.OrderState {
	filled, _ := new(big.Rat).SetString(truth.ExecutedQty)
	if filled == nil {
		filled = new(big.Rat)
	}
	return &orderpb.OrderState{
		OrderId: ci.OrderID, InstrumentId: ci.InstrumentID,
		Status: binanceStatusToProto(truth.Status), FilledQuantity: dec.ToProto(filled),
		Venue: venue, AsOf: timestamppb.New(now.UTC()),
	}
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
