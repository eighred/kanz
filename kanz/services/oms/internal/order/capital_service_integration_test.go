package order

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/bustest"
	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/platform/halt"
	"github.com/eighred/kanz/internal/refdata"
	commandpb "github.com/eighred/kanz/kanz-schemas-go/command/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/oms/internal/capital"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
)

// The complete OMS handler uses a real PostgreSQL store and publishes its
// durable acceptance/fill/refusal records through a real JetStream stream.
func TestFundedOMSCompetingOrdersOverPostgresAndJetStream(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("set TEST_NATS_URL for real OMS/JetStream proof")
	}
	pool := newPool(t)
	ctx, cancel := context.WithTimeout(bus.WithTenantID(context.Background(), testTenant), 20*time.Second)
	defer cancel()
	now := time.Now().UTC()
	if err := capital.Apply(ctx, pool, capital.CashEvent{PortfolioID: "fund", Currency: "USD", Revision: 1, Total: "150", Complete: true, ObservedAt: now}); err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprint(now.UnixNano())
	admin, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	js, err := jetstream.New(admin)
	if err != nil {
		t.Fatal(err)
	}
	bustest.EnsureSubjects(t, ctx, js, "ORDER_CAPITAL_IT_"+suffix, []string{"order.>"})
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "oms-capital-it"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "oms", ProducerVersion: "it", Tenant: testTenant})
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := bus.NewConsumer(client)
	if err != nil {
		t.Fatal(err)
	}
	collector := &rejectionCollector{by: map[string]*orderpb.OrderRejected{}}
	watchCtx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	go func() {
		_ = consumer.Subscribe(watchCtx, EventTypeRejected, "oms-capital-it-"+suffix, collector.handle)
	}()
	cache, err := refdata.NewCache(capitalReferenceSource{refdata.Record{InstrumentID: "BTC-USD", AssetClass: "CRYPTO", BaseAsset: "BTC", QuoteAsset: "USD", AsOf: now}}, refdata.Options{})
	if err != nil {
		t.Fatal(err)
	}
	cache.Lookup("BTC-USD", now)
	if _, err := cache.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	router := execution.NewRouter([]execution.Venue{execution.NewSimVenue("XNAS")})
	store := NewPostgres(pool)
	svc, err := NewService(testTenant, store, NewEmitter(producer), nil, router, nil, nil,
		WithHaltGate(halt.OpenGate(nil)), WithCapitalTerms(SimPhysicalTerms{References: cache, Router: router}))
	if err != nil {
		t.Fatal(err)
	}
	env := &envelopepb.Envelope{EventType: SubjectSubmit, TenantId: testTenant}
	firstID, secondID := "first-"+suffix, "second-"+suffix
	for _, id := range []string{firstID, secondID} {
		cmd := &orderpb.SubmitOrder{OrderId: id, PortfolioId: "fund", InstrumentId: "BTC-USD", Side: orderpb.Side_SIDE_BUY,
			Quantity: d(1, 0), OrderType: orderpb.OrderType_ORDER_TYPE_LIMIT, LimitPrice: d(100, 0),
			TimeInForce: orderpb.TimeInForce_TIME_IN_FORCE_DAY,
			Metadata:    &commandpb.CommandMetadata{Issuer: "service:capital-test"}}
		payload, err := proto.Marshal(cmd)
		if err != nil {
			t.Fatal(err)
		}
		if err := svc.Handle(ctx, env, payload); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
	orders, err := store.ListByStatus(ctx, orderpb.OrderStatus_ORDER_STATUS_FILLED)
	if err != nil || len(orders) != 1 {
		t.Fatalf("filled orders=%d err=%v", len(orders), err)
	}
	var reserved string
	if err := pool.QueryRow(ctx, `SELECT reserved FROM capital_balances WHERE portfolio_id='fund' AND currency='USD'`).Scan(&reserved); err != nil || reserved != "100" {
		t.Fatalf("cash reserved=%s err=%v", reserved, err)
	}
	var published int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE event_type=$1`, EventTypeFilled).Scan(&published); err != nil {
		t.Fatal(err)
	}
	if published != 1 {
		t.Fatalf("broker-backed fill facts=%d", published)
	}
	for deadline := time.Now().Add(5 * time.Second); collector.get(secondID) == nil && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
	if rejected := collector.get(secondID); rejected == nil || rejected.GetErrorCode() != "CAPITAL_REFUSED" {
		t.Fatalf("broker-backed capital refusal=%v", rejected)
	}
}
