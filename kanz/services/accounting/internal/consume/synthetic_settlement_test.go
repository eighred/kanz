package consume_test

import (
	"bytes"
	"context"
	"math/big"
	"net/url"
	"os"
	"slices"
	"strconv"
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
	switch tenant {
	case "synthetic-funded-venue", "synthetic-funded-venue-nanos", "synthetic-funded-venue-pair",
		"synthetic-funded-cancel-fill", "synthetic-go-funded-working", "synthetic-cash-clock-mismatch", "synthetic-cash-clock":
	default:
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
		return time.Now().UTC().Truncate(time.Microsecond).Add(321 * time.Nanosecond)
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
	subject, durable := os.Getenv("SYNTHETIC_BOOK_SUBJECT"), "synthetic-accounting-fill"
	switch subject {
	case "", fillfact.SubjectPartiallyFilled:
		subject = fillfact.SubjectPartiallyFilled
	case fillfact.SubjectFilled:
		if tenant != "synthetic-go-funded-working" || os.Getenv("SYNTHETIC_BOOK_SEQUENCE") != "2" {
			t.Fatal("terminal fill requires working handback and its second execution")
		}
		durable = "synthetic-accounting-remainder"
	default:
		t.Fatal("unexpected accounting fixture subject")
	}
	consumer, err := js.CreateOrUpdateConsumer(ctx, "SYNTHETIC_FACTS", jetstream.ConsumerConfig{Durable: durable, FilterSubject: subject, AckPolicy: jetstream.AckExplicitPolicy})
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
	if env.GetSource() != "oms" || env.GetTenantId() != tenant || env.GetEventType() != subject ||
		env.GetPartitionKey() != orderID || env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_FACT || msg.Subject() != env.GetEventType() {
		t.Fatal("unexpected OMS fill delivery")
	}
	info, err := msg.Metadata()
	if err != nil || mode == "book" && info.NumDelivered != 1 || mode == "replay" && info.NumDelivered != 2 {
		t.Fatal("unexpected fill redelivery", err)
	}
	var fact interface {
		proto.Message
		GetState() *orderpb.OrderState
		GetOrderId() string
		GetFill() *orderpb.Fill
	} = &orderpb.OrderPartiallyFilled{}
	if subject == fillfact.SubjectFilled {
		fact = &orderpb.OrderFilled{}
	}
	if proto.Unmarshal(frame.Payload, fact) != nil || fact.GetOrderId() != orderID || fact.GetState().GetPortfolioId() != portfolio || fact.GetFill().GetVenue() != "XSIM" {
		t.Fatal("fill has wrong scope")
	}
	ctx = bus.WithTraceContext(bus.WithCausationID(bus.WithCorrelationID(ctx, env.CorrelationId), env.EventId), env.TraceContext)
	if err := folder.Handle(ctx, env, frame.Payload); err != nil {
		t.Fatal(err)
	}
	sequence, err := strconv.Atoi(os.Getenv("SYNTHETIC_BOOK_SEQUENCE"))
	if err != nil || sequence < 1 || sequence > 2 || (sequence == 2 && tenant != "synthetic-funded-venue-pair" && subject != fillfact.SubjectFilled) {
		t.Fatal("invalid accounting fixture trade sequence")
	}
	entries, err := store.Journal(ctx, portfolio)
	if err != nil || len(entries) != 2+sequence {
		t.Fatal("unexpected number of opening and trade entries", len(entries), err)
	}
	wire, err := proto.MarshalOptions{Deterministic: true}.Marshal(fact.GetFill())
	if err != nil {
		t.Fatal(err)
	}
	var tradeTimes []time.Time
	seenCurrent := false
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
		var source orderpb.Fill
		if err := proto.Unmarshal(entry.ExecutionEvidence, &source); err != nil || source.GetExecutedAt().CheckValid() != nil || !entry.Effective.Equal(source.ExecutedAt.AsTime().Truncate(time.Microsecond)) {
			t.Fatal("journal lost the original execution time", err)
		}
		tradeTimes = append(tradeTimes, source.ExecutedAt.AsTime())
		if subject == fillfact.SubjectFilled && source.OrderId != orderID {
			t.Fatal("both trades must belong to the transferred order")
		}
		if source.OrderId == orderID && (subject != fillfact.SubjectFilled || source.FillId == fact.GetFill().GetFillId()) {
			seenCurrent = true
			if !bytes.Equal(entry.ExecutionEvidence, wire) {
				t.Fatal("stored execution differs from its signed-order fill FACT")
			}
		}
		if entry.EntryID != "fill:"+fillfact.ExecutionKey(&source) ||
			entry.VenueAccountID != "synthetic-spot" || entry.CashCurrency != "USD" || entry.Cash.Cmp(big.NewRat(-126, 1)) != 0 ||
			entry.Quantity.Cmp(big.NewRat(5, 8)) != 0 || entry.Price.Cmp(big.NewRat(200, 1)) != 0 {
			t.Fatal("journal differs from the XSIM execution economics")
		}
	}
	if !seenCurrent || len(tradeTimes) != sequence || len(cash) != 2 || cash["USD"] == nil || cash["USD"].Cmp(big.NewRat(int64(1000-126*sequence), 1)) != 0 || cash["BNB"] == nil || cash["BNB"].Cmp(big.NewRat(8, 1)) != 0 {
		t.Fatal("unexpected XSIM trade count or journal cash")
	}
	if sequence == 2 {
		slices.SortFunc(tradeTimes, time.Time.Compare)
		first, second := tradeTimes[0], tradeTimes[1]
		if !first.Truncate(time.Microsecond).Equal(second.Truncate(time.Microsecond)) || first.Nanosecond()%1000 != 321 || second.Nanosecond()%1000 != 789 {
			t.Fatal("journal did not preserve both distinct same-microsecond execution times")
		}
		for i, at := range []time.Time{first.Add(-time.Nanosecond), first, second.Add(-time.Nanosecond), second} {
			p, err := ledger.MaterializeCash(ctx, store, portfolio, at)
			if err != nil {
				t.Fatal(err)
			}
			expected := big.NewRat([]int64{1000, 874, 874, 748}[i], 1)
			balance := p.Accounts["synthetic-spot"]["USD"]
			if balance == nil || balance.Cmp(expected) != 0 || p.Book.CashBalance("USD").Cmp(expected) != 0 {
				t.Fatalf("cash at remainder %dns: account=%v portfolio=%s want=%s", at.Nanosecond()%1000, balance, p.Book.CashBalance("USD"), expected)
			}
		}
	}
	var historyCount int
	want := 2 * sequence
	if mode == "replay" {
		want++
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM cash_commit_history WHERE portfolio_id=$1 AND currency='USD'`, portfolio).Scan(&historyCount); err != nil || historyCount != want {
		t.Fatal("unexpected cash history length", historyCount, err)
	}
	// After redelivery, check each original publication against immutable history.
	cashStream, err := js.Stream(ctx, "SYNTHETIC_CASH")
	if err != nil {
		t.Fatal(err)
	}
	for revision := 1; revision <= want; revision++ {
		raw, err := cashStream.GetMsg(ctx, uint64(revision))
		if err != nil {
			t.Fatal(err)
		}
		var published envelopepb.EventFrame
		var retained []byte
		if proto.Unmarshal(raw.Data, &published) != nil || published.GetEnvelope().GetTenantId() != tenant {
			t.Fatal("unexpected cash history publication")
		}
		if err := pool.QueryRow(ctx, "SELECT payload FROM cash_commit_history WHERE portfolio_id=$1 AND currency='USD' AND revision=$2", portfolio, revision).Scan(&retained); err != nil || !bytes.Equal(retained, published.Payload) {
			t.Fatal("retained cash history differs from original JetStream publication", revision, err)
		}
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
