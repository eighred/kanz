//go:build synthetic_handoff

package order

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/outbox"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/oms/internal/capital"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/protobuf/proto"
)

// The successor accepts only bounded ASCII graphic identifiers at ingress.
func syntheticWriterID(s string) bool {
	return len(s) > 0 && len(s) <= 256 && strings.IndexFunc(s, func(r rune) bool {
		return r < '!' || r > '~'
	}) == -1
}

// WithSyntheticHandoffCancellationRecovery recognizes terminal Rust receipts
// in the isolated XSIM successor. The ordinary Go compensator would otherwise
// republish the terminal FACT because Rust does not set Go's announcement marker.
func WithSyntheticHandoffCancellationRecovery(store *Postgres) ServiceOption {
	return func(s *Service) {
		s.retainedCancelReplay = func(ctx context.Context, env *envelopepb.Envelope, payload []byte, cmd *orderpb.CancelOrder, st *orderpb.OrderState, version int64) (bool, error) {
			return s.recoverSyntheticCancellation(ctx, store, env, payload, cmd, st, version)
		}
	}
}

func (s *Service) recoverSyntheticCancellation(ctx context.Context, store *Postgres, env *envelopepb.Envelope, payload []byte, cmd *orderpb.CancelOrder, st *orderpb.OrderState, version int64) (bool, error) {
	if st.GetVenue() != "XSIM" || st.GetVenueAccountId() != "synthetic-spot" {
		return false, nil
	}
	missing := bus.Terminal(errors.New("oms: synthetic cancellation has no matching published receipt"))
	meta := cmd.GetMetadata()
	at := env.GetEventTime()
	if env.GetSource() != "api-gateway" || env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_COMMAND ||
		env.GetEventType() != SubjectCancel || env.GetTenantId() != bus.TenantIDFromContext(ctx) ||
		env.GetPartitionKey() != st.GetOrderId() || meta.GetTargetId() != st.GetOrderId() ||
		at.CheckValid() != nil {
		return false, missing
	}
	digest := sha256.Sum256(payload)
	var active, matched, published bool
	err := store.pool.QueryRow(ctx, `SELECT
		current_database()='kanz_synthetic_oms'
		AND pg_has_role(session_user,'kanz_synthetic_oms_go_funded_runtime','MEMBER')
		AND public.synthetic_oms_assert_writer()>1,
		count(*)=1, coalesce(bool_and(receipt.published_at IS NOT NULL),false)
		FROM (
			SELECT f.published_at FROM public.synthetic_oms_cancellation_authorizations a
			JOIN public.synthetic_oms_observations claim USING(tenant_id,order_id,event_id)
			JOIN public.orders o USING(tenant_id,order_id)
			JOIN public.outbox f ON (f.tenant_id,f.partition_key,f.causation_id)=(a.tenant_id,a.order_id,a.event_id)
			WHERE (a.tenant_id,a.order_id,a.event_id,a.portfolio_id,a.principal_id,a.command_sha256,a.trace_context)
				=($1,$2,$3,$4,$5,$6,$7)
			  AND (claim.effective_seconds,claim.effective_nanos)=($8,$9)
			  AND claim.operation='cancel_pending' AND o.status=$10 AND o.version=$11
			  AND f.subject=$12 AND f.trace_context=a.trace_context
		) receipt`, env.GetTenantId(), st.GetOrderId(), env.GetEventId(), st.GetPortfolioId(), meta.GetIssuer(), digest[:], env.GetTraceContext(),
		at.Seconds, at.Nanos, st.GetStatus(), version, EventTypeCancelled).Scan(&active, &matched, &published)
	if err != nil {
		return false, err
	}
	if !active || !matched {
		return false, missing
	}
	if !published {
		// Another replica can commit the terminal row between this handler's
		// initial drain and its load. Publish the matched record before ACK.
		_, err := s.relay.Flush(ctx, st.GetOrderId())
		return true, err
	}
	return true, nil
}

// CreateSyntheticHandoff admits one direct unfunded XSIM buy into the Go store.
// The independent control role must already have authorized this exact broker
// frame. Record its original bytes in the same transaction as the acceptance.
func (p *Postgres) CreateSyntheticHandoff(ctx context.Context, wire []byte) error {
	return p.createSyntheticHandoff(ctx, wire, false)
}

// CreateFundedSyntheticHandoff requests the isolated XSIM spot fee schedule.
// A separate funding principal must authorize the exact resulting debits.
func (p *Postgres) CreateFundedSyntheticHandoff(ctx context.Context, wire []byte) error {
	return p.createSyntheticHandoff(ctx, wire, true)
}

func (p *Postgres) createSyntheticHandoff(ctx context.Context, wire []byte, funded bool) error {
	invalid := errors.New("unsupported synthetic admission")
	var frame envelopepb.EventFrame
	if len(wire) == 0 || len(wire) > 131072 || proto.Unmarshal(wire, &frame) != nil {
		return invalid
	}
	env := frame.GetEnvelope()
	if bus.Validate(env) != nil || env.GetTenantId() != bus.TenantIDFromContext(ctx) || env.GetSource() != "api-gateway" ||
		env.GetEventType() != "order.order.submit" || env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_COMMAND ||
		!syntheticWriterID(env.GetEventId()) || !syntheticWriterID(env.GetTenantId()) || !syntheticWriterID(env.GetIdempotencyKey()) {
		return invalid
	}
	var cmd orderpb.SubmitOrder
	if len(frame.Payload) > 65536 || (proto.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(frame.Payload, &cmd) != nil {
		return invalid
	}
	meta := cmd.GetMetadata()
	// Keep optional order semantics unknown until the Go and Rust writers agree
	// on them. Byte equality also rejects duplicate or noncanonical encodings.
	direct := &orderpb.SubmitOrder{Metadata: meta, OrderId: cmd.OrderId, PortfolioId: cmd.PortfolioId, InstrumentId: cmd.InstrumentId,
		Venue: cmd.Venue, Side: cmd.Side, OrderType: cmd.OrderType, TimeInForce: cmd.TimeInForce, Quantity: cmd.Quantity, LimitPrice: cmd.LimitPrice}
	canonical, err := proto.MarshalOptions{Deterministic: true}.Marshal(direct)
	if err != nil || !bytes.Equal(canonical, frame.Payload) || cmd.OrderId != env.PartitionKey ||
		meta.GetTargetId() != cmd.OrderId || !syntheticWriterID(meta.GetIssuer()) || !syntheticWriterID(cmd.PortfolioId) || !slices.Contains(meta.GetPrincipalPortfolios(), cmd.PortfolioId) ||
		!strings.HasPrefix(meta.GetIssuer(), "user:") || len(meta.GetIssuer()) <= len("user:") ||
		!dec.InDomain(cmd.Quantity) || !dec.InDomain(cmd.LimitPrice) || cmd.Venue != "XSIM" || cmd.InstrumentId != "SYNTH-USD" || cmd.Side != orderpb.Side_SIDE_BUY ||
		cmd.OrderType != orderpb.OrderType_ORDER_TYPE_LIMIT || cmd.TimeInForce != orderpb.TimeInForce_TIME_IN_FORCE_GTC {
		return invalid
	}
	if len(cmd.OrderId) != 32 || strings.Trim(cmd.OrderId, "0123456789abcdef") != "" {
		return invalid
	}
	now := time.Now().UTC()
	if until := meta.GetValidUntil(); until != nil && (until.CheckValid() != nil || !now.Before(until.AsTime())) {
		return invalid
	}
	st, err := Accept(&cmd, now)
	if err != nil {
		return err
	}
	st.VenueAccountId = "synthetic-spot"
	st.AcceptedAnnouncedAt = st.AsOf
	var amounts []string
	if funded {
		debits, err := capital.PhysicalDebits(st, "SYN", "USD", []*commonpb.Money{
			{CurrencyCode: "USD", Amount: &commonpb.Decimal{Coefficient: 2}},
			{CurrencyCode: "BNB", Amount: &commonpb.Decimal{Coefficient: 1}},
		})
		if err != nil || len(debits) != 2 || debits[0].CurrencyCode != "BNB" || debits[1].CurrencyCode != "USD" {
			return invalid
		}
		amounts = []string{dec.FromProto(debits[0].Amount).RatString(), dec.FromProto(debits[1].Amount).RatString()}
	}
	ctx = bus.WithTraceContext(bus.WithCausationID(bus.WithCorrelationID(ctx, env.EventId), env.EventId), env.TraceContext)
	fact, err := NewEmitter(nil).AcceptedFact(ctx, st)
	if err != nil {
		return err
	}
	err = p.create(ctx, st, []outbox.Record{fact}, nil, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO public.synthetic_oms_commands
			(order_id,principal_id,portfolio_id,idempotency_key,command_sha256,command,evidence_ids,go_gateway_frame,go_funding_amounts)
			SELECT $1,$2,$3,$4,sha256($5),$5,ARRAY(SELECT e.source_id FROM public.synthetic_oms_evidence e WHERE
				(e.tenant_id,e.order_id,e.principal_id,e.portfolio_id,e.command_sha256,e.instrument_id)=
				($7,$1,$2,$3,sha256($5),'SYNTH-USD') ORDER BY e.kind),$6,$8::text[]
			WHERE current_database()='kanz_synthetic_oms' AND public.app_current_tenant()=$7`,
			cmd.OrderId, meta.Issuer, cmd.PortfolioId, env.IdempotencyKey, frame.Payload, wire, env.TenantId, amounts)
		if err == nil && tag.RowsAffected() != 1 {
			return invalid
		}
		return err
	})
	var constraint *pgconn.PgError
	if !errors.Is(err, ErrExists) && (!errors.As(err, &constraint) || constraint.Code != "23505" || constraint.TableName != "synthetic_oms_commands") {
		return err
	}
	// A concurrent retry may have won the canonical uniqueness race. Only
	// acknowledge its exact retained delivery under an active writer fence.
	var epoch int64
	var same bool
	err = p.pool.QueryRow(ctx, `SELECT public.synthetic_oms_assert_writer(), EXISTS(SELECT FROM public.synthetic_oms_commands
		WHERE tenant_id=$1 AND go_gateway_frame=$2 AND command=$3 AND order_id=$4 AND principal_id=$5 AND idempotency_key=$6 AND go_funding_amounts IS NOT DISTINCT FROM $7::text[])`,
		env.TenantId, wire, frame.Payload, cmd.OrderId, meta.Issuer, env.IdempotencyKey, amounts).Scan(&epoch, &same)
	if err != nil || epoch < 1 || !same {
		return invalid
	}
	return ErrExists
}
