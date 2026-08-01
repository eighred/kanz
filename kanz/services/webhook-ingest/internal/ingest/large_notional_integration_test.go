package ingest

// #86 — THE MONEY-ARITHMETIC FIXES, EXERCISED AGAINST A REAL BROKER AND A REAL POSTGRES.
//
// #86's bar is that four merged-but-unexecuted fixes are exercised "against a
// real NATS broker and a real PostgreSQL — not fakeBus, which does not validate
// envelopes and therefore accepts what a real broker rejects."
//
// THE HAZARD HERE IS THE WRAP, NOT THE HANG. These two have been conflated
// repeatedly and they are different failures with different fixes:
//
//	hang  — dec.FromProto materialises 10^abs(exponent); a wild EXPONENT never
//	        returns. Proven against a real broker by the domain-boundary test
//	        beside this one (#95).
//	wrap  — dec.ToProto packs a scaled COEFFICIENT into an int64 and overflows at
//	        roughly 92.2 billion units at scale 8. A wrapped coefficient is not a
//	        refused order, it is a FABRICATED NUMBER the system then acts on. #86
//	        records a wrap that "admitted a $184bn notional against an $80k book".
//
// The fix was to move every capital path onto dec.ToProtoScaled, which RESCALES
// instead of wrapping: it raises the exponent until the coefficient fits,
// trading precision it does not need for magnitude it cannot get wrong. "You do
// not need eight decimal places on $100bn; you do need the magnitude to be
// right" (dec.go).
//
// So the assertion is MAGNITUDE PRESERVATION end to end. A quantity chosen above
// the wrap threshold is submitted over a real broker, and the number that comes
// back on the wire — and the number that lands in Postgres — must still be that
// quantity. Under the pre-fix conversion the coefficient overflows into a small
// or negative value, which is exactly what this catches: not an error, a
// plausible wrong number.
//
// Three of #86's four fixes are on this one path: the OMS aggregate's capital
// conversion (6cfc1cc), the pre-trade gate's notional arithmetic (6568855 — the
// notional here is 5e16, far past where the doubled wrap bit), and the single
// representability-checked conversion (ca8049e). The fourth, the Decimal domain,
// is the neighbouring test.

import (
	"context"
	"fmt"
	"math/big"
	"os"
	"sync"
	"testing"
	"time"

	commandpb "github.com/eighred/kanz/kanz-schemas-go/command/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/signal/translate"
	"github.com/eighred/kanz/pkg/bus"
)

// largeQuantity is above the wrap threshold ON PURPOSE.
//
// dec packs at scale 8, so the coefficient for q is q*1e8 and int64 tops out at
// 9.22e18 — a wrap at roughly 92.2 billion units. 1e12 units scales to 1e20,
// comfortably past it, so the pre-fix ToProto could not represent this and the
// post-fix ToProtoScaled must (by raising the exponent, exactly: 1e18 x 1e-6).
//
// A value just over the line would also wrap, but a wrapped result near the
// boundary can look like a plausible quantity. This one cannot be mistaken for
// anything but what it is.
const largeQuantityUnits = 1_000_000_000_000 // 1e12

func TestIntegration_ALargeQuantityKeepsItsMagnitudeOverTheBusAndInPostgres(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("set TEST_NATS_URL (with the dev stack + OMS running) to run the money-arithmetic proof")
	}
	if os.Getenv("TEST_OMS_ON_BUS") == "" {
		t.Skip("set TEST_OMS_ON_BUS=1 with an OMS consuming order.order.submit on TEST_NATS_URL")
	}
	// The persisted half is a SEPARATE precondition, declared separately: without
	// it this would still prove the wire and quietly prove nothing about the store.
	pgURL := os.Getenv("TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("set TEST_POSTGRES_URL to the database the OMS writes to")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "large-notional-it"})
	if err != nil {
		t.Fatalf("dial NATS: %v", err)
	}
	defer func() { _ = client.Close() }()

	consumer, err := bus.NewConsumer(client)
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}
	var mu sync.Mutex
	filled := map[string]*big.Rat{}
	fills := make(chan string, 8)
	go func() {
		_ = consumer.Subscribe(ctx, "order.order.filled", "large-notional-it", func(_ context.Context, _ *envelopepb.Envelope, payload []byte) error {
			var ev orderpb.OrderFilled
			if err := proto.Unmarshal(payload, &ev); err != nil {
				return nil
			}
			mu.Lock()
			filled[ev.GetOrderId()] = dec.FromProto(ev.GetState().GetFilledQuantity())
			mu.Unlock()
			fills <- ev.GetOrderId()
			return nil
		})
	}()
	time.Sleep(500 * time.Millisecond)

	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "large-notional-it", ProducerVersion: "it"})
	if err != nil {
		t.Fatalf("producer: %v", err)
	}

	orderID := fmt.Sprintf("it-large-%d", time.Now().UnixNano())
	want := new(big.Rat).SetInt64(largeQuantityUnits)

	// Built with ToProtoScaled, the same conversion the capital paths use — so the
	// command on the wire carries the rescaled form a real producer would send,
	// not a hand-packed coefficient that dodges the code under test.
	qty, ok := dec.ToProtoScaled(want)
	if !ok {
		t.Fatalf("ToProtoScaled refused %s — the test's own input is unrepresentable", want.RatString())
	}
	if qty.GetCoefficient() < 0 {
		t.Fatalf("ToProtoScaled produced a NEGATIVE coefficient (%d) for a positive quantity — "+
			"that is the wrap this issue exists about, in the conversion itself", qty.GetCoefficient())
	}
	if err := publishLargeSubmit(ctx, producer, orderID, qty); err != nil {
		t.Fatalf("publish: %v", err)
	}

	deadline := time.After(30 * time.Second)
	for {
		mu.Lock()
		got, have := filled[orderID]
		mu.Unlock()
		if have {
			// THE WIRE. A wrapped coefficient shows up here as a small or negative
			// number, never as an error, so equality against the submitted quantity
			// is the whole assertion.
			if got.Cmp(want) != 0 {
				t.Fatalf("filled quantity came back as %s, want %s — the magnitude did not survive "+
					"the round trip. A wrapped coefficient is a fabricated number the platform then "+
					"acts on; %s was the notional class that admitted $184bn against an $80k book",
					got.RatString(), want.RatString(), got.RatString())
			}
			if got.Sign() <= 0 {
				t.Fatalf("filled quantity is %s — non-positive for a buy of %s", got.RatString(), want.RatString())
			}
			break
		}
		select {
		case <-fills:
		case <-deadline:
			t.Fatalf("no fill for %s within 30s. The OMS either refused a representable large order "+
				"or did not survive it; check its log for a conversion refusal", orderID)
		}
	}

	// THE STORE. The wire could be right while the persisted state is wrong — they
	// are different conversions — so the order is read back out of Postgres and
	// decoded. This also runs under FORCED row-level security as a NOSUPERUSER
	// role, so the read only succeeds with the tenant set, which is the posture
	// production runs in rather than a convenient bypass.
	conn, err := pgx.Connect(ctx, pgURL)
	if err != nil {
		t.Fatalf("connect postgres: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, "SELECT set_config('app.tenant_id', $1, false)", "__system__"); err != nil {
		t.Fatalf("set tenant: %v", err)
	}
	var stateBytes []byte
	if err := conn.QueryRow(ctx, `SELECT state FROM orders WHERE order_id = $1`, orderID).Scan(&stateBytes); err != nil {
		t.Fatalf("read persisted order %s: %v (RLS is FORCED here — a missing row can also mean the "+
			"tenant was not set)", orderID, err)
	}
	var st orderpb.OrderState
	if err := proto.Unmarshal(stateBytes, &st); err != nil {
		t.Fatalf("decode persisted state: %v", err)
	}
	stored := dec.FromProto(st.GetFilledQuantity())
	if stored.Cmp(want) != 0 {
		t.Errorf("the PERSISTED filled quantity is %s, want %s — the wire survived the magnitude but "+
			"the store did not, which is the worse of the two: the bus record and the book disagree",
			stored.RatString(), want.RatString())
	}
}

func publishLargeSubmit(ctx context.Context, p *bus.Producer, orderID string, qty *commonpb.Decimal) error {
	cmd := &orderpb.SubmitOrder{
		Metadata:     &commandpb.CommandMetadata{Issuer: "strategy:large-notional-it", TargetId: orderID},
		OrderId:      orderID,
		PortfolioId:  "fund-alpha",
		InstrumentId: "BTC-USD",
		Side:         orderpb.Side_SIDE_BUY,
		Quantity:     qty,
		OrderType:    orderpb.OrderType_ORDER_TYPE_LIMIT,
		// 50,000 — with the quantity above this is a 5e16 notional, which is where
		// the pre-trade gate's own doubled wrap used to bite (6568855).
		LimitPrice:  &commonpb.Decimal{Coefficient: 5_000_000_000_000, Exponent: -8},
		TimeInForce: orderpb.TimeInForce_TIME_IN_FORCE_GTC,
		Venue:       "XNAS",
	}
	return p.Publish(ctx, bus.Event{
		Subject:        translate.SubjectSubmit,
		EventType:      translate.SubjectSubmit,
		EventClass:     envelopepb.EventClass_EVENT_CLASS_COMMAND,
		SchemaVersion:  1,
		Domain:         "order",
		EventTime:      time.Now().UTC(),
		PartitionKey:   orderID,
		IdempotencyKey: orderID,
		TenantID:       "__system__",
		Payload:        cmd,
	})
}
