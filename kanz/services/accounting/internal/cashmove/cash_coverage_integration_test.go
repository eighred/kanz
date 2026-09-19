package cashmove_test

import (
	"context"
	"fmt"
	"math/big"
	"os"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/cashview"
	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/outbox"
	pb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/consume"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
)

// Real journal transaction -> outbox -> file-backed JetStream -> production
// cash view -> actual buying-power rule. A negative foreign bucket must revoke
// an otherwise passing floor; no fake publisher can prove this boundary.
func TestCashBalanceAnnouncePostgresJetStreamBuyingPower(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("requires real JetStream")
	}
	p := database(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	name := fmt.Sprintf("CASH_COVERAGE_%d", time.Now().UnixNano())
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = js.StreamNameBySubject(ctx, consume.SubjectPortfolioCash); err != nil {
		if _, err = js.CreateStream(ctx, jetstream.StreamConfig{Name: name, Subjects: []string{consume.SubjectPortfolioCash}, Storage: jetstream.FileStorage}); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = js.DeleteStream(context.Background(), name) }()
	}
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: name})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "accounting", ProducerVersion: "test", Tenant: "tenant-A"})
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := bus.NewConsumer(client)
	if err != nil {
		t.Fatal(err)
	}
	view := cashview.New()
	received := make(chan *pb.PortfolioCashBalance, 16)
	done := make(chan error, 1)
	go func() {
		done <- consumer.Subscribe(ctx, consume.SubjectPortfolioCash, name, func(ctx context.Context, env *envelopepb.Envelope, b []byte) error {
			var msg pb.PortfolioCashBalance
			if err := proto.Unmarshal(b, &msg); err != nil {
				return err
			}
			if msg.PortfolioId != name || env.TenantId != "tenant-A" {
				return nil
			}
			if err := view.Handle(ctx, env, b); err != nil {
				return err
			}
			select {
			case received <- &msg:
			case <-ctx.Done():
				return ctx.Err()
			}
			return nil
		})
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("consumer did not stop")
		}
	}()
	st := ledger.NewPostgres(p)
	ann := consume.NewAnnouncer(st, producer, "USD", consume.EntrySourcePosture{Produced: []string{"cash"}}, nil, nil)
	relay, err := outbox.NewRelay(st.Outbox(), producer, nil)
	if err != nil {
		t.Fatal(err)
	}
	rule := &compliancepb.Rule{Params: &compliancepb.Rule_BuyingPower{BuyingPower: &compliancepb.BuyingPowerLimit{MinCashAfter: &commonpb.Decimal{}}}}
	amounts := []*big.Rat{big.NewRat(1, 1000000000), big.NewRat(-50, 1), big.NewRat(50, 1)}
	currencies := []string{"USD", "EUR", "EUR"}
	for i, amount := range amounts {
		now := time.Now().UTC().Truncate(time.Microsecond)
		event := &ledger.Event{EntryID: fmt.Sprintf("coverage:%s:%d", name, i), PortfolioID: name, VenueAccountID: "venue", Type: ledger.EntryCash, Cash: amount, CashCurrency: currencies[i], Effective: now, Knowledge: now}
		if err = st.Append(ctx, event, func(ctx context.Context, reader ledger.Store) ([]outbox.Record, error) {
			return ann.Records(bus.WithTenantID(ctx, "tenant-A"), reader, name)
		}); err != nil {
			t.Fatal(err)
		}
		if _, err = relay.Flush(ctx, name); err != nil {
			t.Fatal(err)
		}
		var msg *pb.PortfolioCashBalance
		select {
		case msg = <-received:
		case <-ctx.Done():
			t.Fatal("balance not delivered")
		}
		if dec.FromProto(msg.Total).Cmp(amounts[0]) != 0 {
			t.Fatal("exact base cash changed on wire")
		}
		foundUSD := false
		for _, account := range msg.ByVenueAccount {
			if account.Asset == "USD" {
				foundUSD = account.VenueAccountId == "venue" && dec.FromProto(account.Amount).Cmp(amounts[0]) == 0
			}
		}
		if !foundUSD {
			t.Fatal("native account cash lost precision on wire")
		}
		book := &comp.Book{PortfolioID: name, BaseCurrency: "USD"}
		comp.JoinEquity(book, view, nil)
		violation := comp.BuyingPowerRule(&comp.Candidate{Book: book}, rule)
		if i == 1 {
			if violation == nil || msg.CurrencyCoverage.GetExcludedCount() != 1 || len(msg.ExcludedCurrencies) != 1 || msg.ExcludedCurrencies[0] != "EUR" {
				t.Fatalf("foreign liability admitted: %v %v", msg, violation)
			}
		} else if violation != nil {
			t.Fatalf("complete base bucket refused: %v", violation)
		}
	}
	// A nonrepresentable balance must roll back both the new entry and its
	// derived announcement, preserving the previous coherent committed state.
	now := time.Now().UTC().Truncate(time.Microsecond)
	bad := &ledger.Event{EntryID: "unsupported", PortfolioID: name, Type: ledger.EntryCash, Cash: big.NewRat(1, 3), CashCurrency: "USD", Effective: now, Knowledge: now}
	if err = st.Append(ctx, bad, func(ctx context.Context, reader ledger.Store) ([]outbox.Record, error) {
		return ann.Records(bus.WithTenantID(ctx, "tenant-A"), reader, name)
	}); err == nil {
		t.Fatal("unsupported total committed")
	}
	entries, err := st.Journal(ctx, name)
	if err != nil || len(entries) != 3 {
		t.Fatalf("rollback journal: %d %v", len(entries), err)
	}
}
