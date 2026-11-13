package custody

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/bustest"
	"github.com/eighred/kanz/internal/outbox"
	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/eighred/kanz/services/accounting/internal/recon"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/proto"
)

func TestAmbiguousReferenceFailureRecoversThroughRealSpine(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" || os.Getenv("TEST_POSTGRES_URL") == "" {
		t.Skip("requires real restricted-role PostgreSQL and JetStream")
	}
	tenant := fmt.Sprintf("ambiguous-reference-%d", time.Now().UnixNano())
	pool := newCustodyPool(t, tenant)
	requireRestrictedCustodyRole(t, pool)
	applyCustodySchema(t, pool)
	ctx, cancel := context.WithTimeout(bus.WithTenantID(t.Context(), tenant), 45*time.Second)
	defer cancel()
	store := NewPostgres(pool)
	if err := store.SaveStatement(ctx, ambiguousReferenceStatement()); err != nil {
		t.Fatal(err)
	}
	prior := investigatedReferenceBreaks(t, ctx, store)
	loader := ambiguousReferenceBook(t, ctx, ledger.NewPostgres(pool))
	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)
	metrics.SeedPair("PF1", "CUST-A")
	r, err := NewReconciler(store, loader, nil, nil, metrics, testLogger(), func() time.Time { return t0 })
	if err != nil {
		t.Fatal(err)
	}
	failed, err := r.Reconcile(ctx, subject())
	if !errors.Is(err, recon.ErrAmbiguousExecutionReference) || !errors.Is(err, ErrPublicationPending) || failed.Outcome != OutcomeFailed {
		t.Fatalf("committed refusal was lost behind publication state: %+v %v", failed, err)
	}
	assertReferenceRunRows(t, ctx, store, 0)
	assertReferenceLifecycle(t, ctx, store, prior)
	var originalPayload []byte
	if err := pool.QueryRow(ctx, `SELECT payload FROM outbox WHERE event_type=$1 AND correlation_id=$2`, SubjectRun, failed.RunID).Scan(&originalPayload); err != nil {
		t.Fatal(err)
	}

	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	bustest.EnsureSubjects(t, ctx, js, fmt.Sprintf("AMBIGUOUS_REFERENCE_%d", time.Now().UnixNano()), []string{SubjectRun, SubjectActionRecorded})
	stream, err := bustest.StreamFor(ctx, js, SubjectRun)
	if err != nil {
		t.Fatal(err)
	}
	consumerName := fmt.Sprintf("ambiguous-reference-%d", time.Now().UnixNano())
	consumer, err := js.CreateConsumer(ctx, stream, jetstream.ConsumerConfig{Name: consumerName, FilterSubject: SubjectRun, DeliverPolicy: jetstream.DeliverNewPolicy, AckPolicy: jetstream.AckExplicitPolicy})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = js.DeleteConsumer(context.Background(), stream, consumerName) })
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "ambiguous-reference-restart"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "accounting", ProducerVersion: "test", Tenant: tenant})
	if err != nil {
		t.Fatal(err)
	}
	restarted := NewPostgres(pool)
	relay, err := outbox.NewRelay(restarted.Outbox(), producer, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	// A fresh service-owned relay recovers the three audited operator actions
	// and the one FAILED completion after stopping before any broker publish.
	if count, err := relay.DrainOnce(ctx); err != nil || count != 4 {
		t.Fatalf("durable restart recovery: count=%d err=%v", count, err)
	}
	var packet []byte
	var sequence uint64
	for delivery := range 2 {
		msg, err := consumer.Next(jetstream.FetchMaxWait(5 * time.Second))
		if err != nil {
			t.Fatal(err)
		}
		env, payload, err := bus.Unframe(msg.Data())
		if err != nil {
			t.Fatal(err)
		}
		if err := bus.Validate(env); err != nil {
			t.Fatal(err)
		}
		var fact accountingpb.ReconciliationRun
		if err := proto.Unmarshal(payload, &fact); err != nil {
			t.Fatal(err)
		}
		if env.GetTenantId() != tenant || env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_FACT || env.GetCorrelationId() != failed.RunID ||
			fact.GetRunId() != failed.RunID || fact.GetOutcome() != accountingpb.ReconciliationOutcome_RECONCILIATION_OUTCOME_FAILED ||
			fact.GetFailureReason() != failed.FailureReason || len(fact.GetBreaks()) != 0 || !bytes.Equal(payload, originalPayload) {
			t.Fatalf("wrong immutable failure FACT: %v %v", env, &fact)
		}
		meta, err := msg.Metadata()
		if err != nil {
			t.Fatal(err)
		}
		if delivery == 0 {
			packet, sequence = append([]byte(nil), msg.Data()...), meta.Sequence.Stream
			if err := msg.Nak(); err != nil {
				t.Fatal(err)
			}
		} else {
			if meta.Sequence.Stream != sequence || meta.NumDelivered < 2 || !bytes.Equal(packet, msg.Data()) {
				t.Fatal("broker did not redeliver the original immutable failure")
			}
			if err := msg.DoubleAck(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	afterRestart, err := NewReconciler(restarted, loader, producer, nil, metrics, testLogger(), func() time.Time { return t0 })
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := afterRestart.Reconcile(ctx, subject())
	if !errors.Is(err, recon.ErrAmbiguousExecutionReference) || errors.Is(err, ErrPublicationPending) || repeated.RunID != failed.RunID {
		t.Fatalf("retry changed the committed refusal: %+v %v", repeated, err)
	}
	assertReferenceRunRows(t, ctx, restarted, 1)
	assertReferenceLifecycle(t, ctx, restarted, prior)
	if count, err := relay.DrainOnce(ctx); err != nil || count != 0 {
		t.Fatalf("retry queued another transition: count=%d err=%v", count, err)
	}
	info, err := consumer.Info(ctx)
	if err != nil || info.NumPending != 0 || info.NumAckPending != 0 || info.AckFloor.Stream < sequence {
		t.Fatalf("failure completion was not durably acknowledged: %+v %v", info, err)
	}
	var retryPayload []byte
	if err := pool.QueryRow(ctx, `SELECT payload FROM outbox WHERE event_type=$1 AND correlation_id=$2`, SubjectRun, failed.RunID).Scan(&retryPayload); err != nil || !bytes.Equal(originalPayload, retryPayload) {
		t.Fatalf("retry rewrote the committed failure: %v", err)
	}
	if fresh, _ := gaugeValue(t, reg, "kanz_accounting_reconciliation_last_success_timestamp_seconds", map[string]string{"portfolio": "PF1", "custodian": "CUST-A"}); fresh != 0 {
		t.Fatal("pending/published FAILED run advanced assessment freshness")
	}
}
