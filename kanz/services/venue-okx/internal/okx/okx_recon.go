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
	"github.com/eighred/kanz/pkg/bus"
)

// OKXReconciler is the OKX secondary audit layer — the analog of the Binance
// Reconciler. It polls OKX for the venue truth and, where Kanz's state has
// drifted, emits a correcting FACT (StateHealed / BalanceReconciled). It never
// edits state; the journal folds the FACTs to restore parity bitemporally.
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
	// (#418). Nil ⇒ silent, which is only right in a test: in production an
	// operator must be able to tell "reconciliation found nothing wrong" from
	// "reconciliation could not check", and those look identical otherwise.
	onUnknownBalance func(asset string)
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
	// loop force-clears it. <=0 ⇒ 1500ms (the mandate's trigger).
	CloseTimeout     time.Duration
	Pub              Publisher
	Venue            string
	Tenant           string
	Now              func() time.Time
	OnUnknownBalance func(asset string)
}

func newOKXReconciler(cfg OKXReconcilerConfig) *OKXReconciler {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Venue == "" {
		cfg.Venue = "OKX"
	}
	if cfg.CloseTimeout <= 0 {
		cfg.CloseTimeout = 1500 * time.Millisecond
	}
	return &OKXReconciler{
		rest: cfg.REST, symbols: cfg.Symbols, expected: cfg.Expected, balances: cfg.Balances,
		closes: cfg.Closes, closeTimeout: cfg.CloseTimeout,
		pub: cfg.Pub, venue: cfg.Venue, tenant: cfg.Tenant, now: cfg.Now,
		onUnknownBalance: cfg.OnUnknownBalance,
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

// RunHealing drives the In-Flight Certainty watchdog on a fast tick (independent
// of the slower order/balance cron): every tick it force-resolves any close that
// has stayed unconfirmed past CloseTimeout so the ledger never freezes. No-op
// when no close registry is configured.
func (r *OKXReconciler) RunHealing(ctx context.Context, tick time.Duration) {
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
// by clOrdId; if the venue confirms it terminal, emit a StateHealed carrying the
// truth. If the order is still working or the venue is unresponsive, force-clear
// it (StateHealed → CANCELLED) and sweep the residual exposure with an aggressive
// market order, then reconcile balances so a BalanceReconciled FACT re-anchors
// the ledger from real venue truth — never a fabricated balance.
func (r *OKXReconciler) HealClosures(ctx context.Context) error {
	if r.closes == nil {
		return nil
	}
	swept := false
	for _, ci := range r.closes.DueCloses(r.now(), r.closeTimeout) {
		instID, ok := r.symbols.Symbol(ci.InstrumentID)
		if !ok {
			r.closes.Resolve(ci.OrderID) // untradeable here — stop watching it
			continue
		}
		o, qErr := r.rest.queryOrder(ctx, instID, ci.OrderID)
		if qErr == nil && okxTerminal(o.State) {
			// Venue confirms the close landed — adopt its truth and stop.
			healed, drift, hErr := okxHealedFromQuery(ci, o, r.now())
			if hErr != nil {
				return hErr
			}
			if drift {
				if err := r.emitStateHealed(ctx, healed, "in-flight close confirmed terminal on venue query"); err != nil {
					return err
				}
			}
			r.closes.Resolve(ci.OrderID)
			continue
		}
		// Stuck or unresponsive: force-clear and sweep so nothing freezes.
		if err := r.forceSweep(ctx, ci, instID); err != nil {
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

func (r *OKXReconciler) forceSweep(ctx context.Context, ci CloseIntent, instID string) error {
	reason := "in-flight close timeout (>" + r.closeTimeout.String() + "); force-cleared"
	// Sweep the residual exposure with an aggressive market order when there is
	// something to flatten and a direction to flatten it in. Best-effort: a sweep
	// failure raises a structural alert but must not block the force-clear — the
	// ledger must progress. The sweep's real fills arrive via the user-data
	// stream / next balance pass; we never fabricate them.
	if ci.Leaves != nil && ci.Leaves.Sign() > 0 && ci.SweepSide != orderpb.Side_SIDE_UNSPECIFIED {
		side, sErr := okxSide(ci.SweepSide)
		if sErr == nil {
			// SCALED, NOT WRAPPING (#94). This size becomes a live MARKET order at OKX.
			// dec.ToProto wraps once the scaled coefficient exceeds an int64 — about
			// 92.2 billion units at scale 8 — and a residual past that is not
			// hypothetical here: a meme-coin position trades in the trillions on this
			// venue. A residual of 1e12 renders through ToProto as 77662796314.5224192,
			// so the sweep meant to flatten the book would instead market-buy a
			// fabricated size, immediately and irreversibly.
			sz, ok := dec.ToProtoScaled(ci.Leaves)
			switch {
			case !ok:
				// Refusing leaves a residual open, which is bad. Sending a number the
				// platform made up is worse, and unrecoverable. The force-clear below
				// still runs and the break is still raised, so this surfaces.
				reason += "; sweep REFUSED: residual " + ci.Leaves.FloatString(8) +
					" cannot be represented as a Decimal, and this platform does not send an order size it invented"
			default:
				if _, err := r.rest.sweepMarket(ctx, instID, side, FormatDec(sz), "heal-"+ci.OrderID); err != nil {
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

// okxTerminal reports whether an OKX order state is terminal.
func okxTerminal(state string) bool {
	return state == "filled" || state == "canceled"
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
	filled, _ := new(big.Rat).SetString(o.AccFillSz)
	if filled == nil {
		filled = new(big.Rat)
	}
	// SCALED, NOT WRAPPING. AccFillSz is an exchange string; a token filled
	// quantity in the trillions wraps through dec.ToProto and this FACT would
	// then heal the order to a filled size that never traded.
	filledD, ok := dec.ToProtoScaled(filled)
	if !ok {
		return nil, false, fmt.Errorf("okx: order %s filled quantity %q is not representable as a Decimal",
			ci.OrderID, o.AccFillSz)
	}
	return &orderpb.OrderState{
		OrderId: ci.OrderID, InstrumentId: ci.InstrumentID,
		Status: okxStateToProto(o.State), FilledQuantity: filledD,
		Venue: "OKX", AsOf: timestamppb.New(now.UTC()),
	}, true, nil
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
		healed, drift, hErr := okxHealedState(exp, o, r.now())
		if hErr != nil {
			return hErr
		}
		if drift {
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
		// UNKNOWN SKIPS, IT DOES NOT COMPARE AGAINST ZERO (#418). Substituting
		// zero for a balance nobody has announced reports every asset the exchange
		// holds as a discrepancy — a break storm on the first run, which teaches an
		// operator to ignore this layer.
		expected, known := r.balances.Balance(ccy)
		if !known {
			r.unknownBalance(ccy)
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
	exFilled, _ := new(big.Rat).SetString(o.AccFillSz)
	if exFilled == nil {
		exFilled = new(big.Rat)
	}
	status := okxStateToProto(o.State)
	if exFilled.Cmp(dec.FromProto(exp.GetFilledQuantity())) == 0 && status == exp.GetStatus() {
		return nil, false, nil
	}
	ordered := dec.FromProto(exp.GetOrderedQuantity())
	leaves := new(big.Rat).Sub(ordered, exFilled)
	// SCALED, NOT WRAPPING. These two become the order's filled and leaves sizes
	// in the healed FACT the journal folds; a wrapped value corrects the book to a
	// size that never traded, which is worse than the drift being healed.
	filledD, fok := dec.ToProtoScaled(exFilled)
	leavesD, lok := dec.ToProtoScaled(leaves)
	if !fok || !lok {
		return nil, false, fmt.Errorf("okx: order %s healed quantities are not representable as a Decimal "+
			"(filled=%s leaves=%s)", exp.GetOrderId(), exFilled.FloatString(8), leaves.FloatString(8))
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
	// SCALED, NOT WRAPPING (#94). These three numbers ARE the balance break — the
	// figures an operator reads to decide whether the book or the exchange is
	// wrong. dec.ToProto wraps above ~92.2 billion units at scale 8, which a
	// token balance reaches, and a wrapped Delta does not report a smaller break:
	// it reports a DIFFERENT one, and can turn a real break into an apparent
	// match. Refusing to publish is the only safe failure here.
	expectedD, eok := dec.ToProtoScaled(expected)
	actualD, aok := dec.ToProtoScaled(actual)
	deltaD, dok := dec.ToProtoScaled(delta)
	if !eok || !aok || !dok {
		return fmt.Errorf("okx: %s balance reconciliation is not representable as a Decimal "+
			"(expected=%s actual=%s) — refusing to publish a break with fabricated figures",
			asset, expected.FloatString(8), actual.FloatString(8))
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
// an incident.
func (r *OKXReconciler) unknownBalance(asset string) {
	if r.onUnknownBalance != nil {
		r.onUnknownBalance(asset)
	}
}
