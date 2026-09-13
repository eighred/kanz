package custody

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/bustest"
	"github.com/eighred/kanz/internal/outbox"
	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/recon"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
)

func TestCustodyActionDurableFACTReachesRealSpine(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("set TEST_NATS_URL for real custody FACT delivery")
	}
	tenant := fmt.Sprintf("custody-action-%d", time.Now().UnixNano())
	pool := newCustodyPool(t, tenant)
	applyCustodySchema(t, pool)
	st := NewPostgres(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
	bustest.EnsureSubjects(t, ctx, js, fmt.Sprintf("CUSTODY_ACTION_%d", time.Now().UnixNano()), []string{SubjectActionRecorded})
	sub, err := nc.SubscribeSync(SubjectActionRecorded)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertBreaks(ctx, subject(), detectedQuantityBreak(t0, 100, 90), t0); err != nil {
		t.Fatal(err)
	}
	id := BreakID("PF1", "CUST-A", recon.BreakQuantity, "AAPL")
	e, err := st.ApplyAction(ctx, Action{Tenant: tenant, Actor: "alice", RequestID: "spine-claim", BreakID: id, Kind: "claim", ExpectedRevision: 1})
	if err != nil {
		t.Fatal(err)
	}
	records, err := outbox.NewPostgres(pool, "accounting").Pending(ctx, id, 10)
	if err != nil || len(records) != 1 {
		t.Fatalf("outbox: %+v %v", records, err)
	}
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "custody-action-test"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "accounting", ProducerVersion: "test", Tenant: tenant})
	if err != nil {
		t.Fatal(err)
	}
	event, err := records[0].Record.Event()
	if err != nil {
		t.Fatal(err)
	}
	if err := producer.Publish(ctx, event); err != nil {
		t.Fatal(err)
	}
	msg, err := sub.NextMsgWithContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	env, payload, err := bus.Unframe(msg.Data)
	if err != nil {
		t.Fatal(err)
	}
	var fact accountingpb.CustodyActionRecorded
	if err := proto.Unmarshal(payload, &fact); err != nil {
		t.Fatal(err)
	}
	if env.GetTenantId() != tenant || env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_FACT || env.GetCorrelationId() != e.RequestID || fact.GetActor() != e.Actor || fact.GetBreakId() != id || fact.GetBefore().GetRevision() != 1 || fact.GetAfter().GetRevision() != 2 {
		t.Fatalf("wrong wire evidence: %v %v", env, &fact)
	}
}
