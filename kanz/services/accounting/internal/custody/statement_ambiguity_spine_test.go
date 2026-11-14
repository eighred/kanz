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
	"github.com/eighred/kanz/internal/outbox"
	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestPostgresJetStreamAmbiguousStatementReplayAndRecovery(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" || os.Getenv("TEST_POSTGRES_URL") == "" {
		t.Skip("requires real restricted-role PostgreSQL and JetStream")
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	tenant := "custody-ambiguity-" + suffix
	pool := newCustodyPool(t, tenant)
	requireRestrictedCustodyRole(t, pool)
	applyCustodySchema(t, pool)
	ctx, cancel := context.WithTimeout(bus.WithTenantID(t.Context(), tenant), 60*time.Second)
	defer cancel()
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	bustest.EnsureSubjects(t, ctx, js, "CUSTODY_AMBIGUITY_"+suffix, []string{SubjectStatement, SubjectRun, SubjectActionRecorded})
	inputStream, err := bustest.StreamFor(ctx, js, SubjectStatement)
	if err != nil {
		t.Fatal(err)
	}
	outputStream, err := bustest.StreamFor(ctx, js, SubjectRun)
	if err != nil {
		t.Fatal(err)
	}
	outputName := "ambiguity-output-" + suffix
	output, err := js.CreateConsumer(ctx, outputStream, jetstream.ConsumerConfig{Name: outputName, FilterSubject: SubjectRun, DeliverPolicy: jetstream.DeliverNewPolicy, AckPolicy: jetstream.AckExplicitPolicy})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = js.DeleteConsumer(context.Background(), outputStream, outputName) })
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "custody-ambiguity-" + suffix})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "custody-fixture", ProducerVersion: "test", Tenant: tenant})
	if err != nil {
		t.Fatal(err)
	}
	type delivery struct {
		env     *envelopepb.Envelope
		payload []byte
		err     error
	}
	handled := make(chan delivery, 16)
	start := func(store Store, group string) func() {
		t.Helper()
		consumer, err := bus.NewConsumer(client, bus.WithDLQ(client), bus.WithDedupWindow(0, 0))
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
				case handled <- delivery{env: proto.Clone(env).(*envelopepb.Envelope), payload: bytes.Clone(payload), err: err}:
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
				t.Error("statement ambiguity consumer did not join")
			}
		}
		t.Cleanup(join)
		select {
		case <-ready:
		case err := <-done:
			stopped = true
			stop()
			t.Fatalf("statement subscription exited: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		return join
	}
	next := func() delivery {
		t.Helper()
		select {
		case d := <-handled:
			if d.err != nil {
				t.Fatalf("valid source statement was refused at ingest: %v", d.err)
			}
			if err := bus.Validate(d.env); err != nil {
				t.Fatal(err)
			}
			return d
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		return delivery{}
	}
	assertAcked := func(group string) {
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
					t.Fatal("ambiguous fixture durable")
				}
				name = info.Name
			}
		}
		if listing.Err() != nil || name == "" {
			t.Fatalf("source durable missing: %v", listing.Err())
		}
		t.Cleanup(func() { _ = js.DeleteConsumer(context.Background(), inputStream, name) })
		source, err := js.Consumer(ctx, inputStream, name)
		if err != nil {
			t.Fatal(err)
		}
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
				t.Fatal("source receipt was not durably acknowledged")
			}
		}
	}
	group := "ambiguity-input-" + suffix
	stop := start(NewPostgres(pool), group)
	originals := map[string]delivery{}
	publish := func(id string, quantity int64, received time.Time) delivery {
		t.Helper()
		msg := &accountingpb.CustodianStatement{StatementId: id, PortfolioId: "PF1", CustodianId: "CUST-A", BusinessDate: timestamppb.New(BusinessDay(t0)), ReceivedAt: timestamppb.New(received),
			Positions: []*accountingpb.StatementPosition{{InstrumentId: "AAPL", Quantity: &commonpb.Decimal{Coefficient: quantity}}}}
		if err := producer.Publish(ctx, bus.Event{Subject: SubjectStatement, EventType: SubjectStatement, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT,
			SchemaVersion: 1, Domain: "accounting", EventTime: received, PartitionKey: "PF1", PayloadSchemaRef: schemaRefStatement, Payload: msg}); err != nil {
			t.Fatal(err)
		}
		d := next()
		var wire accountingpb.CustodianStatement
		if err := proto.Unmarshal(d.payload, &wire); err != nil || !proto.Equal(&wire, msg) || d.env.GetTenantId() != tenant {
			t.Fatalf("canonical statement identity/payload changed: %v", err)
		}
		return d
	}
	for i, quantity := range []int64{90, 100} {
		d := publish(fmt.Sprintf("ambiguous-%d", i), quantity, t0)
		originals[d.env.GetEventId()] = d
	}
	assertAcked(group)
	stop()
	store := NewPostgres(pool)
	prior := investigatedReferenceBreaks(t, ctx, store)
	r, err := NewReconciler(store, loaderFor(bookWith(map[string]int64{"AAPL": 100}, nil)), nil, nil, nil, testLogger(), func() time.Time { return t0 })
	if err != nil {
		t.Fatal(err)
	}
	failed, err := r.Reconcile(ctx, subject())
	if !errors.Is(err, ErrAmbiguousStatement) || !errors.Is(err, ErrPublicationPending) || failed.Outcome != OutcomeFailed {
		t.Fatalf("tied canonical receipts were assessed: %+v %v", failed, err)
	}
	assertReferenceLifecycle(t, ctx, store, prior)
	assertReferenceRunRows(t, ctx, store, 0)
	var originalPayload []byte
	if err := pool.QueryRow(ctx, `SELECT payload FROM outbox WHERE event_type=$1 AND correlation_id=$2`, SubjectRun, failed.RunID).Scan(&originalPayload); err != nil {
		t.Fatal(err)
	}
	pool.Close()
	reconnected := newCustodyPool(t, tenant)
	requireRestrictedCustodyRole(t, reconnected)
	restarted := NewPostgres(reconnected)
	relay, err := outbox.NewRelay(restarted.Outbox(), producer, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if count, err := relay.DrainOnce(ctx); err != nil || count != 4 {
		t.Fatalf("restart lost action/failure delivery: count=%d err=%v", count, err)
	}
	var frame []byte
	var sequence uint64
	for attempt := range 2 {
		message, err := output.Next(jetstream.FetchMaxWait(5 * time.Second))
		if err != nil {
			t.Fatal(err)
		}
		env, payload, err := bus.Unframe(message.Data())
		if err != nil || bus.Validate(env) != nil || env.GetTenantId() != tenant || env.GetCorrelationId() != failed.RunID || !bytes.Equal(payload, originalPayload) {
			t.Fatalf("durable refusal changed on the real spine: %v", err)
		}
		var fact accountingpb.ReconciliationRun
		if err := proto.Unmarshal(payload, &fact); err != nil {
			t.Fatal(err)
		}
		assertAmbiguousStatementFact(t, &fact, failed)
		metadata, err := message.Metadata()
		if err != nil {
			t.Fatal(err)
		}
		if attempt == 0 {
			frame, sequence = bytes.Clone(message.Data()), metadata.Sequence.Stream
			if err := message.Nak(); err != nil {
				t.Fatal(err)
			}
		} else {
			if metadata.Sequence.Stream != sequence || metadata.NumDelivered < 2 || !bytes.Equal(frame, message.Data()) {
				t.Fatal("failure was not redelivered from retained original bytes")
			}
			if err := message.DoubleAck(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	// A fresh group replays the original retained input IDs/payloads. No source
	// publication occurs until both have been observed and acknowledged again.
	replayGroup := "ambiguity-replay-" + suffix
	stopReplay := start(restarted, replayGroup)
	for range len(originals) {
		d := next()
		original, ok := originals[d.env.GetEventId()]
		if !ok || !proto.Equal(d.env, original.env) || !bytes.Equal(d.payload, original.payload) {
			t.Fatal("fresh group did not replay an original canonical statement")
		}
		delete(originals, d.env.GetEventId())
	}
	assertAcked(replayGroup)
	if _, err := restarted.LatestStatement(ctx, subject()); !errors.Is(err, ErrAmbiguousStatement) {
		t.Fatalf("retained replay resolved an unproven ordering: %v", err)
	}
	assertReferenceLifecycle(t, ctx, restarted, prior)
	assertReferenceRunRows(t, ctx, restarted, 1)
	newer := publish("unique-newer", 100, t0.Add(time.Hour))
	if newer.env.GetEventId() == "" {
		t.Fatal("recovery had no independent source identity")
	}
	assertAcked(replayGroup)
	stopReplay()
	afterRestart, err := NewReconciler(restarted, loaderFor(bookWith(map[string]int64{"AAPL": 100}, nil)), producer, nil, nil, testLogger(), func() time.Time { return t0.Add(2 * time.Hour) })
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := afterRestart.Reconcile(ctx, subject())
	if err != nil || recovered.Outcome != OutcomeClean || recovered.StatementID != "unique-newer" || recovered.RunID == failed.RunID {
		t.Fatalf("unique newer evidence did not recover comparison: %+v %v", recovered, err)
	}
	message, err := output.Next(jetstream.FetchMaxWait(5 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	env, payload, err := bus.Unframe(message.Data())
	if err != nil || bus.Validate(env) != nil || env.GetTenantId() != tenant || env.GetCorrelationId() != recovered.RunID {
		t.Fatalf("recovery FACT lost its identity: %v", err)
	}
	var clean accountingpb.ReconciliationRun
	if err := proto.Unmarshal(payload, &clean); err != nil || clean.GetOutcome() != accountingpb.ReconciliationOutcome_RECONCILIATION_OUTCOME_CLEAN || clean.GetStatementId() != "unique-newer" {
		t.Fatalf("real spine did not carry the separately supported recovery: %v %v", &clean, err)
	}
	if err := message.DoubleAck(ctx); err != nil {
		t.Fatal(err)
	}
	info, err := output.Info(ctx)
	if err != nil || info.NumAckPending != 0 || info.NumPending != 0 || info.AckFloor.Stream <= sequence {
		t.Fatalf("failure and recovery were not durably acknowledged: %+v %v", info, err)
	}
	var retryPayload []byte
	if err := reconnected.QueryRow(ctx, `SELECT payload FROM outbox WHERE event_type=$1 AND correlation_id=$2`, SubjectRun, failed.RunID).Scan(&retryPayload); err != nil || !bytes.Equal(originalPayload, retryPayload) {
		t.Fatalf("recovery rewrote earlier immutable failure: %v", err)
	}
}
