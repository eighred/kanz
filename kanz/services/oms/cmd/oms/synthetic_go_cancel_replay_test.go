package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/oms/internal/order"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
)

// The isolated Go successor consumes the legacy native cancellation using
// its retained receipt. Go's ordinary cancel compensator expects a marker
// for both Go publications; the native FACT is already durably queued.
func replaySyntheticFundedCancel(t *testing.T, ctx context.Context, pool *pgxpool.Pool, broker, eventID string, st *orderpb.OrderState) {
	t.Helper()
	tenant := bus.TenantIDFromContext(ctx)
	nc, err := nats.Connect(broker)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	commands, err := js.Stream(ctx, "SYNTHETIC_ORDERS")
	if err != nil {
		t.Fatal(err)
	}
	facts, err := js.Stream(ctx, "SYNTHETIC_FACTS")
	if err != nil {
		t.Fatal(err)
	}
	before, err := facts.Info(ctx)
	if err != nil || before.State.Msgs != 4 {
		t.Fatalf("retained broker facts before Go replay: %v", err)
	}
	consumer, err := commands.CreateConsumer(ctx, jetstream.ConsumerConfig{
		Durable: "synthetic-go-retained-cancel", AckPolicy: jetstream.AckExplicitPolicy,
		FilterSubject: bus.TenantRoutedSubject(tenant, order.SubjectCancel),
	})
	if err != nil {
		t.Fatal(err)
	}
	for delivery := uint64(1); delivery <= 2; delivery++ {
		msg, err := consumer.Next(jetstream.FetchMaxWait(3 * time.Second))
		if err != nil {
			t.Fatal(err)
		}
		meta, err := msg.Metadata()
		if err != nil || meta.NumDelivered != delivery {
			t.Fatalf("unexpected cancel delivery %d: %v", delivery, err)
		}
		var frame envelopepb.EventFrame
		var cmd orderpb.CancelOrder
		if proto.Unmarshal(msg.Data(), &frame) != nil || bus.Validate(frame.Envelope) != nil ||
			proto.Unmarshal(frame.Payload, &cmd) != nil {
			t.Fatal("invalid retained cancellation delivery")
		}
		env := frame.Envelope
		if env.TenantId != tenant || env.Source != "api-gateway" || env.EventId != eventID ||
			env.EventClass != envelopepb.EventClass_EVENT_CLASS_COMMAND || env.EventType != order.SubjectCancel ||
			env.PartitionKey != st.OrderId || cmd.OrderId != st.OrderId || cmd.GetMetadata().GetTargetId() != st.OrderId {
			t.Fatal("cancellation scope differs from handback")
		}
		anchored := sha256.Sum256(frame.Payload)
		var completed bool
		err = pool.QueryRow(ctx, `SELECT public.synthetic_oms_assert_writer()=3 AND EXISTS (
			SELECT FROM synthetic_oms_cancellation_authorizations a
			JOIN synthetic_oms_observations claim USING(tenant_id,order_id,event_id)
			JOIN orders o USING(tenant_id,order_id)
			JOIN outbox f ON (f.tenant_id,f.partition_key,f.causation_id)=(a.tenant_id,a.order_id,a.event_id)
			WHERE (a.order_id,a.event_id,a.portfolio_id,a.principal_id,a.command_sha256,a.trace_context)=($1,$2,$3,$4,$5,$6)
			  AND (claim.effective_seconds,claim.effective_nanos)=($7,$8)
			  AND claim.operation='cancel_pending' AND o.status=5 AND f.subject='order.order.cancelled' AND f.published_at IS NOT NULL
		)`, cmd.OrderId, env.EventId, st.PortfolioId, cmd.GetMetadata().GetIssuer(), anchored[:], env.TraceContext,
			env.EventTime.Seconds, env.EventTime.Nanos).Scan(&completed)
		if err != nil || !completed {
			t.Fatalf("no matching published receipt for the original cancellation: %v", err)
		}
		if delivery == 1 {
			err = msg.Nak()
		} else {
			err = msg.DoubleAck(ctx)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	after, err := facts.Info(ctx)
	if err != nil || after.State.LastSeq != before.State.LastSeq || after.State.Msgs != before.State.Msgs {
		t.Fatalf("Go replay changed the broker fact sequence: %v", err)
	}
	if _, err := facts.GetLastMsgForSubject(ctx, order.EventTypeOutcome); !errors.Is(err, jetstream.ErrMsgNotFound) {
		t.Fatalf("Go replay emitted another command outcome: %v", err)
	}
	t.Log("GO_HANDBACK Go acknowledged redelivery of the original signed cancellation from its retained published receipt")
}
