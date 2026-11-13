package marketdata

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/bustest"
	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/marketdata/store"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
)

// The unit fake cannot prove framing, broker acceptance, SQL encoding or DLQ
// retention. Exercise those boundaries with real services and independent exact
// values, including replay through a new durable consumer without local dedup.
func TestMIDExactJetStreamPostgres(t *testing.T) {
	dsn, url := os.Getenv("TEST_POSTGRES_URL"), os.Getenv("TEST_NATS_URL")
	if dsn == "" || url == "" {
		t.Skip("requires TEST_POSTGRES_URL and TEST_NATS_URL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	pool := boundaryPool(t, ctx, dsn)
	t.Log("PostgreSQL role and its memberships have neither SUPERUSER nor BYPASSRLS; market facts are global, not tenant-owned")
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
	bustest.EnsureSubjects(t, ctx, js, "MID_MARKET", []string{"market.>"})
	bustest.EnsureSubjects(t, ctx, js, "MID_DLQ", []string{"dlq.>"})
	id := fmt.Sprintf("mid-exact-%d", time.Now().UnixNano())
	// Isolate the fixture's deliveries while retaining the production market.>
	// route and canonical quote event type.
	subject := "market.crypto.quote." + id
	streamName, err := js.StreamNameBySubject(ctx, subject+".0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanupCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		stream, err := js.Stream(cleanupCtx, streamName)
		if err != nil {
			t.Error(err)
			return
		}
		names := stream.ConsumerNames(cleanupCtx)
		for name := range names.Name() {
			if strings.HasPrefix(name, id+"-") {
				if err := js.DeleteConsumer(cleanupCtx, streamName, name); err != nil {
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
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "mid-exact-test", ProducerVersion: "test", Tenant: "__system__"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	cases := exactMidpointCases()
	// These are valid ingress Decimals. The existing exact encoder cannot emit
	// their half-unit coefficient or normalized exponent, respectively.
	cases = append(cases,
		midpointCase{"unsupported coefficient", decv(math.MaxInt64-1, 0), decv(math.MaxInt64, 0), ""},
		midpointCase{"unsupported exact exponent", decv(1, 61), decv(1, 61), ""},
	)
	for n, tc := range cases {
		ev := midpointQuote(fmt.Sprintf("%s-%d", id, n), tc.bid, tc.ask)
		ev.EventTime = timestamppb.New(now)
		if err := producer.Publish(ctx, bus.Event{Subject: fmt.Sprintf("%s.%d", subject, n), EventType: "market.crypto.quote", EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, Domain: "market", PartitionKey: ev.InstrumentId, EventTime: now, IngestionTime: now, Payload: ev}); err != nil {
			t.Fatal(err)
		}
	}
	for pass := 0; pass < 2; pass++ {
		group := fmt.Sprintf("%s-%d", id, pass)
		consumer, err := bus.NewConsumer(client, bus.WithDLQ(client), bus.WithDedupWindow(0, 0))
		if err != nil {
			t.Fatal(err)
		}
		dlq, err := nc.SubscribeSync("dlq." + subject + ".*")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = dlq.Unsubscribe() }()
		if err := nc.FlushTimeout(time.Second); err != nil {
			t.Fatal(err)
		}
		seen := make(chan error, len(cases))
		subCtx, stop := context.WithCancel(ctx)
		defer stop()
		done := make(chan error, 1)
		go func() {
			done <- consumer.Subscribe(subCtx, subject+".*", group, func(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
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
					if !strings.Contains(err.Error(), "unsupported exact MID encoding") {
						t.Errorf("unexpected handler refusal: %v", err)
					}
				}
			case err := <-done:
				stop()
				t.Fatalf("consumer ended early: %v", err)
			case <-ctx.Done():
				stop()
				t.Fatal(ctx.Err())
			}
		}
		if rejected != 2 {
			t.Fatalf("pass %d: refused %d quotes, want only the two unsupported exact encodings", pass, rejected)
		}
		// Await durable parking before cancellation: Handler returns before the
		// consumer publishes its refusal, so receiving seen is insufficient.
		refused := make(map[string]bool)
		for range 2 {
			msg, err := dlq.NextMsg(5 * time.Second)
			if err != nil {
				stop()
				t.Fatal(err)
			}
			env, _, err := bus.Unframe(msg.Data)
			if err != nil || msg.Header.Get(bus.HeaderDLQOriginalSubject) != strings.TrimPrefix(msg.Subject, "dlq.") {
				t.Fatalf("DLQ lost concrete source or framing: %v", err)
			}
			refused[env.GetPartitionKey()] = true
			dlqStreamName, err := js.StreamNameBySubject(ctx, msg.Subject)
			if err != nil {
				t.Fatal(err)
			}
			dlqStream, err := js.Stream(ctx, dlqStreamName)
			if err != nil {
				t.Fatal(err)
			}
			stored, err := dlqStream.GetLastMsgForSubject(ctx, msg.Subject)
			if err != nil {
				t.Fatal("DLQ refusal was not retained by JetStream")
			}
			storedEnvelope, _, err := bus.Unframe(stored.Data)
			if err != nil || storedEnvelope.GetEventId() != env.GetEventId() {
				t.Fatal("retained DLQ refusal lost its original event identity")
			}
		}
		if err := dlq.Unsubscribe(); err != nil {
			t.Fatal(err)
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
		for n, tc := range cases {
			instrument := fmt.Sprintf("%s-%d", id, n)
			rows, err := st.History(ctx, store.Query{InstrumentID: instrument, AsOf: now})
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if len(rows) != 0 || !refused[instrument] {
					t.Fatalf("pass %d %s: unsupported MID stored or not parked", pass, tc.name)
				}
				continue
			}
			if len(rows) != 1 || refused[instrument] {
				t.Fatalf("pass %d %s: got %d rows or erroneous refusal", pass, tc.name, len(rows))
			}
			want, ok := new(big.Rat).SetString(tc.want)
			if !ok {
				t.Fatal("invalid independent rational fixture")
			}
			row := rows[0]
			got, ok := dec.FromProtoChecked(row.Price)
			if !ok || got.Cmp(want) != 0 || row.Kind != store.PriceKindMid || !row.ObservationTime.Equal(now) || !row.KnowledgeTime.Equal(now) {
				t.Fatalf("pass %d %s: persisted MID=%v, want exact %s with original times", pass, tc.name, got, want)
			}
		}
		var marks, bars, coverage int
		if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM price_observations),
			(SELECT count(*) FROM ohlcv_bars), (SELECT count(*) FROM ingestion_coverage)`).Scan(&marks, &bars, &coverage); err != nil {
			t.Fatal(err)
		}
		if marks != len(cases)-2 || bars != 0 || coverage != 0 {
			t.Fatalf("pass %d: unexpected writes: marks=%d bars=%d coverage=%d", pass, marks, bars, coverage)
		}
		t.Logf("pass %d: %d exact MID rows, two unsupported results refused and retained in DLQ, no extra writes", pass, marks)
	}
}
