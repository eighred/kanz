package marketdata

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/bustest"
	"github.com/eighred/kanz/internal/marketdata/store"
	"github.com/eighred/kanz/internal/marketedge/coverage"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"
	"github.com/eighred/kanz/pkg/bus"
)

// Exercise framing, validation, JetStream delivery, DLQ publication and the real
// SQL writer together. Replay the same frames through a new consumer to prove
// the durable history remains correct across restart, without dedup masking it.
func TestPayloadBoundaryJetStreamPostgres(t *testing.T) {
	dsn, url := os.Getenv("TEST_POSTGRES_URL"), os.Getenv("TEST_NATS_URL")
	if dsn == "" || url == "" {
		t.Skip("requires TEST_POSTGRES_URL and TEST_NATS_URL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	pool := boundaryPool(t, ctx, dsn)
	st := store.NewPostgres(pool)
	ing, err := NewIngestor(st)
	if err != nil {
		t.Fatal(err)
	}
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprintf("boundary-%d", time.Now().UnixNano())
	bustest.EnsureSubjects(t, ctx, js, "BOUNDARY_MARKET", []string{"market.>"})
	bustest.EnsureSubjects(t, ctx, js, "BOUNDARY_DLQ", []string{"dlq.>"})
	marketStream, err := js.StreamNameBySubject(ctx, "market.crypto.trade")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stream, err := js.Stream(cleanupCtx, marketStream)
		if err != nil {
			t.Error(err)
			return
		}
		names := stream.ConsumerNames(cleanupCtx)
		for name := range names.Name() {
			if strings.HasPrefix(name, id+"-") {
				if err := js.DeleteConsumer(cleanupCtx, marketStream, name); err != nil {
					t.Error(err)
				}
			}
		}
		if err := names.Err(); err != nil {
			t.Error(err)
		}
	}()
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: id})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "boundary-test", ProducerVersion: "test", Tenant: "__system__"})
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	trade := &marketpb.MarketDataEvent{InstrumentId: id, EventTime: timestamppb.New(now), Data: &marketpb.MarketDataEvent_Trade{Trade: &marketpb.Trade{Price: decv(100, 0)}}}
	quote := &marketpb.MarketDataEvent{InstrumentId: id, EventTime: timestamppb.New(now), Data: &marketpb.MarketDataEvent_Quote{Quote: &marketpb.Quote{BidPrice: decv(100, 0), AskPrice: decv(102, 0)}}}
	bar := barEvent("XBIN", now.Add(-time.Minute), now)
	bar.InstrumentId = id
	book := &marketpb.OrderBookSnapshot{InstrumentId: id, EventTime: timestamppb.New(now), Bids: []*marketpb.PriceLevel{{Price: decv(90, 0)}}}
	twoSided := proto.Clone(book).(*marketpb.OrderBookSnapshot)
	twoSided.Asks = []*marketpb.PriceLevel{{Price: decv(110, 0)}}
	cov := covMsg()
	cov.InstrumentId = id
	cases := []struct {
		typ     string
		payload proto.Message
	}{
		{"market.book.snapshot", book},
		{"market.book.snapshot", twoSided},
		{"market.crypto.volume_profile", trade},
		{"market.crypto.unknown", trade},
		{"market.crypto.quote", trade},
		{"market.crypto.trade", trade},
		{"market.crypto.quote", quote},
		{"market.crypto.bar", bar},
		{coverage.Subject, cov},
	}
	for _, tc := range cases {
		if err := producer.Publish(ctx, bus.Event{Subject: tc.typ, EventType: tc.typ, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, Domain: "market", PartitionKey: id, EventTime: now, IngestionTime: now, Payload: tc.payload}); err != nil {
			t.Fatal(err)
		}
	}
	for pass := 0; pass < 2; pass++ {
		consumer, err := bus.NewConsumer(client, bus.WithDLQ(client), bus.WithDedupWindow(0, 0))
		if err != nil {
			t.Fatal(err)
		}
		// Subscribe before the replay starts; the DLQ frames must come from this
		// pass, not from an earlier successful run left in the retained stream.
		dlq, err := nc.SubscribeSync("dlq.market.>")
		if err != nil {
			t.Fatal(err)
		}
		if err := nc.FlushTimeout(time.Second); err != nil {
			t.Fatal(err)
		}
		seen := make(chan error, len(cases))
		subCtx, stop := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() {
			done <- consumer.Subscribe(subCtx, "market.>", fmt.Sprintf("%s-%d", id, pass), func(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
				if env.PartitionKey != id {
					return nil
				}
				err := ing.Handler(ctx, env, payload)
				select {
				case seen <- err:
				case <-ctx.Done():
				}
				return err
			})
		}()
		rejected := 0
		for range cases {
			select {
			case err := <-seen:
				if err != nil {
					rejected++
				}
			case err := <-done:
				stop()
				t.Fatalf("consumer ended early: %v", err)
			case <-ctx.Done():
				stop()
				t.Fatal(ctx.Err())
			}
		}
		stop()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal("consumer failed to stop")
		}
		if rejected != 2 {
			t.Fatalf("pass %d: refused %d events, want only unknown and mismatched", pass, rejected)
		}
		for range 2 {
			msg, err := dlq.NextMsg(3 * time.Second)
			if err != nil {
				t.Fatalf("missing durable DLQ publication: %v", err)
			}
			if msg.Subject != "dlq.market.crypto.unknown" && msg.Subject != "dlq.market.crypto.quote" {
				t.Fatalf("known non-price traffic went to DLQ: %s", msg.Subject)
			}
			envelope, _, err := bus.Unframe(msg.Data)
			if err != nil || envelope.GetPartitionKey() != id || msg.Header.Get(bus.HeaderDLQOriginalSubject) != strings.TrimPrefix(msg.Subject, "dlq.") {
				t.Fatalf("DLQ lost original identity or concrete redrive address: %v", err)
			}
			streamName, err := js.StreamNameBySubject(ctx, msg.Subject)
			if err != nil {
				t.Fatal(err)
			}
			stream, err := js.Stream(ctx, streamName)
			if err != nil {
				t.Fatal(err)
			}
			stored, err := stream.GetLastMsgForSubject(ctx, msg.Subject)
			if err != nil {
				t.Fatal(err)
			}
			storedEnvelope, _, err := bus.Unframe(stored.Data)
			if err != nil || storedEnvelope.GetEventId() != envelope.GetEventId() {
				t.Fatal("DLQ publication was not retained by JetStream")
			}
		}
		if err := dlq.Unsubscribe(); err != nil {
			t.Fatal(err)
		}
		rows, err := st.History(ctx, store.Query{InstrumentID: id, AsOf: now})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 3 {
			t.Fatalf("pass %d: got %d marks, want trade/quote/bar only: %+v", pass, len(rows), rows)
		}
		want := map[store.PriceKind]*marketpb.MarketDataEvent{store.PriceKindLast: trade, store.PriceKindMid: quote, store.PriceKindClose: bar}
		for _, row := range rows {
			ev, ok := want[row.Kind]
			if !ok {
				t.Fatalf("unexpected mark kind %v", row.Kind)
			}
			expected, err := TranslateEvent(&envelopepb.Envelope{IngestionTime: timestamppb.New(now)}, ev)
			if err != nil || !proto.Equal(row.Price, expected.Price) {
				t.Fatalf("incorrect persisted price: %+v; error=%v", row, err)
			}
			delete(want, row.Kind)
		}
		for _, table := range []string{"price_observations", "ohlcv_bars", "ingestion_coverage"} {
			var count int
			if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{table}.Sanitize()).Scan(&count); err != nil {
				t.Fatal(err)
			}
			expected := 1
			if table == "price_observations" {
				expected = 3
			}
			if count != expected {
				t.Fatalf("pass %d: %s count=%d, want %d", pass, table, count, expected)
			}
		}
	}
}

func boundaryPool(t *testing.T, ctx context.Context, dsn string) *pgxpool.Pool {
	t.Helper()
	boot, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer boot.Close()
	var privileged bool
	if err := boot.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE pg_has_role(current_user, oid, 'MEMBER') AND (rolsuper OR rolbypassrls))`).Scan(&privileged); err != nil {
		t.Fatal(err)
	}
	if privileged {
		t.Fatal("test requires a non-superuser/non-BYPASSRLS role, including memberships")
	}
	schema := fmt.Sprintf("market_boundary_%d", time.Now().UnixNano())
	if _, err := boot.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conn, err := pgx.Connect(cleanupCtx, dsn)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.Close(cleanupCtx) }()
		if _, err := conn.Exec(cleanupCtx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, name := range []string{"0001_price_history.sql", "0003_ohlcv_bars.sql", "0004_ohlcv_trade_count_nullable.sql", "0005_ingestion_coverage.sql"} {
		data, err := os.ReadFile(filepath.Join("..", "..", "services", "market-data", "migrations", name))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(data)); err != nil {
			t.Fatal(err)
		}
	}
	return pool
}
