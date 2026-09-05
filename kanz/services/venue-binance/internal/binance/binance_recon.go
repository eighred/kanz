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
	// onUnknownBalance is called for an asset whose expected balance is UNKNOWN
	// (#418). Nil ⇒ silent, which is only right in a test: in production an
	// operator must be able to tell "reconciliation found nothing wrong" from
	// "reconciliation could not check", and those look identical otherwise.
	onUnknownBalance func(asset string)
	// onCloseUnhealable is called for EVERY in-flight close this watchdog dropped
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
	// loop force-clears it. <=0 ⇒ execution.DefaultCloseTimeout (the mandate's
	// trigger).
	CloseTimeout     time.Duration
	Pub              Publisher
	Venue            string
	Tenant           string
	Now              func() time.Time
	OnUnknownBalance func(asset string)
	// OnCloseUnhealable is called for every in-flight close dropped without a venue
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
	if cfg.CloseTimeout <= 0 {
		cfg.CloseTimeout = execution.DefaultCloseTimeout
	}
	return &Reconciler{
		rest: cfg.REST, symbols: cfg.Symbols, expected: cfg.Expected, balances: cfg.Balances,
		closes: cfg.Closes, closeTimeout: cfg.CloseTimeout,
		pub: cfg.Pub, venue: cfg.Venue, tenant: cfg.Tenant, now: cfg.Now,
		onUnknownBalance: cfg.OnUnknownBalance, onCloseUnhealable: cfg.OnCloseUnhealable,
	}
}

// Run polls every interval until ctx is cancelled. A per-pass error is returned
// only when it is a rate-limit exhaustion the caller should alert on; ordinary
// transient faults are logged-and-continued by the caller. It backs off rather
// than hammering the exchange.
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
		tick = execution.DefaultHealInterval
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
		// TWO DIFFERENT ANSWERS, COUNTED SEPARATELY (#1036). A malformed intent is a
		// writer defect and used to be indistinguishable from "untradeable here",
		// which is how an empty InstrumentID on every close the venue adapter
		// tracked stayed invisible: Symbol("") misses, and the miss read as
		// configuration.
		if reason := ci.Unhealable(); reason != "" {
			r.dropUnhealableClose(ci, reason)
			continue
		}
		symbol, ok := r.symbols.Symbol(ci.InstrumentID)
		if !ok {
			// Untradeable here — stop watching it, and SAY SO. This is still a close
			// nobody asked the exchange about.
			r.dropUnhealableClose(ci, execution.CloseDropUnmappedSymbol)
			continue
		}
		truth, qErr := r.rest.queryOrder(ctx, symbol, ci.OrderID)
		if qErr == nil && binanceTerminal(truth.Status) {
			// Venue confirms the close landed — adopt its truth and stop.
			healed, hErr := binanceHealedFromQuery(ci, truth, r.venue, r.now())
			if hErr != nil {
				return hErr
			}
			if err := r.emitStateHealed(ctx, healed,
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

// dropUnhealableClose stops watching a close the watchdog could not turn into a
// venue question, and says so.
//
// IT IS NOT A RESOLUTION AND MUST NOT LOOK LIKE ONE (#1036). The order may still
// be resting and fillable at the exchange while the OMS, the position book, risk,
// compliance and the IBOR all have it CANCELLED — and CANCELLED is terminal, so
// no sweep and no resume will look at it again. The intent IS dropped rather than
// retried forever, because it is unhealable by construction and a growing
// registry of questions nobody can ask helps no one; the counter and the ERROR
// log are what an operator acts on.
func (r *Reconciler) dropUnhealableClose(ci CloseIntent, reason string) {
	if r.onCloseUnhealable != nil {
		r.onCloseUnhealable(ci.OrderID, ci.InstrumentID, reason)
	}
	r.closes.Resolve(ci.OrderID)
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
			// SCALED, NOT WRAPPING (#94) — the same reasoning as the OKX sweep. This
			// quantity becomes a live MARKET order; dec.ToProto wraps above ~92.2
			// billion units at scale 8, which a meme-coin residual reaches, and the
			// order would be placed for a size the platform fabricated.
			scaled, ok := dec.ToProtoScaled(ci.Leaves)
			switch {
			case !ok:
				reason += "; sweep REFUSED: residual " + ci.Leaves.FloatString(8) +
					" cannot be represented as a Decimal, and this platform does not send an order size it invented"
			default:
				qty := formatDec(scaled)
				if _, err := r.rest.sweepMarket(ctx, symbol, side, qty, "heal-"+ci.OrderID); err != nil {
					reason += "; sweep error: " + err.Error()
				} else {
					reason += "; swept residual " + ci.Leaves.FloatString(8) + " via market " + side
				}
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
// The error return was added for the Decimal conversion (#94): ExecutedQty is an
// exchange string, and a token filled quantity in the trillions wraps through
// dec.ToProto — healing the order to a filled size that never traded.
func binanceHealedFromQuery(ci CloseIntent, truth *orderResponse, venue string, now time.Time) (*orderpb.OrderState, error) {
	filled, _ := new(big.Rat).SetString(truth.ExecutedQty)
	if filled == nil {
		filled = new(big.Rat)
	}
	filledD, ok := dec.ToProtoScaled(filled)
	if !ok {
		return nil, fmt.Errorf("binance: order %s filled quantity %q is not representable as a Decimal",
			ci.OrderID, truth.ExecutedQty)
	}
	return &orderpb.OrderState{
		OrderId: ci.OrderID, InstrumentId: ci.InstrumentID,
		Status: binanceStatusToProto(truth.Status), FilledQuantity: filledD,
		Venue: venue, AsOf: timestamppb.New(now.UTC()),
	}, nil
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
		healed, drift, hErr := healedState(exp, truth, r.now())
		if hErr != nil {
			return hErr
		}
		if drift {
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
		// UNKNOWN SKIPS, IT DOES NOT COMPARE AGAINST ZERO (#418). Substituting
		// zero for a balance nobody has announced reports every asset the exchange
		// holds as a discrepancy — a break storm on the first run, which teaches an
		// operator to ignore this layer.
		expected, known := r.balances.Balance(b.Asset)
		if !known {
			r.unknownBalance(b.Asset)
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
	exFilled, _ := new(big.Rat).SetString(truth.ExecutedQty)
	if exFilled == nil {
		exFilled = new(big.Rat)
	}
	status := binanceStatusToProto(truth.Status)
	kanzFilled := dec.FromProto(exp.GetFilledQuantity())
	if exFilled.Cmp(kanzFilled) == 0 && status == exp.GetStatus() {
		return nil, false, nil // in parity
	}
	ordered := dec.FromProto(exp.GetOrderedQuantity())
	leaves := new(big.Rat).Sub(ordered, exFilled)
	// SCALED, NOT WRAPPING: these become the healed order's sizes in the FACT the
	// journal folds, so a wrapped value corrects the book to a size nothing traded.
	filledD, fok := dec.ToProtoScaled(exFilled)
	leavesD, lok := dec.ToProtoScaled(leaves)
	if !fok || !lok {
		return nil, false, fmt.Errorf("binance: order %s healed quantities are not representable as a Decimal "+
			"(filled=%s leaves=%s)", exp.GetOrderId(), exFilled.FloatString(8), leaves.FloatString(8))
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
	// SCALED, NOT WRAPPING (#94) — the same reasoning as the OKX reconciler. These
	// three numbers ARE the balance break; a wrapped Delta does not understate it,
	// it reports a different break entirely, and can make a real one look like a
	// match. Refusing to publish is the only safe failure.
	expectedD, eok := dec.ToProtoScaled(expected)
	actualD, aok := dec.ToProtoScaled(actual)
	deltaD, dok := dec.ToProtoScaled(delta)
	if !eok || !aok || !dok {
		return fmt.Errorf("binance: %s balance reconciliation is not representable as a Decimal "+
			"(expected=%s actual=%s) — refusing to publish a break with fabricated figures",
			asset, expected.FloatString(8), actual.FloatString(8))
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
// an incident.
func (r *Reconciler) unknownBalance(asset string) {
	if r.onUnknownBalance != nil {
		r.onUnknownBalance(asset)
	}
}
