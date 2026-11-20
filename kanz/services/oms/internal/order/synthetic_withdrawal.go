//go:build synthetic_handoff

package order

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/execution"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"google.golang.org/protobuf/proto"
)

// WithSyntheticHandoffWithdrawal enables the restricted Go successor's XSIM
// withdrawal path. Its SQL role needs the two settlement bridge functions.
func WithSyntheticHandoffWithdrawal(store *Postgres) ServiceOption {
	return func(s *Service) {
		WithSyntheticHandoffCancellationRecovery(store)(s)
		s.fundedWithdrawal = func(ctx context.Context, env *envelopepb.Envelope, payload []byte, cmd *orderpb.CancelOrder, st *orderpb.OrderState) error {
			return s.withdrawSyntheticHandoff(ctx, store, env, payload, cmd, st)
		}
		s.withdrawalPending = store.syntheticWithdrawalPending
	}
}

func (p *Postgres) syntheticWithdrawalPending(ctx context.Context, st *orderpb.OrderState, version int64) (bool, error) {
	if st.GetVenue() != "XSIM" || st.GetVenueAccountId() != "synthetic-spot" ||
		st.GetInstrumentId() != "SYNTH-USD" || st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED {
		return false, nil
	}
	var phase string
	var active bool
	var claims int
	err := p.pool.QueryRow(ctx, `SELECT c.phase,
		pg_has_role(session_user,to_regrole('kanz_synthetic_oms_go_working_runtime'),'USAGE')
			AND public.synthetic_oms_assert_writer()>1,
		(SELECT count(*) FROM public.synthetic_oms_observations x
		 JOIN public.synthetic_oms_cancellation_authorizations a USING(tenant_id,order_id,event_id)
		 WHERE (x.tenant_id,x.order_id,a.portfolio_id,a.principal_id)=(c.tenant_id,c.order_id,c.portfolio_id,c.principal_id)
		   AND x.operation='cancel_pending' AND x.writer_epoch=public.synthetic_oms_assert_writer()
		   AND x.observation=''::bytea
		   AND a.effective_at=to_timestamp(x.effective_seconds)+(x.effective_nanos/1000)*interval '1 microsecond')
		FROM public.synthetic_oms_commands c
		JOIN public.orders o USING(tenant_id,order_id,portfolio_id)
		WHERE current_database()='kanz_synthetic_oms' AND o.tenant_id=$1 AND o.order_id=$2 AND o.portfolio_id=$3
		  AND o.status=$4 AND o.version=$5 AND c.go_gateway_frame IS NOT NULL AND c.go_funding_amounts IS NOT NULL`,
		bus.TenantIDFromContext(ctx), st.GetOrderId(), st.GetPortfolioId(), st.GetStatus(), version).Scan(&phase, &active, &claims)
	if err != nil {
		return false, err
	}
	if active {
		switch {
		case phase == "working" && claims == 0:
			return false, nil
		case (phase == "cancel_pending" || phase == "unknown_cancel") && claims == 1:
			return true, nil
		}
	}
	return false, errors.New("oms: inconsistent retained withdrawal at startup")
}

func (s *Service) withdrawSyntheticHandoff(ctx context.Context, store *Postgres, env *envelopepb.Envelope,
	payload []byte, cmd *orderpb.CancelOrder, st *orderpb.OrderState,
) error {
	refused := bus.Terminal(errors.New("oms: synthetic working withdrawal not authorized"))
	at := env.GetEventTime()
	meta := cmd.GetMetadata()
	if bus.Validate(env) != nil || env.GetTenantId() != bus.TenantIDFromContext(ctx) || env.GetSource() != "api-gateway" ||
		env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_COMMAND || env.GetEventType() != SubjectCancel ||
		env.GetPartitionKey() != st.GetOrderId() || meta.GetTargetId() != st.GetOrderId() ||
		!syntheticWriterID(meta.GetIssuer()) || !syntheticWriterID(env.GetEventId()) ||
		len(payload) == 0 || len(payload) > 65536 || at.CheckValid() != nil ||
		st.GetVenue() != "XSIM" || st.GetVenueAccountId() != "synthetic-spot" || st.GetInstrumentId() != "SYNTH-USD" ||
		st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED || st.GetVenueAckAt() == nil {
		return refused
	}
	if s.router == nil {
		return errors.New("oms: synthetic withdrawal requires a venue router")
	}
	venue, err := s.router.Route(st)
	if err != nil {
		return err
	}
	closer, canClose := venue.(execution.Closer)
	query, canQuery := venue.(execution.Querier)
	if !canClose || !canQuery {
		return errors.New("oms: synthetic withdrawal requires both typed cancel and order query")
	}
	var rawFill []byte
	if err = store.pool.QueryRow(ctx, `SELECT fill FROM public.order_executions WHERE order_id=$1`, st.GetOrderId()).Scan(&rawFill); err != nil {
		return err
	}
	var retained orderpb.Fill
	if proto.Unmarshal(rawFill, &retained) != nil || retained.GetFillId() == "" || retained.GetExecutedAt().CheckValid() != nil {
		return errors.New("oms: unreadable retained execution for synthetic withdrawal")
	}
	var decision string
	args := []any{st.GetPortfolioId(), st.GetOrderId(), meta.GetIssuer(), env.GetEventId(), env.GetTraceContext(), at.Seconds, at.Nanos, payload}
	// QueryRow finishes the autocommit statement before returning SEND. A
	// retry after any ambiguous result can only query that same order.
	if err = store.pool.QueryRow(ctx, `SELECT public.synthetic_oms_go_cancel_claim($1,$2,$3,$4,$5,$6,$7,$8)`, args...).Scan(&decision); err != nil {
		return err
	}
	switch decision {
	case "SEND":
		if err = closer.CancelOrder(ctx, st); err != nil {
			return fmt.Errorf("oms: claimed synthetic withdrawal needs reconciliation: %w", err)
		}
	case "QUERY":
	default:
		return refused
	}

	view, err := query.QueryOrder(ctx, st)
	if err != nil {
		return err
	}
	if view.State != execution.OrderViewCancelled || !MatchesRetainedSyntheticExecution(view, st, &retained) {
		return errors.New("oms: synthetic withdrawal has no matching terminal execution history")
	}
	effective := at.AsTime()
	if executed := retained.GetExecutedAt().AsTime(); executed.After(effective) {
		effective = executed
	}
	ctx, done := commitContext(ctx)
	defer done()
	if err := s.finishSyntheticWithdrawal(ctx, store, st, view, &retained, effective, args); err != nil {
		return err
	}
	_, err = s.relay.Flush(ctx, st.GetOrderId())
	return err
}

// MatchesRetainedSyntheticExecution verifies the complete single-fill history
// required by the funded synthetic handback before adoption or settlement.
func MatchesRetainedSyntheticExecution(view execution.OrderView, st *orderpb.OrderState, retained *orderpb.Fill) bool {
	total, filled, quantity := view.ExecutedQuantity, st.GetFilledQuantity(), retained.GetQuantity()
	return retained != nil && len(view.Fills) == 1 && proto.Equal(view.Fills[0], retained) &&
		dec.InDomain(total) && dec.InDomain(filled) && dec.InDomain(quantity) && dec.IsPositive(quantity) &&
		dec.Cmp(total, filled) == 0 && dec.Cmp(total, quantity) == 0
}

func (s *Service) finishSyntheticWithdrawal(ctx context.Context, store *Postgres, st *orderpb.OrderState, view execution.OrderView,
	retained *orderpb.Fill, effective time.Time, args []any,
) error {
	next, leaves, err := Cancel(st, effective)
	if err != nil {
		return err
	}
	fact, err := s.emitter.CancelledFact(ctx, st.GetOrderId(), leaves, effective)
	if err != nil {
		return err
	}
	marshal := proto.MarshalOptions{Deterministic: true}.Marshal
	body, err := marshal(view.ExecutedQuantity)
	if err != nil {
		return err
	}
	state, err := marshal(next)
	if err != nil {
		return err
	}
	var decision string
	args = append(args, retained.GetFillId()+"/cancelled", effective.Unix(), int32(effective.Nanosecond()), body, state, fact.Payload)
	if err = store.pool.QueryRow(ctx, `SELECT public.synthetic_oms_go_cancel_finish($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`, args...).Scan(&decision); err != nil {
		return err
	}
	if decision != "APPLIED" && decision != "DUPLICATE" {
		return errors.New("oms: synthetic withdrawal completion lacks settlement authority")
	}
	s.logger.Info("oms synthetic withdrawal settlement", "order_id", st.GetOrderId(), "result", decision)
	return nil
}
