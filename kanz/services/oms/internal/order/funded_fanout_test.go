package order

import (
	"bytes"
	"context"
	"errors"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/bustest"
	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/platform/halt"
	"github.com/eighred/kanz/internal/refdata"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/oms/internal/capital"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	nserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Run the webhook package in another process so the original signed alert,
// translation, real broker and fill reader stay independent of the funded OMS.
func TestFundedWebhookFanout(t *testing.T) {
	probe := os.Getenv("TEST_WEBHOOK_FANOUT_PROBE")
	if probe == "" {
		t.Skip("requires the separately built webhook integration test binary")
	}
	const tenant = "acme"
	ctx, cancel := context.WithTimeout(bus.WithTenantID(t.Context(), tenant), 40*time.Second)
	defer cancel()
	config := newPool(t).Config()
	scoped := config.AfterConnect
	config.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		if err := scoped(ctx, c); err != nil {
			return err
		}
		_, err := c.Exec(ctx, "SELECT set_config('app.tenant_id', $1, false)", tenant)
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	now := time.Now().UTC()
	if err := capital.Apply(ctx, pool, capital.CashEvent{PortfolioID: "fund-alpha", Currency: "USD", Revision: 1, Total: "50000000000050000", Complete: true, ObservedAt: now}); err != nil {
		t.Fatal(err)
	}
	options, err := nserver.ProcessConfigFile(filepath.Join("..", "..", "..", "..", "test", "backing", "nats-dev.conf"))
	if err != nil {
		t.Fatal(err)
	}
	options.Host, options.Port, options.StoreDir = "127.0.0.1", -1, t.TempDir()
	broker, err := nserver.NewServer(options)
	if err != nil {
		t.Fatal(err)
	}
	broker.Start()
	defer func() { broker.Shutdown(); broker.WaitForShutdown() }()
	if !broker.ReadyForConnections(5 * time.Second) {
		t.Fatal("private JetStream did not start")
	}
	nc, err := nats.Connect(broker.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	bustest.EnsureSubjects(t, ctx, js, "FUNDED_FANOUT", []string{"strategy.>", "order.>", "risk.>"})
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: broker.ClientURL(), Name: "funded-fanout-oms"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	consumer, err := bus.NewConsumer(client)
	if err != nil {
		t.Fatal(err)
	}
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "oms", ProducerVersion: "fixture", Tenant: tenant})
	if err != nil {
		t.Fatal(err)
	}
	cache, err := refdata.NewCache(capitalReferenceSource{refdata.Record{InstrumentID: "BTC-USD", AssetClass: "CRYPTO", BaseAsset: "BTC", QuoteAsset: "USD", AsOf: now}}, refdata.Options{})
	if err != nil {
		t.Fatal(err)
	}
	cache.Lookup("BTC-USD", now)
	if _, err := cache.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	router := execution.NewRouter([]execution.Venue{execution.NewSimVenue("XNAS"), execution.NewSimVenue("XLON")})
	store := NewPostgres(pool)
	svc, err := NewService(tenant, store, NewEmitter(producer), nil, router, nil, nil, WithHaltGate(halt.OpenGate(nil)), WithCapitalTerms(SimPhysicalTerms{References: cache, Router: router}))
	if err != nil {
		t.Fatal(err)
	}
	listen, stop := context.WithCancel(ctx)
	ready, done := make(chan struct{}), make(chan error, 1)
	go func() {
		done <- consumer.SubscribeReady(listen, SubjectSubmit, "funded-webhook-oms", svc.Handle, func() { close(ready) })
	}()
	defer stop()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("submit subscription ended: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	defer func() {
		stop()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("submit subscription shutdown: %v", err)
		}
	}()
	cmd := exec.CommandContext(ctx, probe, "-test.run=^TestIntegration_LoopOverNATS$", "-test.count=1", "-test.v")
	cmd.Env = []string{"TEST_FUNDED_WEBHOOK_NATS_URL=" + broker.ClientURL()}
	for _, key := range []string{"SystemRoot", "TEMP", "TMP", "TMPDIR"} {
		if val, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+val)
		}
	}
	output, err := cmd.CombinedOutput()
	if err != nil || !bytes.Contains(output, []byte("--- PASS: TestIntegration_LoopOverNATS")) {
		t.Fatalf("funded signed webhook: %v\n%s", err, output)
	}
	filled, err := store.ListByStatus(ctx, orderpb.OrderStatus_ORDER_STATUS_FILLED)
	if err != nil || len(filled) != 3 {
		t.Fatalf("stored filled orders=%d, error=%v", len(filled), err)
	}
	var large bool
	for _, order := range filled {
		if order.GetOrderId() == "funded-large" {
			large = dec.FromProto(order.GetFilledQuantity()).Cmp(big.NewRat(1_000_000_000_000, 1)) == 0
		}
	}
	if !large {
		t.Fatal("stored large fill lost magnitude")
	}
	var invalid int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM orders WHERE order_id='funded-invalid-exponent'").Scan(&invalid); err != nil || invalid != 0 {
		t.Fatalf("out-of-domain order persisted: %d (%v)", invalid, err)
	}
	var reserved string
	if err := pool.QueryRow(ctx, "SELECT reserved FROM capital_balances WHERE portfolio_id='fund-alpha' AND currency='USD'").Scan(&reserved); err != nil || reserved != "50000000000050000" {
		t.Fatalf("funded reservation=%s, error=%v", reserved, err)
	}
}
