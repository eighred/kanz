package main

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
)

func TestSyntheticGoRustGoRoundTrip(t *testing.T) {
	if os.Getenv("KANZ_SYNTHETIC_GO_ROUNDTRIP") != "1" {
		t.Skip("requires a disposable Go-to-Rust-to-Go fixture")
	}
	const tenant = "synthetic-go-roundtrip"
	exerciseSyntheticSession(t, tenant, func(ctx context.Context, s syntheticSession) {
		consumer, err := s.js.CreateConsumer(ctx, "SYNTHETIC_ORDERS", jetstream.ConsumerConfig{
			Durable: "roundtrip-origin", FilterSubject: bus.TenantRoutedSubject(tenant, "order.order.submit"),
			AckPolicy: jetstream.AckExplicitPolicy,
		})
		if err != nil {
			t.Fatal(err)
		}
		msg, err := consumer.Next(jetstream.FetchMaxWait(3 * time.Second))
		if err != nil {
			t.Fatal(err)
		}
		wire := msg.Data()
		source, request := admitSignedGoOrigin(t, ctx, s, msg)
		var cmd orderpb.SubmitOrder
		if err = proto.Unmarshal(source.Payload, &cmd); err != nil {
			t.Fatal(err)
		}
		reader := syntheticPool(t, ctx, "RUNTIME", tenant)
		var state, retained []byte
		var admitted, version int64
		err = reader.QueryRow(ctx, `SELECT o.state,o.version,c.writer_epoch,c.go_gateway_frame
			FROM orders o JOIN synthetic_oms_commands c USING(tenant_id,order_id) WHERE o.order_id=$1`,
			cmd.OrderId).Scan(&state, &version, &admitted, &retained)
		if err != nil || version != 0 || admitted != 1 || !bytes.Equal(retained, wire) {
			t.Fatalf("same-transaction Go admission: version %d, epoch %d: %v", version, admitted, err)
		}

		// Interrupt the command before ACK. The next owner must recover the
		// persisted result from the same broker delivery rather than re-admit.
		if err = msg.Nak(); err != nil {
			t.Fatal(err)
		}
		operator := syntheticPool(t, ctx, "SWITCH", tenant)
		var epoch int64
		if err = operator.QueryRow(ctx, "SELECT synthetic_test_handoff()").Scan(&epoch); err != nil || epoch != 2 {
			t.Fatalf("Go-to-Rust handoff: epoch %d: %v", epoch, err)
		}
		retry, err := consumer.Next(jetstream.FetchMaxWait(3 * time.Second))
		if err != nil {
			t.Fatal(err)
		}
		meta, err := retry.Metadata()
		if err != nil || meta.NumDelivered != 2 || !bytes.Equal(retry.Data(), wire) {
			t.Fatal("successor did not receive the original signed delivery")
		}
		goOriginResult(t, ctx, retry.Data(), "REFUSED")
		useWriterGeneration(t, "NEXT")
		reopened := requireNativeReceipt(t, ctx, request, "DUPLICATE")
		if reopened.OrderVersion != 0 || !bytes.Equal(reopened.State, state) {
			t.Fatal("Rust replay changed the accepted Go order")
		}
		if err = retry.DoubleAck(ctx); err != nil {
			t.Fatal(err)
		}

		exerciseSyntheticGoHandback(t, ctx, s, &cmd, request, 2)

		var observations []int64
		err = reader.QueryRow(ctx, `SELECT c.writer_epoch,c.go_gateway_frame,
			ARRAY(SELECT DISTINCT writer_epoch FROM synthetic_oms_observations WHERE order_id=c.order_id ORDER BY writer_epoch)
			FROM synthetic_oms_commands c WHERE c.order_id=$1`, cmd.OrderId).Scan(&admitted, &retained, &observations)
		if err != nil || admitted != 1 || !bytes.Equal(retained, wire) || len(observations) != 1 || observations[0] != 2 {
			t.Fatalf("retained first admission or venue lineage changed: epoch %d, observations %v: %v", admitted, observations, err)
		}
		t.Log("Go generation 1 admitted the original signed command, Rust generation 2 recovered its redelivery and venue history, Go generation 3 resumed the same acknowledged order")
	})
}
