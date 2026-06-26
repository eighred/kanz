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

	"github.com/kanz-eng/kanz/services/oms/internal/compliance"
	"github.com/kanz-eng/kanz/services/oms/internal/execution"
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
	now     func() time.Time
	logger  *slog.Logger
}

// NewService wires the handler. gate defaults to deny-nothing (compliance.AllowAll)
// when nil; router may be nil to admit orders without working them (they rest).
func NewService(store Store, emitter *Emitter, gate compliance.Gate, router *execution.Router, logger *slog.Logger) (*Service, error) {
	if store == nil || emitter == nil {
		return nil, errors.New("oms: store and emitter required")
	}
	if gate == nil {
		gate = compliance.AllowAll{}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{store: store, gate: gate, emitter: emitter, router: router, now: time.Now, logger: logger}, nil
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

	// Idempotent re-submit: the order already exists ⇒ ack without re-processing
	// (the envelope idempotency-key dedup is the first guard; this covers a
	// re-submit under a fresh key). One order per order_id.
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
	if err := s.store.Save(ctx, st); err != nil {
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
	next, cancelledQty, cerr := Cancel(st, now)
	if cerr != nil {
		var re *RejectError
		if errors.As(cerr, &re) {
			return s.outcomeReject(ctx, cmd.GetOrderId(), re.Code, re.Msg, now)
		}
		return cerr
	}
	if err := s.store.Save(ctx, next); err != nil {
		return err
	}
	if err := s.emitter.EmitCancelled(ctx, next.GetOrderId(), cancelledQty, now); err != nil {
		return err
	}
	return s.emitter.EmitOutcome(ctx, next.GetOrderId(),
		commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_EXECUTED, "order cancelled", "", "", now)
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
