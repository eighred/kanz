package order

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/bustest"
	"github.com/eighred/kanz/internal/cashview"
	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/oms/internal/capital"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Real PostgreSQL, JetStream, and TLS HTTP exercise the production handler.
// The HTTP peer supplies protocol evidence; gateway authorization has separate
// gateway/accounting tests. This is not a live deployment identity attestation.
func TestCapitalCashHandlerRecoversBrokerRetentionGap(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("requires real JetStream and PostgreSQL")
	}
	pool := newPool(t)
	ctx, cancel := context.WithTimeout(bus.WithTenantID(context.Background(), testTenant), 20*time.Second)
	t.Cleanup(cancel)
	suffix := fmt.Sprint(time.Now().UnixNano())
	portfolio := "cash-prefix-" + suffix
	page := cashview.CommitPage{TenantID: testTenant, PortfolioID: portfolio, Currency: "USD", Through: 3, Next: 3}
	var latest *accountingpb.PortfolioCashBalance
	for revision := int64(1); revision <= 3; revision++ {
		latest = &accountingpb.PortfolioCashBalance{PortfolioId: portfolio, BaseCurrency: "USD", Total: d(100, 0),
			AsOf: timestamppb.New(t0), KnowledgeTime: timestamppb.New(t0), CurrencyCoverage: &domainpb.InputCoverage{Contributed: 1},
			Completeness: &accountingpb.BalanceCompleteness{ProducedEntryTypes: []string{"trade", "cash", "fee", "corporate_action", "accrual"}},
			CashCommit:   &accountingpb.CashCommitCoverage{Revision: revision, Complete: true}}
		payload, err := proto.Marshal(latest)
		if err != nil {
			t.Fatal(err)
		}
		page.Records = append(page.Records, cashview.CommitRecord{Revision: revision, Payload: payload})
	}
	history := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-only" || r.URL.Query().Get("through") != "3" || r.URL.Query().Get("after") != "0" || r.URL.Path != "/v1/portfolios/"+portfolio+"/cash-commits" {
			t.Error("handler requested a different history identity or watermark")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(page)
	}))
	t.Cleanup(history.Close)
	reader, err := capital.NewHistoryClient(history.URL, history.Client(), func(context.Context) (string, error) { return "test-only", nil })
	if err != nil {
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
	bustest.EnsureSubjects(t, ctx, js, "CASH_HISTORY_"+suffix, []string{cashview.Subject})
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "oms-cash-history-test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "accounting", ProducerVersion: "test", Tenant: testTenant})
	if err != nil {
		t.Fatal(err)
	}
	if err := producer.Publish(ctx, bus.Event{Subject: cashview.Subject, EventType: cashview.Subject, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT,
		Domain: "accounting", SchemaVersion: 1, PayloadSchemaRef: "accounting.v1.PortfolioCashBalance:1", PartitionKey: portfolio, EventTime: t0, Payload: latest}); err != nil {
		t.Fatal(err)
	}
	stream, err := js.StreamNameBySubject(ctx, cashview.Subject)
	if err != nil {
		t.Fatal(err)
	}
	group := "cash-history-" + suffix
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := js.DeleteConsumer(cleanup, stream, group+"-accounting_balance_portfolio"); err != nil && !errors.Is(err, jetstream.ErrConsumerNotFound) {
			t.Error(err)
		}
	})
	consumer, err := bus.NewConsumer(client)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewPostgres(pool).CapitalCashHandler(testTenant, reader)
	run, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	result := make(chan error, 1)
	go func() {
		done <- consumer.Subscribe(run, cashview.Subject, group, func(delivery context.Context, env *envelopepb.Envelope, payload []byte) error {
			if env.GetTenantId() != testTenant || env.GetPartitionKey() != portfolio {
				return nil
			} // shared CI broker isolation
			err := handler(delivery, env, payload)
			select {
			case result <- err:
			case <-delivery.Done():
			}
			return err
		})
	}()
	t.Cleanup(func() {
		stop()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	})
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("cash consumer failed to recover retained prefix")
	}
	var revision, receipts int
	if err := pool.QueryRow(ctx, `SELECT revision FROM capital_balances WHERE portfolio_id=$1`, portfolio).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM capital_cash_events WHERE portfolio_id=$1`, portfolio).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if revision != 3 || receipts != 3 {
		t.Fatalf("retained prefix incomplete: revision=%d receipts=%d", revision, receipts)
	}
}
