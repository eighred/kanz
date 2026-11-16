package storeconformance

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/bustest"
	"github.com/eighred/kanz/internal/outbox"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	_ "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const relaySubject = "order.order.cancelled"

// This records bytes emitted by the actual Go Producer, not a duplicate of its
// stamping/normalization implementation. Real broker acceptance is tested below.
type relayCapture struct{ message bus.Message }

func (c *relayCapture) Publish(_ context.Context, m bus.Message) error { c.message = m; return nil }
func (*relayCapture) Subscribe(context.Context, string, string, bus.Handler) error {
	return errors.New("unused subscription")
}
func (*relayCapture) Close() error { return nil }

func relayRecord(t *testing.T, key string, quantity int64) outbox.Record {
	t.Helper()
	r, err := outbox.From(bus.WithTenantID(context.Background(), "relay-envelope-tenant"), bus.Event{
		Subject: relaySubject, EventType: relaySubject, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion: 1, Domain: "order", EventTime: at, PartitionKey: key,
		CorrelationID: "relay-correlation", CausationID: "relay-cause", TraceContext: "relay-trace",
		Payload: &orderpb.OrderCancelled{OrderId: key, CancelledQuantity: &commonpb.Decimal{Coefficient: quantity, Exponent: -10}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func verifyRelayFrame(t *testing.T, record outbox.Record, raw []byte, sequence uint64, since time.Time) *envelopepb.EventFrame {
	t.Helper()
	var actual envelopepb.EventFrame
	if proto.Unmarshal(raw, &actual) != nil || actual.Envelope == nil {
		t.Fatal("invalid actual frame")
	}
	e := actual.Envelope
	if bus.Validate(e) != nil {
		t.Fatal("actual envelope refused by canonical validation")
	}
	id, err := uuid.Parse(e.EventId)
	if err != nil || id.Version() != 7 || id.Variant() != uuid.RFC4122 {
		t.Fatal("per-attempt identity is not UUIDv7")
	}
	for _, stamp := range []time.Time{e.GetIngestionTime().AsTime(), e.GetPublishTime().AsTime()} {
		if stamp.Before(since) || stamp.After(time.Now().Add(time.Second)) {
			t.Fatal("publication timestamp outside actual invocation")
		}
	}
	// IDs and publication clocks are nondeterministic. Bind the Go oracle to the
	// observed valid identity/ingestion time; normalize only its wall-clock publish
	// time and sequence after independently asserting the expected sequence.
	if e.ProducerSequence != sequence {
		t.Fatalf("sequence %d, want %d", e.ProducerSequence, sequence)
	}
	event, err := record.Event()
	if err != nil {
		t.Fatal("Go could not reconstruct expected record")
	}
	event.EventID, event.IngestionTime = e.EventId, e.IngestionTime.AsTime()
	capture := &relayCapture{}
	producer, err := bus.NewProducer(capture, bus.ProducerConfig{Source: "oms/relay-fixture", ProducerVersion: os.Getenv("KANZ_OMS_STORE_VERSION")})
	if err != nil || producer.Publish(context.Background(), event) != nil {
		t.Fatal("Go oracle publication refused")
	}
	var expected envelopepb.EventFrame
	if proto.Unmarshal(capture.message.Body, &expected) != nil {
		t.Fatal("Go oracle frame invalid")
	}
	expected.Envelope.PublishTime = proto.Clone(e.PublishTime).(*timestamppb.Timestamp)
	expected.Envelope.ProducerSequence = sequence
	if !bytes.Equal(expected.Payload, actual.Payload) || !proto.Equal(expected.Envelope, e) {
		t.Fatal("Go/Rust payload bytes or complete envelope differ")
	}
	return &actual
}

func TestRustRelayFrameCompatibility(t *testing.T) {
	base := relayRecord(t, "frame-order", 7)
	cases := map[string]outbox.Record{"canonical-decimal": base}
	root := base
	root.CorrelationID, root.CausationID, root.TraceContext, root.PartitionKey = "", "", "", ""
	cases["root-empty-lineage-and-key"] = root
	for _, fee := range []*commonpb.Money{nil, {CurrencyCode: "USD", Amount: &commonpb.Decimal{Coefficient: 0}}} {
		f := fill()
		f.Fee = fee
		f.ProtoReflect().SetUnknown([]byte{0xf8, 0x07, 0x81, 0})
		r := base
		r.PayloadSchemaRef = "order.v1.OrderFilled:1"
		r.Payload = wire(t, &orderpb.OrderFilled{OrderId: f.OrderId, Fill: f})
		name := "fill-fee-missing"
		if fee != nil {
			name = "fill-fee-present-zero"
		}
		cases[name] = r
	}
	first, second := fill(), fill()
	first.ProtoReflect().SetUnknown([]byte{0xf8, 0x07, 1})
	second.ProtoReflect().SetUnknown([]byte{0xf8, 0x07, 2})
	repeated := base
	repeated.PayloadSchemaRef = "order.v1.ExecutionFeeCorrectionProposed:1"
	repeated.Payload = wire(t, &orderpb.ExecutionFeeCorrectionProposed{CaseId: "case", Changes: []*orderpb.ExecutionFeeChange{{Previous: first, Revised: second}, {Previous: second, Revised: first}}})
	cases["repeated-fee-change-child-unknowns"] = repeated
	// Link and exercise the currently emitted schemas without a hand-written
	// alternate financial model. Empty messages here test schema transport only.
	for _, name := range []string{"order.v1.OrderAccepted", "order.v1.OrderPendingApproval", "order.v1.OrderApproved", "order.v1.OrderRejected", "order.v1.OrderRouted", "order.v1.OrderFilled", "order.v1.OrderPartiallyFilled", "order.v1.OrderExpired", "order.v1.ExecutionAttributionRecorded", "order.v1.ExecutionFeeCorrectionProposed", "order.v1.ExecutionFeeCorrectionApproved", "order.v1.ExecutionRecoveryRecorded", "order.v1.ExecutionRecovered", "order.v1.ExecutionRecoveryApplied", "command.v1.CommandOutcome", "domain.v1.PositionState"} {
		mt, err := protoregistry.GlobalTypes.FindMessageByName(protoreflect.FullName(name))
		if err != nil {
			t.Fatal(err)
		}
		r := base
		r.Payload = wire(t, mt.New().Interface())
		r.PayloadSchemaRef = name + ":1"
		cases[name] = r
	}
	unknown := base
	unknown.Payload = append(bytes.Clone(base.Payload), 0xf8, 0x07, 0x81, 0x00)
	cases["noncanonical-unknown-value"] = unknown
	group := base
	group.Payload = append(bytes.Clone(base.Payload), 0xfb, 0x07, 0x08, 0x81, 0x00, 0xfc, 0x07)
	cases["unknown-group"] = group
	// Duplicate embedded Decimal fragments must MERGE, not replace. The second
	// fragment changes only exponent; this is valid protobuf accepted by Go.
	duplicate := base
	fd := (&orderpb.OrderCancelled{}).ProtoReflect().Descriptor().Fields().ByName("cancelled_quantity")
	fragment := wire(t, &commonpb.Decimal{Exponent: -8})
	duplicate.Payload = protowire.AppendBytes(protowire.AppendTag(bytes.Clone(base.Payload), protowire.Number(fd.Number()), protowire.BytesType), fragment)
	cases["duplicate-embedded-merge"] = duplicate
	known := base
	known.Payload = append([]byte{0x0a, byte(len("frame-order")) | 0x80, 0}, []byte("frame-order")...)
	cases["noncanonical-known-length"] = known
	nested := base
	nested.Payload = protowire.AppendBytes(protowire.AppendTag(bytes.Clone(base.Payload), protowire.Number(fd.Number()), protowire.BytesType), []byte{0xf8, 0x07, 0x81, 0})
	cases["duplicate-embedded-unknown-only"] = nested
	// Resolve and decode the corpus even when the actual worker is absent.
	for name, record := range cases {
		if _, err := record.Event(); err != nil {
			t.Fatalf("Go corpus %s is invalid: %v", name, err)
		}
	}
	fixtureBinary(t)
	for name, record := range cases {
		t.Run(name, func(t *testing.T) {
			since := time.Now()
			got := runResult(t, map[string]any{"mode": "relay-frame", "records": []map[string]any{factJSON(record)}})
			expect(t, got.Status, "framed")
			sequence := uint64(1)
			if record.PartitionKey == "" {
				sequence = 0
			}
			verifyRelayFrame(t, record, got.State, sequence, since)
		})
	}
	t.Run("malformed-and-unavailable-schema", func(t *testing.T) {
		for _, schema := range []string{base.PayloadSchemaRef, "order.v1.FutureUnknown:1"} {
			r := base
			r.PayloadSchemaRef = schema
			if schema == base.PayloadSchemaRef {
				r.Payload = []byte{0xff}
			}
			expect(t, run(t, map[string]any{"mode": "relay-frame", "records": []map[string]any{factJSON(r)}}), "InvalidRecord")
		}
	})
}

type relayBroker struct {
	consumer jetstream.Consumer
	client   bus.Client
}

func newRelayBroker(t *testing.T, f *fixture) relayBroker {
	t.Helper()
	address := os.Getenv("TEST_NATS_URL")
	if address == "" {
		t.Skip("TEST_NATS_URL unavailable: actual relay broker required")
	}
	nc, err := nats.Connect(address, nats.Timeout(5*time.Second))
	if err != nil {
		t.Fatal("broker unavailable")
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	bustest.EnsureSubjects(t, f.ctx, js, "RELAY_"+f.schema, []string{relaySubject})
	stream, err := bustest.StreamFor(f.ctx, js, relaySubject)
	if err != nil {
		t.Fatal(err)
	}
	name := "relay-" + f.schema
	consumer, err := js.CreateConsumer(f.ctx, stream, jetstream.ConsumerConfig{Name: name, FilterSubject: relaySubject, DeliverPolicy: jetstream.DeliverNewPolicy, AckPolicy: jetstream.AckExplicitPolicy})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := js.DeleteConsumer(ctx, stream, name); err != nil {
			t.Error("fixture consumer cleanup failed")
		}
	})
	client, err := bus.DialNATS(f.ctx, bus.NATSConfig{URL: address, Name: "relay-go-oracle"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return relayBroker{consumer, client}
}

func (b relayBroker) next(t *testing.T, key string) []byte {
	t.Helper()
	for i := 0; i < 10000; i++ {
		msg, err := b.consumer.Next(jetstream.FetchMaxWait(5 * time.Second))
		if err != nil {
			t.Fatal("expected actual persisted broker frame absent")
		}
		env, _, err := bus.Unframe(msg.Data())
		if err != nil {
			_ = msg.Ack()
			continue
		}
		ackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err = msg.DoubleAck(ackCtx)
		cancel()
		if err != nil {
			t.Fatal("broker acknowledgement failed")
		}
		if env.PartitionKey == key {
			if msg.Headers().Get("Nats-Msg-Id") != env.IdempotencyKey || msg.Headers().Get("Kanz-Partition-Key") != key {
				t.Fatal("broker frame headers lost identity or partition")
			}
			return bytes.Clone(msg.Data())
		}
	}
	t.Fatal("foreign fixture traffic exceeded bound")
	return nil
}

func enqueueRelay(t *testing.T, f *fixture, service string, records ...outbox.Record) {
	t.Helper()
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(f.ctx) }()
	if outbox.Enqueue(f.ctx, tx, service, records...) != nil || tx.Commit(f.ctx) != nil {
		t.Fatal("canonical Go enqueue failed")
	}
}
func relayRequest(f *fixture, mode, key string, batch int) map[string]any {
	return map[string]any{"mode": "relay-" + mode, "schema": f.schema, "tenant": f.tenant, "order_id": key, "limit": batch}
}
func relayResult(t *testing.T, r fixtureResult, sent int, stall string) {
	t.Helper()
	if r.Status != "drained" || r.Sent != sent || r.Stalled != stall || r.Error != "" || !r.SessionsClosed {
		t.Fatalf("relay outcome %s/%d/%s/%s", r.Status, r.Sent, r.Stalled, r.Error)
	}
}
func pendingRelay(t *testing.T, f *fixture, key string) []outbox.Pending {
	t.Helper()
	rows, err := outbox.NewPostgres(f.pool, outbox.ServiceOMS).Pending(f.ctx, key, 256)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestRustRelayPostgresNATS(t *testing.T) {
	t.Run("full-batch-is-not-an-empty-queue-proof", func(t *testing.T) {
		f := newFixture(t)
		b := newRelayBroker(t, f)
		key := f.schema
		r := relayRecord(t, key, 1)
		enqueueRelay(t, f, outbox.ServiceOMS, r)
		since := time.Now()
		relayResult(t, runResult(t, relayRequest(f, "flush", key, 1)), 1, "FullBatch")
		verifyRelayFrame(t, r, b.next(t, key), 1, since)
		if len(pendingRelay(t, f, key)) != 0 {
			t.Fatal("full batch was not actually published")
		}
		relayResult(t, runResult(t, relayRequest(f, "flush", key, 1)), 0, "")
	})
	t.Run("ordered-restart", func(t *testing.T) {
		f := newFixture(t)
		b := newRelayBroker(t, f)
		key := f.schema
		records := []outbox.Record{relayRecord(t, key, 1), relayRecord(t, key, 2), relayRecord(t, key, 3)}
		enqueueRelay(t, f, outbox.ServiceOMS, records...)
		since := time.Now()
		relayResult(t, runResult(t, relayRequest(f, "flush", key, 2)), 2, "FullBatch")
		for i := 0; i < 2; i++ {
			verifyRelayFrame(t, records[i], b.next(t, key), uint64(i+1), since)
		}
		relayResult(t, runResult(t, relayRequest(f, "flush", key, 2)), 1, "")
		verifyRelayFrame(t, records[2], b.next(t, key), 1, since)
		relayResult(t, runResult(t, relayRequest(f, "flush", key, 2)), 0, "")
		if len(pendingRelay(t, f, key)) != 0 {
			t.Fatal("restart left acknowledged rows")
		}
	})
	t.Run("fail-first-retry", func(t *testing.T) {
		f := newFixture(t)
		b := newRelayBroker(t, f)
		key := f.schema
		r := relayRecord(t, key, 1)
		enqueueRelay(t, f, outbox.ServiceOMS, r, r)
		req := relayRequest(f, "flush", key, 10)
		req["other"] = "fail-first"
		relayResult(t, runResult(t, req), 0, "Broker")
		rows := pendingRelay(t, f, key)
		if len(rows) != 2 || rows[0].Attempts != 1 || rows[1].Attempts != 0 {
			t.Fatal("failed head did not stop key")
		}
		since := time.Now()
		req["other"] = ""
		relayResult(t, runResult(t, req), 2, "")
		for i := 1; i <= 2; i++ {
			verifyRelayFrame(t, r, b.next(t, key), uint64(i), since)
		}
	})
	t.Run("poison-key-isolation", func(t *testing.T) {
		f := newFixture(t)
		b := newRelayBroker(t, f)
		bad := relayRecord(t, f.schema+"-a", 1)
		bad.Payload = []byte{0xff}
		good := relayRecord(t, f.schema+"-b", 2)
		enqueueRelay(t, f, outbox.ServiceOMS, bad, bad, good)
		since := time.Now()
		relayResult(t, runResult(t, relayRequest(f, "once", "", 10)), 1, "")
		verifyRelayFrame(t, good, b.next(t, good.PartitionKey), 1, since)
		rows := pendingRelay(t, f, bad.PartitionKey)
		if len(rows) != 2 || rows[0].Attempts != 1 || rows[1].Attempts != 0 {
			t.Fatal("poison head skipped or unrelated key blocked")
		}
	})
	t.Run("real-broker-refusal-stops-key", func(t *testing.T) {
		f := newFixture(t)
		b := newRelayBroker(t, f)
		key := f.schema
		head := relayRecord(t, key, 1)
		head.Subject = "relayfixture." + f.schema + ".cancelled"
		head.EventType, head.Domain = head.Subject, "relayfixture"
		// Prove this is an actual missing-stream refusal, not a local malformed
		// record or injected publisher failure.
		nc, err := nats.Connect(os.Getenv("TEST_NATS_URL"), nats.Timeout(5*time.Second))
		if err != nil {
			t.Fatal("broker unavailable")
		}
		defer nc.Close()
		js, err := jetstream.New(nc)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := js.StreamNameBySubject(f.ctx, head.Subject); !errors.Is(err, jetstream.ErrStreamNotFound) {
			t.Fatal("refusal subject is not proven outside broker streams")
		}
		if _, err := head.Event(); err != nil {
			t.Fatal("canonical refusal record invalid")
		}
		enqueueRelay(t, f, outbox.ServiceOMS, head, relayRecord(t, key, 2))
		relayResult(t, runResult(t, relayRequest(f, "flush", key, 10)), 0, "Broker")
		rows := pendingRelay(t, f, key)
		if len(rows) != 2 || rows[0].Attempts != 1 || rows[1].Attempts != 0 {
			t.Fatal("broker refusal did not retain the failed head and untouched successor")
		}
		b.noNext(t, key)
	})
	t.Run("publish-mark-rollback-redelivery", func(t *testing.T) {
		f := newFixture(t)
		b := newRelayBroker(t, f)
		key := f.schema
		r := relayRecord(t, key, 1)
		enqueueRelay(t, f, outbox.ServiceOMS, r)
		_, err := f.pool.Exec(f.ctx, `CREATE FUNCTION refuse_relay_mark() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'fixture'; END$$; CREATE CONSTRAINT TRIGGER refuse_relay_mark AFTER UPDATE ON outbox DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION refuse_relay_mark()`)
		if err != nil {
			t.Fatal(err)
		}
		since := time.Now()
		result := runResult(t, relayRequest(f, "flush", key, 10))
		if result.Status != "error" || !result.SessionsClosed || result.Sent != 0 || len(pendingRelay(t, f, key)) != 1 {
			t.Fatal("failed mark claimed durable publication")
		}
		first := verifyRelayFrame(t, r, b.next(t, key), 1, since)
		if _, err = f.pool.Exec(f.ctx, "DROP TRIGGER refuse_relay_mark ON outbox"); err != nil {
			t.Fatal(err)
		}
		relayResult(t, runResult(t, relayRequest(f, "flush", key, 10)), 1, "")
		second := verifyRelayFrame(t, r, b.next(t, key), 1, since)
		if first.Envelope.EventId == second.Envelope.EventId {
			t.Fatal("Go per-attempt identity contract changed")
		}
	})
	t.Run("mark-failed-refusal", func(t *testing.T) {
		f := newFixture(t)
		_ = newRelayBroker(t, f)
		key := f.schema
		enqueueRelay(t, f, outbox.ServiceOMS, relayRecord(t, key, 1))
		if _, err := f.pool.Exec(f.ctx, `CREATE FUNCTION refuse_attempt() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'fixture'; END$$; CREATE TRIGGER refuse_attempt BEFORE UPDATE ON outbox FOR EACH ROW EXECUTE FUNCTION refuse_attempt()`); err != nil {
			t.Fatal(err)
		}
		req := relayRequest(f, "flush", key, 10)
		req["other"] = "fail-first"
		result := runResult(t, req)
		rows := pendingRelay(t, f, key)
		if result.Status != "error" || !result.SessionsClosed || result.Sent != 0 || len(rows) != 1 || rows[0].Attempts != 0 {
			t.Fatal("failed attempt persistence was hidden")
		}
	})
	t.Run("stop-joins-inflight", func(t *testing.T) {
		f := newFixture(t)
		b := newRelayBroker(t, f)
		key := f.schema
		r := relayRecord(t, key, 1)
		enqueueRelay(t, f, outbox.ServiceOMS, r, r)
		req := relayRequest(f, "run", key, 10)
		req["other"] = "block-first"
		req["relay_hold_ms"] = 500
		req["relay_stop_after_ms"] = 100
		since := time.Now()
		relayResult(t, runResult(t, req), 1, "Stopped")
		if time.Since(since) < 450*time.Millisecond || len(pendingRelay(t, f, key)) != 1 {
			t.Fatal("run returned before owned publication completed")
		}
		verifyRelayFrame(t, r, b.next(t, key), 1, since)
		b.noNext(t, key)
		relayResult(t, runResult(t, relayRequest(f, "flush", key, 10)), 1, "")
		verifyRelayFrame(t, r, b.next(t, key), 1, since)
	})
	t.Run("competing-go-rust", func(t *testing.T) {
		f := newFixture(t)
		b := newRelayBroker(t, f)
		key := f.schema
		for i := int64(1); i <= 3; i++ {
			enqueueRelay(t, f, outbox.ServiceOMS, relayRecord(t, key, i))
		}
		producer, err := bus.NewProducer(b.client, bus.ProducerConfig{Source: "oms/relay-fixture", ProducerVersion: os.Getenv("KANZ_OMS_STORE_VERSION")})
		if err != nil {
			t.Fatal(err)
		}
		goRelay, err := outbox.NewRelay(outbox.NewPostgres(f.pool, outbox.ServiceOMS), producer, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		ch := make(chan fixtureResult, 1)
		since := time.Now()
		go func() { defer close(ch); ch <- runResult(t, relayRequest(f, "flush", key, 10)) }()
		n, err := goRelay.Flush(f.ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		r, ok := <-ch
		if !ok || r.Status != "drained" || !r.SessionsClosed || r.Sent+n != 3 || len(pendingRelay(t, f, key)) != 0 {
			t.Fatal("concurrent relays lost or duplicated durable marks")
		}
		for i := int64(1); i <= 3; i++ {
			verifyRelayFrame(t, relayRecord(t, key, i), b.next(t, key), uint64(i), since)
		}
	})
	t.Run("tenant-service-isolation", func(t *testing.T) {
		f := newFixture(t)
		b := newRelayBroker(t, f)
		key := f.schema
		r := relayRecord(t, key, 1)
		enqueueRelay(t, f, outbox.ServiceOMS, r)
		enqueueRelay(t, f, outbox.ServiceAccounting, r)
		other := &fixture{t: t, ctx: f.ctx, schema: f.schema, tenant: "relay-storage-b", pool: f.open("relay-storage-b")}
		enqueueRelay(t, other, outbox.ServiceOMS, r)
		since := time.Now()
		relayResult(t, runResult(t, relayRequest(f, "flush", key, 10)), 1, "")
		verifyRelayFrame(t, r, b.next(t, key), 1, since)
		foreign, err := outbox.NewPostgres(f.pool, outbox.ServiceAccounting).Pending(f.ctx, key, 10)
		if err != nil || len(foreign) != 1 || len(pendingRelay(t, other, key)) != 1 {
			t.Fatal("foreign tenant/service row claimed")
		}
		relayResult(t, runResult(t, relayRequest(other, "flush", key, 10)), 1, "")
		verifyRelayFrame(t, r, b.next(t, key), 1, since)
	})
}

func (b relayBroker) noNext(t *testing.T, key string) {
	t.Helper()
	deadline := time.Now().Add(250 * time.Millisecond)
	for count := 0; count < 10000 && time.Now().Before(deadline); count++ {
		msg, err := b.consumer.Next(jetstream.FetchMaxWait(100 * time.Millisecond))
		if errors.Is(err, nats.ErrTimeout) {
			continue
		}
		if err != nil {
			t.Fatal("broker absence control unavailable")
		}
		env, _, err := bus.Unframe(msg.Data())
		_ = msg.Ack()
		if err == nil && env.PartitionKey == key {
			t.Fatal("stopped relay published the next record")
		}
	}
	if time.Now().Before(deadline) {
		t.Fatal("broker absence control traffic exceeded bound")
	}
}
