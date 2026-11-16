package custody

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"os"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/bustest"
	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/outbox"
	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
)

func TestCustodyCommittedRunRecoversThroughRealSpineAndRedelivery(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" || os.Getenv("TEST_POSTGRES_URL") == "" {
		t.Skip("requires real restricted-role PostgreSQL and JetStream")
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	tenant := "custody-run-" + suffix
	pool := newCustodyPool(t, tenant)
	requireRestrictedCustodyRole(t, pool)
	applyCustodySchema(t, pool)
	ctx, cancel := context.WithTimeout(bus.WithTenantID(t.Context(), tenant), 40*time.Second)
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
	bustest.EnsureSubjects(t, ctx, js, "CUSTODY_RUN_"+suffix, []string{SubjectRun})
	stream, err := bustest.StreamFor(ctx, js, SubjectRun)
	if err != nil {
		t.Fatal(err)
	}
	consumerName := "custody-run-" + suffix
	consumer, err := js.CreateConsumer(ctx, stream, jetstream.ConsumerConfig{Name: consumerName, FilterSubject: SubjectRun, DeliverPolicy: jetstream.DeliverNewPolicy, AckPolicy: jetstream.AckExplicitPolicy})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = js.DeleteConsumer(context.Background(), stream, consumerName) })

	st := NewPostgres(pool)
	if err := st.SaveStatement(ctx, statement("S1", map[string]int64{"AAPL": 90}, nil)); err != nil {
		t.Fatal(err)
	}
	book := loaderFor(bookWith(map[string]int64{"AAPL": 100}, nil))
	beforeRestart, err := NewReconciler(st, book, nil, nil, nil, testLogger(), func() time.Time { return t0 })
	if err != nil {
		t.Fatal(err)
	}
	run, err := beforeRestart.Reconcile(ctx, subject())
	if !errors.Is(err, ErrPublicationPending) || run.Outcome != OutcomeBreaks || len(run.Breaks) != 1 {
		t.Fatalf("business commit/pending delivery confused: %+v %v", run, err)
	}
	// Simulate stopping after the business commit and before publishing: a new
	// store/relay must recover from durable rows, with no reconciler-local state.
	restarted := NewPostgres(pool)
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "custody-run-restart"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "accounting", ProducerVersion: "test", Tenant: tenant})
	if err != nil {
		t.Fatal(err)
	}
	relay, err := outbox.NewRelay(restarted.Outbox(), producer, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if n, err := relay.DrainOnce(ctx); err != nil || n != 1 {
		t.Fatalf("restart delivery: n=%d err=%v", n, err)
	}
	var sequence uint64
	for delivery := 0; delivery < 2; delivery++ {
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
		if env.GetTenantId() != tenant || env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_FACT || env.GetCorrelationId() != run.RunID || fact.GetRunId() != run.RunID || fact.GetOutcome() != accountingpb.ReconciliationOutcome_RECONCILIATION_OUTCOME_BREAKS || len(fact.GetBreaks()) != 1 {
			t.Fatalf("wrong committed FACT: %v %v", env, &fact)
		}
		if difference, ok := dec.FromProtoChecked(fact.GetBreaks()[0].GetDifference()); !ok || difference.Cmp(big.NewRat(10, 1)) != 0 {
			t.Fatal("committed FACT changed the exact difference")
		}
		meta, err := msg.Metadata()
		if err != nil {
			t.Fatal(err)
		}
		if delivery == 0 {
			sequence = meta.Sequence.Stream
			if err := msg.Nak(); err != nil {
				t.Fatal(err)
			}
		} else {
			if meta.Sequence.Stream != sequence || meta.NumDelivered < 2 {
				t.Fatal("committed FACT was not redelivered by the broker")
			}
			if err := msg.DoubleAck(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	afterRestart, err := NewReconciler(restarted, book, producer, nil, nil, testLogger(), func() time.Time { return t0 })
	if err != nil {
		t.Fatal(err)
	}
	if repeated, err := afterRestart.Reconcile(ctx, subject()); err != nil || repeated.RunID != run.RunID {
		t.Fatalf("reconcile redelivery: %+v %v", repeated, err)
	}
	if _, err := restarted.CommitRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if n, err := relay.DrainOnce(ctx); err != nil || n != 0 {
		t.Fatalf("retry queued a duplicate completion: n=%d err=%v", n, err)
	}
	b, err := restarted.LoadBreak(ctx, run.Breaks[0].BreakID)
	if err != nil || b.Revision != 1 {
		t.Fatalf("restart/retry repeated lifecycle: %+v %v", b, err)
	}
	var total, published int
	if err := pool.QueryRow(ctx, `SELECT count(*),count(published_at) FROM outbox WHERE producer_service=$1 AND event_type=$2`, outbox.ServiceAccounting, SubjectRun).Scan(&total, &published); err != nil || total != 1 || published != 1 {
		t.Fatalf("delivery state: total=%d published=%d err=%v", total, published, err)
	}
}
