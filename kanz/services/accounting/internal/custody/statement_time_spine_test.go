package custody

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/bustest"
	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestPostgresJetStreamStatementTimesDLQAndReplay(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" || os.Getenv("TEST_POSTGRES_URL") == "" {
		t.Skip("requires real restricted-role PostgreSQL and JetStream")
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	tenant := "custody-time-" + suffix
	ctx, cancel := context.WithTimeout(bus.WithTenantID(t.Context(), tenant), 60*time.Second)
	defer cancel()
	pool := newCustodyPool(t, tenant)
	requireRestrictedCustodyRole(t, pool)
	applyCustodySchema(t, pool)
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	// Source and DLQ have separate message-ID deduplication domains, including
	// when the fixture must provision streams on an otherwise empty test broker.
	bustest.EnsureSubjects(t, ctx, js, "CUSTODY_TIME_INPUT_"+suffix, []string{SubjectStatement})
	bustest.EnsureSubjects(t, ctx, js, "CUSTODY_TIME_DLQ_"+suffix, []string{"dlq." + SubjectStatement})
	inputStream, err := bustest.StreamFor(ctx, js, SubjectStatement)
	if err != nil {
		t.Fatal(err)
	}
	deadStream, err := bustest.StreamFor(ctx, js, "dlq."+SubjectStatement)
	if err != nil {
		t.Fatal(err)
	}
	observer := func(stream, subject, name string, policy jetstream.DeliverPolicy) jetstream.Consumer {
		t.Helper()
		consumer, err := js.CreateConsumer(ctx, stream, jetstream.ConsumerConfig{Name: name, FilterSubject: subject, DeliverPolicy: policy, AckPolicy: jetstream.AckExplicitPolicy})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = js.DeleteConsumer(context.Background(), stream, name) })
		return consumer
	}
	next := func(consumer jetstream.Consumer) jetstream.Msg {
		t.Helper()
		message, err := consumer.Next(jetstream.FetchMaxWait(5 * time.Second))
		if err != nil {
			t.Fatal(err)
		}
		return message
	}
	inputs := observer(inputStream, SubjectStatement, "time-input-wire-"+suffix, jetstream.DeliverNewPolicy)
	deadLetters := observer(deadStream, "dlq."+SubjectStatement, "time-dlq-"+suffix, jetstream.DeliverNewPolicy)
	type result struct {
		id  string
		err error
	}
	handled := make(chan result, 32)
	start := func(store Store, group string) (*bus.NATSClient, func()) {
		t.Helper()
		client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: group})
		if err != nil {
			t.Fatal(err)
		}
		consumer, err := bus.NewConsumer(client, bus.WithDLQ(client))
		if err != nil {
			t.Fatal(err)
		}
		owner, err := NewStatementConsumer(store, tenant, testLogger())
		if err != nil {
			t.Fatal(err)
		}
		inputCtx, stop := context.WithCancel(ctx)
		ready, done := make(chan struct{}), make(chan error, 1)
		go func() {
			done <- consumer.SubscribeReady(inputCtx, SubjectStatement, group, func(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
				if env.GetTenantId() != tenant {
					return nil
				}
				err := owner.Handle(ctx, env, payload)
				select {
				case handled <- result{id: env.GetEventId(), err: err}:
				case <-ctx.Done():
					return ctx.Err()
				}
				return err
			}, func() { close(ready) })
		}()
		stopped := false
		join := func() {
			if stopped {
				return
			}
			stopped = true
			stop()
			select {
			case err := <-done:
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Error(err)
				}
			case <-time.After(10 * time.Second):
				t.Error("statement-time subscriber did not join")
			}
			_ = client.Close()
		}
		t.Cleanup(join)
		select {
		case <-ready:
		case err := <-done:
			stopped = true
			stop()
			_ = client.Close()
			t.Fatalf("statement subscription exited before readiness: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		return client, join
	}
	// Read the actual broker durable, rather than treating a handler callback as
	// proof of ACK: the callback precedes the bus's DLQ publish and source ACK.
	assertSourceAcked := func(group string) {
		t.Helper()
		stream, err := js.Stream(ctx, inputStream)
		if err != nil {
			t.Fatal(err)
		}
		var name string
		listing := stream.ListConsumers(ctx)
		for info := range listing.Info() {
			if strings.HasPrefix(info.Name, group+"-") && info.Config.FilterSubject == SubjectStatement {
				if name != "" {
					t.Fatal("ambiguous fixture source durable")
				}
				name = info.Name
			}
		}
		if listing.Err() != nil || name == "" {
			t.Fatalf("cannot find actual source durable: %v", listing.Err())
		}
		source, err := js.Consumer(ctx, inputStream, name)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = js.DeleteConsumer(context.Background(), inputStream, name) })
		for {
			info, err := source.Info(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if info.NumAckPending == 0 && info.NumPending == 0 {
				return
			}
			select {
			case <-time.After(10 * time.Millisecond):
			case <-ctx.Done():
				t.Fatal("source message was not acknowledged after durable/DLQ handling")
			}
		}
	}
	group := "time-input-" + suffix
	client, stop := start(NewPostgres(pool), group)
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "custody-time-fixture", ProducerVersion: "test", Tenant: tenant})
	if err != nil {
		t.Fatal(err)
	}
	type retained struct {
		frame    []byte
		sequence uint64
		field    string
	}
	originals := map[string]retained{}
	publish := func(msg *accountingpb.CustodianStatement, field string) {
		t.Helper()
		// This valid envelope time is independent of the deliberately malformed
		// received_at payload. Envelope refusal would not exercise this decoder.
		if err := producer.Publish(ctx, bus.Event{Subject: SubjectStatement, EventType: SubjectStatement, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT,
			SchemaVersion: 1, Domain: "accounting", EventTime: time.Date(2026, 11, 14, 15, 0, 0, 0, time.UTC), PartitionKey: "TIME-PF", PayloadSchemaRef: schemaRefStatement, Payload: msg}); err != nil {
			t.Fatal(err)
		}
		var got result
		select {
		case got = <-handled:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if (field == "" && got.err != nil) || (field != "" && (got.err == nil || !strings.Contains(got.err.Error(), field))) {
			t.Fatalf("statement dispatch: field=%s err=%v", field, got.err)
		}
		input := next(inputs)
		env, payload, err := bus.Unframe(input.Data())
		if err != nil || env.GetEventId() != got.id || env.GetTenantId() != tenant {
			t.Fatalf("ingress identity/frame changed: %v", err)
		}
		var wire accountingpb.CustodianStatement
		if err := proto.Unmarshal(payload, &wire); err != nil || !proto.Equal(&wire, msg) {
			t.Fatalf("broker ingress changed payload representation: %v", err)
		}
		meta, err := input.Metadata()
		if err != nil {
			t.Fatal(err)
		}
		originals[got.id] = retained{frame: bytes.Clone(input.Data()), sequence: meta.Sequence.Stream, field: field}
		if err := input.DoubleAck(ctx); err != nil {
			t.Fatal(err)
		}
		if field == "" {
			return
		}
		dead := next(deadLetters)
		if !bytes.Equal(dead.Data(), originals[got.id].frame) || dead.Headers().Get(bus.HeaderDLQOriginalSubject) != SubjectStatement ||
			dead.Headers().Get(bus.HeaderDLQAttempts) != "1" || !strings.Contains(dead.Headers().Get(bus.HeaderDLQError), field) {
			t.Fatal("DLQ lost original malformed frame, source subject or decoder reason")
		}
		first, err := dead.Metadata()
		if err != nil {
			t.Fatal(err)
		}
		if err := dead.Nak(); err != nil {
			t.Fatal(err)
		}
		redelivered := next(deadLetters)
		second, err := redelivered.Metadata()
		if err != nil || second.Sequence.Stream != first.Sequence.Stream || second.NumDelivered < 2 || !bytes.Equal(redelivered.Data(), dead.Data()) {
			t.Fatalf("DLQ was not genuinely redelivered unchanged: %v", err)
		}
		if err := redelivered.DoubleAck(ctx); err != nil {
			t.Fatal(err)
		}
	}
	publish(statementTimeDurableWire(), "")
	for _, field := range statementTimeFields() {
		for _, id := range []string{"retained-time", "new-invalid-time"} {
			msg := statementTimeDurableWire()
			msg.StatementId = id
			field.set(msg, &timestamppb.Timestamp{Seconds: 0, Nanos: -1})
			publish(msg, field.name)
		}
	}
	// A valid fresh event after the malformed ones must still be admitted.
	publish(statementTimeDurableWire(), "")
	assertSourceAcked(group)
	stop()
	pool.Close()
	reconnected := newCustodyPool(t, tenant)
	requireRestrictedCustodyRole(t, reconnected)
	assertStored := func() {
		t.Helper()
		got, err := NewPostgres(reconnected).LatestStatement(ctx, statementTimeSubject())
		if err != nil {
			t.Fatal(err)
		}
		assertStatementTimeDurable(t, got)
		var count int
		if err := reconnected.QueryRow(ctx, `SELECT count(*) FROM custody_statements`).Scan(&count); err != nil || count != 1 {
			t.Fatalf("invalid or duplicate ingress persisted: count=%d err=%v", count, err)
		}
	}
	assertStored()
	// A fresh client/store and group replay the retained input FACTs, not newly
	// republished copies. Broker DLQ deduplication may suppress repeat parks for
	// its two-minute window; replay does not promise another copy of each DLQ.
	replayGroup := "time-replay-" + suffix
	_, stopReplay := start(NewPostgres(reconnected), replayGroup)
	seen := map[string]bool{}
	for range len(originals) {
		select {
		case got := <-handled:
			old, exists := originals[got.id]
			if !exists || seen[got.id] || (old.field == "" && got.err != nil) || (old.field != "" && (got.err == nil || !strings.Contains(got.err.Error(), old.field))) {
				t.Fatalf("retained replay changed dispatch/idempotency: id=%s err=%v", got.id, got.err)
			}
			seen[got.id] = true
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	assertSourceAcked(replayGroup)
	stopReplay()
	assertStored()
	// Read the original broker sequences again as a separate retained replay
	// observation. A new consumer is replay, not same-consumer redelivery.
	replayedWire := observer(inputStream, SubjectStatement, "time-replay-wire-"+suffix, jetstream.DeliverAllPolicy)
	remaining := len(originals)
	for remaining > 0 {
		message := next(replayedWire)
		env, _, err := bus.Unframe(message.Data())
		if err != nil {
			t.Fatal(err)
		}
		if env.GetTenantId() == tenant {
			old, exists := originals[env.GetEventId()]
			meta, err := message.Metadata()
			if !exists || err != nil || old.sequence != meta.Sequence.Stream || !bytes.Equal(message.Data(), old.frame) {
				t.Fatalf("retained original sequence/frame changed: %v", err)
			}
			remaining--
		}
		if err := message.DoubleAck(ctx); err != nil {
			t.Fatal(err)
		}
	}
}
