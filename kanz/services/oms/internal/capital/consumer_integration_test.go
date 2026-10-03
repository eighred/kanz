package capital

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/bustest"
	"github.com/eighred/kanz/internal/cashview"
	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
)

func TestCapitalDurableConsumerReplaysPrefixAndResumesWithFreshPool(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("requires real PostgreSQL and JetStream")
	}
	_ = database(t)
	suffix := fmt.Sprint(time.Now().UnixNano())
	tenant := "capital-" + suffix
	p := tenantPool(t, tenant)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	bustest.EnsureSubjects(t, ctx, js, "CAPITAL_"+suffix, []string{cashview.Subject})
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "capital-replay-test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	stream, err := js.StreamNameBySubject(ctx, cashview.Subject)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := js.DeleteConsumer(cleanup, stream, "capital-proof-"+suffix+"-accounting_balance_portfolio"); err != nil && !errors.Is(err, jetstream.ErrConsumerNotFound) {
			t.Errorf("delete test consumer: %v", err)
		}
	})
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "accounting", ProducerVersion: "test", Tenant: tenant})
	if err != nil {
		t.Fatal(err)
	}
	publish := func(rev int64) {
		t.Helper()
		msg := coverageBalance()
		msg.CashCommit.Applied = nil
		msg.CashCommit.Revision = rev
		if err := producer.Publish(ctx, bus.Event{Subject: cashview.Subject, EventType: cashview.Subject,
			EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, Domain: "accounting",
			EventTime: msg.AsOf.AsTime(), PartitionKey: msg.PortfolioId,
			PayloadSchemaRef: "accounting.v1.PortfolioCashBalance:1", Payload: msg}); err != nil {
			t.Fatal(err)
		}
	}
	type result struct {
		revision int64
		err      error
	}
	seen := make(chan result, 32)
	start := func(pool *pgxpool.Pool) func() {
		t.Helper()
		consumer, err := bus.NewConsumer(client)
		if err != nil {
			t.Fatal(err)
		}
		run, stop := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() {
			done <- consumer.Subscribe(run, cashview.Subject, "capital-proof-"+suffix,
				func(delivery context.Context, env *envelopepb.Envelope, payload []byte) error {
					// Shared CI brokers contain other tests' tenants; production uses
					// tenant NATS accounts. This filter is only test isolation.
					if env.GetTenantId() != tenant {
						return nil
					}
					err := ApplyEnvelope(delivery, pool, tenant, env, payload)
					var msg accountingpb.PortfolioCashBalance
					_ = proto.Unmarshal(payload, &msg)
					select {
					case seen <- result{msg.GetCashCommit().GetRevision(), err}:
					case <-delivery.Done():
					}
					return err
				})
		}()
		var once sync.Once
		join := func() {
			once.Do(func() {
				stop()
				if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
					t.Errorf("consumer shutdown: %v", err)
				}
			})
		}
		t.Cleanup(join)
		return join
	}
	wait := func(target int64) {
		t.Helper()
		for {
			select {
			case got := <-seen:
				if got.err != nil {
					t.Fatalf("revision %d: %v", got.revision, got.err)
				}
				if got.revision == target {
					return
				}
			case <-ctx.Done():
				t.Fatalf("waiting for revision %d: %v", target, ctx.Err())
			}
		}
	}
	for _, rev := range []int64{1, 2, 3} {
		publish(rev) // before subscription: a snapshot consumer would miss 1 and 2
	}
	stop := start(p)
	wait(3)
	stop()
	publish(4) // source keeps moving while the OMS consumer is down
	restarted := tenantPool(t, tenant)
	stop = start(restarted)
	defer stop()
	wait(4)
	var revision, receipts int
	if err := restarted.QueryRow(ctx, "SELECT revision FROM capital_balances").Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if err := restarted.QueryRow(ctx, "SELECT count(*) FROM capital_cash_events").Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if revision != 4 || receipts != 4 {
		t.Fatalf("lost prefix or restart: revision=%d receipts=%d", revision, receipts)
	}
}
