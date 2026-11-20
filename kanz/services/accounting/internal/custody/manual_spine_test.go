package custody

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/bustest"
	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
)

func TestManualCompletionRetainsOperatorOnJetStream(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("TEST_NATS_URL required for broker acceptance")
	}
	key := uuid.NewString()
	tenant := "manual-" + key
	pool := newCustodyPool(t, tenant)
	requireRestrictedCustodyRole(t, pool)
	applyCustodySchema(t, pool)
	ctx, cancel := context.WithTimeout(bus.WithTenantID(t.Context(), tenant), 15*time.Second)
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
	bustest.EnsureSubjects(t, ctx, js, "MANUAL_"+key, []string{SubjectRun})
	stream, err := bustest.StreamFor(ctx, js, SubjectRun)
	if err != nil {
		t.Fatal(err)
	}
	out, err := js.CreateConsumer(ctx, stream, jetstream.ConsumerConfig{Name: tenant, FilterSubject: SubjectRun, DeliverPolicy: jetstream.DeliverNewPolicy, AckPolicy: jetstream.AckExplicitPolicy})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = js.DeleteConsumer(context.Background(), stream, tenant) })
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: tenant})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "accounting", ProducerVersion: "manual-fixture", Tenant: tenant})
	if err != nil {
		t.Fatal(err)
	}
	store := NewPostgres(pool)
	sub := subject()
	sub.BusinessDate = BusinessDay(t0)
	statementID := manualSignedFeed(t, ctx, store)(0, 0)
	r := manualReconciler(t, store, LedgerBookLoader(ledger.NewPostgres(pool), closeScope(t, true), testLogger()), producer)
	got, err := r.ReconcileManual(ctx, sub, key, "operator")
	if err != nil {
		t.Fatal(err)
	}
	for ctx.Err() == nil {
		msg, err := out.Next(jetstream.FetchMaxWait(5 * time.Second))
		if err != nil {
			t.Fatal(err)
		}
		env, payload, err := bus.Unframe(msg.Data())
		if err != nil {
			t.Fatal(err)
		}
		if err := msg.DoubleAck(ctx); err != nil {
			t.Fatal(err)
		}
		if env.GetTenantId() != tenant {
			continue
		}
		var fact accountingpb.ReconciliationRun
		if proto.Unmarshal(payload, &fact) != nil || env.GetCausationId() != key || env.GetCorrelationId() != got.RunID ||
			env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_FACT || env.GetEventType() != SubjectRun ||
			fact.GetRequestId() != key || fact.GetActor() != "operator" || fact.GetRunId() != got.RunID ||
			fact.GetStatementId() != statementID || fact.GetBusinessDate().AsTime() != sub.BusinessDate ||
			fact.GetOutcome() != accountingpb.ReconciliationOutcome_RECONCILIATION_OUTCOME_CLEAN {
			t.Fatal("published completion lost verified operator, dated statement or run attribution")
		}
		return
	}
	t.Fatal(ctx.Err())
}
