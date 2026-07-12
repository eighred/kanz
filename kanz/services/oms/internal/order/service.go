package order

import (
	"context"
	"errors"
	"log/slog"
	"time"

	commandpb "github.com/kanz-eng/kanz-schemas-go/command/v1"
	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"

	"github.com/kanz-eng/kanz/internal/execution"
	"github.com/kanz-eng/kanz/services/oms/internal/compliance"
)

// Service is the OMS command handler (OMS-01b): the bus.EventHandler that
// drives the order aggregate. Per delivery it decodes the order command,
// validates it, runs the pre-trade compliance gate (OMS-01f), admits or refuses
// it, persists the resulting OrderState, and emits the lifecycle FACTs + the
// universal command outcome. Admission of a marketable order routes it to the
// EMS (OMS-01c) and folds the resulting fills.
//
// Error discipline mirrors the bus contract: a returned error nacks (retry/DLQ)
// and is reserved for TRANSIENT faults (store/publish failure). A bad command —
// validation or compliance rejection — is terminal: it is acked (nil) after a
// REJECTED outcome, so a poison command never blocks the partition.
type Service struct {
	store   Store
	gate    compliance.Gate
	emitter *Emitter
	router  *execution.Router
	closes  execution.CloseTracker
	now     func() time.Time
	logger  *slog.Logger
}

// NewService wires the handler. gate defaults to deny-nothing (compliance.AllowAll)
// when nil; router may be nil to admit orders without working them (they rest).
// closes is the in-flight-close registry the venue-close dispatch path writes to
// and the reconcilers' healing watchdogs drain; nil disables venue-side cancel
// dispatch (the cancel stays ledger-only).
func NewService(store Store, emitter *Emitter, gate compliance.Gate, router *execution.Router, closes execution.CloseTracker, logger *slog.Logger) (*Service, error) {
	if store == nil || emitter == nil {
		return nil, errors.New("oms: store and emitter required")
	}
	if gate == nil {
		gate = compliance.AllowAll{}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		store: store, gate: gate, emitter: emitter, router: router, closes: closes,
		now: time.Now, logger: logger,
	}, nil
}

// Handle is the bus.EventHandler. It dispatches by the command subject/type.
func (s *Service) Handle(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
	switch env.GetEventType() {
	case SubjectSubmit:
		return s.handleSubmit(ctx, payload)
	case SubjectCancel:
		return s.handleCancel(ctx, payload)
	case SubjectAmend:
		return s.handleAmend(ctx, payload)
	default:
		// Subscription scope matched but the type is unknown — surface so the
		// operator sees the misconfiguration (same stance as ingest).
		return errors.New("oms: unknown command type: " + env.GetEventType())
	}
}

func (s *Service) handleSubmit(ctx context.Context, payload []byte) error {
	var cmd orderpb.SubmitOrder
	if err := proto.Unmarshal(payload, &cmd); err != nil {
		// A malformed command body is a permanent defect; reject-and-ack rather
		// than redeliver forever. order_id is unknown, so nothing to correlate.
		s.logger.Error("oms: malformed SubmitOrder", "err", err)
		return nil
	}
	now := s.now().UTC()

	// Fast path for an obvious re-submit: skip the compliance gate and validation
	// for an order we already know. This is an OPTIMIZATION ONLY — it is a
	// check-then-act and cannot be the guard. Admission is enforced atomically by
	// store.Create below, which is what actually stands between a duplicated
	// SubmitOrder and a duplicated venue order.
	if _, err := s.store.Load(ctx, cmd.GetOrderId()); err == nil {
		return nil
	} else if !errors.Is(err, ErrNotFound) {
		return err // transient store failure
	}

	// Pre-trade compliance gate (OMS-01f).
	breach, err := s.gate.Check(ctx, &cmd)
	if err != nil {
		return err // transient gate failure ⇒ retry
	}
	if breach != nil {
		return s.refuse(ctx, cmd.GetOrderId(), "COMPLIANCE_"+breach.Code, breach.Reason, now)
	}

	// Validate + admit.
	st, err := Accept(&cmd, now)
	if err != nil {
		var re *RejectError
		if errors.As(err, &re) {
			return s.refuse(ctx, cmd.GetOrderId(), re.Code, re.Msg, now)
		}
		return err
	}
	// THE ADMISSION GATE. Create is atomic: exactly one concurrent delivery of this
	// order_id can insert it, and every other gets ErrExists. Losing the race means
	// another delivery already owns this order and is working it — so this one acks
	// and stops, HERE, before s.work() below routes it to a venue. The old
	// Load()-then-Save() let both deliveries through this point and both reached the
	// venue: the state converged (Save upserts) while the fund traded twice.
	if err := s.store.Create(ctx, st); err != nil {
		if errors.Is(err, ErrExists) {
			return nil // lost the admission race — the winner works the order
		}
		return err
	}
	if err := s.emitter.EmitAccepted(ctx, st); err != nil {
		return err
	}

	// Work the order if a router is wired; otherwise it rests (ACCEPTED).
	st, err = s.work(ctx, st)
	if err != nil {
		return err
	}

	status := commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_ACCEPTED
	reason := "order admitted"
	if st.GetStatus() == orderpb.OrderStatus_ORDER_STATUS_FILLED {
		status = commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_EXECUTED
		reason = "order filled"
	}
	return s.emitter.EmitOutcome(ctx, st.GetOrderId(), status, reason, "", "", now)
}

// work routes an admitted order to a venue and folds the resulting fills. A nil
// router leaves the order resting. Transient routing/publish errors are
// returned (retry); a venue that returns no fills leaves the order working.
func (s *Service) work(ctx context.Context, st *orderpb.OrderState) (*orderpb.OrderState, error) {
	if s.router == nil {
		return st, nil
	}
	venue, err := s.router.Route(st)
	if errors.Is(err, execution.ErrNoVenue) {
		return st, nil // no venue ⇒ rest
	}
	if err != nil {
		return st, err
	}
	routed := Route(st, s.now().UTC())
	if err := s.store.Save(ctx, routed); err != nil {
		return st, err
	}
	if err := s.emitter.EmitRouted(ctx, routed.GetOrderId(), venue.MIC(), "", routed.GetAsOf().AsTime()); err != nil {
		return st, err
	}
	st = routed

	fills, err := venue.Execute(ctx, st)
	if err != nil {
		return st, err
	}
	for _, fill := range fills {
		next, aerr := ApplyFill(st, fill, fill.GetExecutedAt().AsTime())
		if aerr != nil {
			// An over-fill from the venue is a bug, not a transient fault; log
			// and stop working this order rather than loop.
			s.logger.Error("oms: venue fill rejected by aggregate", "order_id", st.GetOrderId(), "err", aerr)
			break
		}
		if err := s.store.Save(ctx, next); err != nil {
			return st, err
		}
		if err := s.emitter.EmitFill(ctx, fill, next); err != nil {
			return st, err
		}
		st = next
		if IsTerminal(st) {
			break
		}
	}
	return st, nil
}

func (s *Service) handleCancel(ctx context.Context, payload []byte) error {
	var cmd orderpb.CancelOrder
	if err := proto.Unmarshal(payload, &cmd); err != nil {
		s.logger.Error("oms: malformed CancelOrder", "err", err)
		return nil
	}
	now := s.now().UTC()
	st, err := s.store.Load(ctx, cmd.GetOrderId())
	if errors.Is(err, ErrNotFound) {
		return s.outcomeReject(ctx, cmd.GetOrderId(), "UNKNOWN_ORDER", "no such order", now)
	}
	if err != nil {
		return err
	}
	// Validate the withdrawal against the aggregate first (the terminal guard), so
	// a cancel the ledger refuses never reaches the exchange.
	next, cancelledQty, cerr := Cancel(st, now)
	if cerr != nil {
		var re *RejectError
		if errors.As(cerr, &re) {
			return s.outcomeReject(ctx, cmd.GetOrderId(), re.Code, re.Msg, now)
		}
		return cerr
	}
	// Withdraw the order AT the venue before recording the cancellation. Without
	// this the ledger calls the order CANCELLED while it is still resting — and
	// still fillable — on the exchange. st (not next) carries the pre-cancel
	// quantities the close intent is built from.
	s.closeAtVenue(ctx, st, now)

	if err := s.store.Save(ctx, next); err != nil {
		return err
	}
	if err := s.emitter.EmitCancelled(ctx, next.GetOrderId(), cancelledQty, now); err != nil {
		return err
	}
	return s.emitter.EmitOutcome(ctx, next.GetOrderId(),
		commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_EXECUTED, "order cancelled", "", "", now)
}

// closeAtVenue withdraws a working order at the exchange holding it, recording
// the close in the in-flight registry BEFORE it races the venue so the
// reconciler's healing watchdog owns the acknowledgement window (the In-Flight
// Certainty seam). A venue with nothing resting externally (SimVenue) does not
// implement execution.Closer and is skipped — its cancel is ledger-only.
//
// It deliberately reports no error: the ledger must never freeze on a venue that
// will not answer. A failed or hung cancel stays TRACKED and the watchdog
// force-resolves it against venue truth. Returning it as a transient fault would
// instead nack the command and redeliver a cancel the venue may already have
// applied.
func (s *Service) closeAtVenue(ctx context.Context, st *orderpb.OrderState, now time.Time) {
	if s.router == nil || s.closes == nil {
		return
	}
	venue, err := s.router.Route(st)
	if err != nil {
		return // nothing is working this order at a venue
	}
	closer, ok := venue.(execution.Closer)
	if !ok {
		return // no order resting at an exchange to withdraw
	}

	// Track BEFORE dispatch: if the call hangs, times out ambiguously, or the
	// process dies mid-flight, the watchdog still sees the close. Tracking after
	// the ack would cover none of those windows.
	//
	// A cancelled resting order carries NO residual exposure to sweep — it has not
	// traded, so withdrawing it opens nothing (see execution.CloseIntent). Leaves
	// and SweepSide stay zero and the watchdog force-clears without sweeping. If
	// the cancel raced a fill, the watchdog's venue query returns the order
	// terminal and StateHealed carries that truth: we learn the fill, we never
	// invent an offsetting trade.
	// A SelfHealing venue (an out-of-process adapter) tracks and heals its own
	// closes. Tracking it here too would leave an entry nobody resolves in a
	// registry the in-process OKX reconciler drains indiscriminately — and it would
	// then try to heal another venue's order. Hands off.
	if _, selfHealing := closer.(execution.SelfHealing); !selfHealing {
		s.closes.Track(execution.CloseIntent{
			OrderID:      st.GetOrderId(),
			InstrumentID: st.GetInstrumentId(),
			RequestedAt:  now,
		})
	}
	if err := closer.CancelOrder(ctx, st); err != nil {
		s.logger.Error("oms: venue cancel unconfirmed — left to the healing watchdog",
			"order_id", st.GetOrderId(), "venue", venue.MIC(), "err", err)
		return
	}
	s.closes.Resolve(st.GetOrderId()) // venue confirmed the withdrawal — nothing to heal
}

func (s *Service) handleAmend(ctx context.Context, payload []byte) error {
	var cmd orderpb.AmendOrder
	if err := proto.Unmarshal(payload, &cmd); err != nil {
		s.logger.Error("oms: malformed AmendOrder", "err", err)
		return nil
	}
	now := s.now().UTC()
	st, err := s.store.Load(ctx, cmd.GetOrderId())
	if errors.Is(err, ErrNotFound) {
		return s.outcomeReject(ctx, cmd.GetOrderId(), "UNKNOWN_ORDER", "no such order", now)
	}
	if err != nil {
		return err
	}
	next, aerr := Amend(st, &cmd, now)
	if aerr != nil {
		var re *RejectError
		if errors.As(aerr, &re) {
			return s.outcomeReject(ctx, cmd.GetOrderId(), re.Code, re.Msg, now)
		}
		return aerr
	}
	if err := s.store.Save(ctx, next); err != nil {
		return err
	}
	return s.emitter.EmitOutcome(ctx, next.GetOrderId(),
		commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_EXECUTED, "order amended", "", "", now)
}

// refuse emits an ORDER_REJECTED FACT and a REJECTED command outcome, then acks
// (returns nil) — a refused command is terminal.
func (s *Service) refuse(ctx context.Context, orderID, code, reason string, t time.Time) error {
	if err := s.emitter.EmitRejected(ctx, orderID, code, reason, t); err != nil {
		return err
	}
	return s.outcomeReject(ctx, orderID, code, reason, t)
}

func (s *Service) outcomeReject(ctx context.Context, orderID, code, reason string, t time.Time) error {
	return s.emitter.EmitOutcome(ctx, orderID,
		commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_REJECTED, reason, code, "", t)
}
