package consume_test

import (
	"bytes"
	"context"
	"math/big"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/fillfact"
	"github.com/eighred/kanz/internal/outbox"
	"github.com/eighred/kanz/internal/pg"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/consume"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
)

// The signed gateway fixture runs this accounting process twice for a fill.
// Its login can reach only the separate accounting database.
func TestSyntheticSettlementJournal(t *testing.T) {
	mode := os.Getenv("SYNTHETIC_BOOK_STEP")
	if mode == "" {
		t.Skip("requires the isolated funded OMS and accounting estate")
	}
	const portfolio = "synthetic-portfolio"
	tenant := os.Getenv("SYNTHETIC_BOOK_TENANT")
	if tenant != "synthetic-funded-venue" && tenant != "synthetic-funded-venue-nanos" && tenant != "synthetic-cash-clock" {
		t.Fatal("expected a funded XSIM fixture tenant")
	}
	dsn, broker := os.Getenv("SYNTHETIC_BOOK_URL"), os.Getenv("SYNTHETIC_BOOK_NATS")
	db, err := url.Parse(dsn)
	if err != nil || db.Scheme != "postgres" || db.Host != "127.0.0.1:5432" || db.Path != "/kanz_synthetic_accounting" ||
		db.User.Username() != "kanz_synthetic_accounting" || db.RawQuery != "sslmode=disable" {
		t.Fatal("expected the disposable accounting database")
	}
	endpoint, err := url.Parse(broker)
	if err != nil || endpoint.Scheme != "nats" || endpoint.Hostname() != "127.0.0.1" || endpoint.Port() == "" || endpoint.User != nil {
		t.Fatal("expected the private JetStream server")
	}
	ctx := bus.WithTenantID(t.Context(), tenant)
	pool, err := pg.NewTenantPool(ctx, dsn, tenant)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var restricted bool
	if err := pool.QueryRow(ctx, `SELECT NOT (rolsuper OR rolbypassrls OR rolcreaterole OR rolcreatedb)
		AND current_database()='kanz_synthetic_accounting' AND c.relforcerowsecurity AND c.relrowsecurity AND c.relowner<>r.oid
		FROM pg_roles r,pg_class c WHERE r.rolname=current_user AND c.oid='ledger_entries'::regclass`).Scan(&restricted); err != nil || !restricted {
		t.Fatal("accounting requires its isolated restricted RLS identity", err)
	}
	client, err := bus.DialNATS(ctx, bus.NATSConfig{Name: "accounting", URL: broker})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "accounting", ProducerVersion: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	store := ledger.NewPostgres(pool)
	// This estate supplies only opening cash and the XSIM trade. The normal
	// announcer also records the excluded BNB bucket on the USD level.
	announcer := consume.NewAnnouncer(store, producer, "USD", consume.EntrySourcePosture{Produced: []string{"cash", "trade"}}, nil, func() time.Time {
		// The outbox persists event_time as PostgreSQL timestamptz. Sample a
		// representable fixture cutoff before building the cash payload.
		at := time.Now().UTC().Truncate(time.Microsecond)
		if tenant == "synthetic-cash-clock" {
			return at.Add(321 * time.Nanosecond)
		}
		return at
	})
	folder, err := consume.NewFolder(tenant, store, "USD", consume.WithAnnouncer(announcer))
	if err != nil {
		t.Fatal(err)
	}
	if mode == "open" {
		now := time.Now().UTC().Truncate(time.Microsecond)
		for _, leg := range []struct {
			currency string
			units    int64
		}{{"BNB", 8}, {"USD", 1000}} {
			entry := &ledger.Event{EntryID: "cash:synthetic-opening-" + leg.currency, Type: ledger.EntryCash,
				PortfolioID: portfolio, VenueAccountID: "synthetic-spot", CashCurrency: leg.currency, Cash: big.NewRat(leg.units, 1),
				Effective: now, Knowledge: now, SettlementBasis: ledger.SettlementSettled, SourceRef: "synthetic-opening-" + leg.currency}
			var announce ledger.Announcer
			if leg.currency == "USD" {
				announce = func(ctx context.Context, view ledger.Store) ([]outbox.Record, error) {
					return announcer.Records(ctx, view, portfolio)
				}
			}
			if err := store.Append(ctx, entry, announce); err != nil {
				t.Fatal(err)
			}
		}
		if count, err := folder.Outbox().Flush(ctx, portfolio); err != nil || count != 1 {
			t.Fatal("opening balance publication", count, err)
		}
		return
	}
	if mode != "book" && mode != "replay" {
		t.Fatal("unknown synthetic accounting step")
	}
	nc, err := nats.Connect(broker, nats.NoReconnect())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := js.CreateOrUpdateConsumer(ctx, "SYNTHETIC_FACTS", jetstream.ConsumerConfig{Durable: "synthetic-accounting-fill", FilterSubject: fillfact.SubjectPartiallyFilled, AckPolicy: jetstream.AckExplicitPolicy})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := consumer.Next(jetstream.FetchMaxWait(3 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	orderID := os.Getenv("SYNTHETIC_BOOK_ORDER")
	var frame envelopepb.EventFrame
	if err := proto.Unmarshal(msg.Data(), &frame); err != nil {
		t.Fatal(err)
	}
	env := frame.GetEnvelope()
	if err := bus.Validate(env); err != nil {
		t.Fatal(err)
	}
	if env.GetSource() != "oms" || env.GetTenantId() != tenant || env.GetEventType() != fillfact.SubjectPartiallyFilled ||
		env.GetPartitionKey() != orderID || env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_FACT || msg.Subject() != env.GetEventType() {
		t.Fatal("unexpected OMS fill delivery")
	}
	info, err := msg.Metadata()
	if err != nil || mode == "book" && info.NumDelivered != 1 || mode == "replay" && info.NumDelivered != 2 {
		t.Fatal("unexpected fill redelivery", err)
	}
	var partial orderpb.OrderPartiallyFilled
	if proto.Unmarshal(frame.Payload, &partial) != nil || partial.GetOrderId() != orderID || partial.GetState().GetPortfolioId() != portfolio || partial.GetFill().GetVenue() != "XSIM" {
		t.Fatal("fill has wrong scope")
	}
	ctx = bus.WithTraceContext(bus.WithCausationID(bus.WithCorrelationID(ctx, env.CorrelationId), env.EventId), env.TraceContext)
	if err := folder.Handle(ctx, env, frame.Payload); err != nil {
		t.Fatal(err)
	}
	entries, err := store.Journal(ctx, portfolio)
	if err != nil || len(entries) != 3 {
		t.Fatal("expected two opening entries and one trade", len(entries), err)
	}
	wire, err := proto.MarshalOptions{Deterministic: true}.Marshal(partial.Fill)
	if err != nil {
		t.Fatal(err)
	}
	trades := 0
	cash := map[string]*big.Rat{}
	for _, entry := range entries {
		if entry.Cash == nil || entry.PortfolioID != portfolio || entry.VenueAccountID != "synthetic-spot" {
			t.Fatal("journal entry has incomplete cash or account attribution")
		}
		if cash[entry.CashCurrency] == nil {
			cash[entry.CashCurrency] = new(big.Rat)
		}
		cash[entry.CashCurrency].Add(cash[entry.CashCurrency], entry.Cash)
		if entry.Type != ledger.EntryTrade {
			continue
		}
		trades++
		if entry.EntryID != "fill:"+fillfact.ExecutionKey(partial.Fill) || !bytes.Equal(entry.ExecutionEvidence, wire) ||
			entry.VenueAccountID != "synthetic-spot" || entry.CashCurrency != "USD" || entry.Cash.Cmp(big.NewRat(-126, 1)) != 0 ||
			entry.Quantity.Cmp(big.NewRat(5, 8)) != 0 || entry.Price.Cmp(big.NewRat(200, 1)) != 0 {
			t.Fatal("journal differs from the XSIM execution economics")
		}
	}
	if trades != 1 || len(cash) != 2 || cash["USD"] == nil || cash["USD"].Cmp(big.NewRat(874, 1)) != 0 || cash["BNB"] == nil || cash["BNB"].Cmp(big.NewRat(8, 1)) != 0 {
		t.Fatal("expected one XSIM trade and journal balances of 874 USD and 8 BNB")
	}
	var historyCount int
	want := 2
	if mode == "replay" {
		want = 3
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM cash_commit_history WHERE portfolio_id=$1 AND currency='USD'`, portfolio).Scan(&historyCount); err != nil || historyCount != want {
		t.Fatal("unexpected cash history length", historyCount, err)
	}
	if mode == "book" {
		if err := msg.Nak(); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := msg.DoubleAck(ctx); err != nil {
			t.Fatal(err)
		}
		state, err := consumer.Info(ctx)
		if err != nil || state.NumPending != 0 || state.NumAckPending != 0 || state.AckFloor.Stream != info.Sequence.Stream {
			t.Fatal("fill remains outstanding after accounting replay", err)
		}
	}
}
