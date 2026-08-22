// THE FILL AND RECONCILIATION FACTs, DRIVEN THROUGH A REAL PRODUCER (#245).
//
// reconCapture is an EVENT-LEVEL double that accepts anything: it appends the
// bus.Event and returns nil. The existing tests use it to assert WHICH events an
// adapter emits and what their payloads say, which is the right tool for that —
// but it means no test in this package has ever seen bus.Validate, and the
// envelope these FACTs ride was unproven.
//
// WHAT RIDES ON IT. MOST FILLS ARRIVE HERE, on the user-data websocket, long
// after Execute returned. A fill FACT the broker refuses is an order that was
// worked at a live exchange and never reported: the fund's money moved and the
// OMS never heard. The reconciler's StateHealed and BalanceReconciled are the
// same shape one layer out — they exist to correct drift, so losing them means
// the drift stays.
//
// THE DERIVATION IS THE POINT (#245's entry for this package names it). None of
// these four publish sites sets PayloadSchemaRef; the producer derives it from
// the payload's proto full name and e.SchemaVersion. Validate requires it
// non-empty, so a derivation that returned "" would fail every publish — and
// that is not hypothetical: producer.go's own comment records TWELVE sites that
// omitted the field, validated fine against doubles, and failed on the first
// real broker. The derived value is asserted here rather than assumed.
package binance

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/pkg/bus"
)

// captureClient records the framed wire bytes — the Tier-B helper from
// internal/risk/publish and services/accounting/internal/cashmove.
type captureClient struct{ sent []bus.Message }

func (c *captureClient) Publish(_ context.Context, m bus.Message) error {
	c.sent = append(c.sent, m)
	return nil
}
func (c *captureClient) Subscribe(context.Context, string, string, bus.Handler) error {
	return errors.New("not implemented")
}
func (c *captureClient) Close() error { return nil }

// realVenueProducer mirrors cmd/venue-binance's venueProducerConfig, INCLUDING
// the Tenant fallback. That field is load-bearing for the market-trade publish
// the ticker feed makes: it sets no TenantID of its own, so without the fallback
// every one of those events is refused. It used to discard the refusal too — #673
// moved that publish into execution.MarkTickPublisher, which counts and names it
// — but the fallback is still the only thing standing between the feed and a
// broker that says no. See TestAVenueEventWithoutTheTenantFallbackIsRefused.
func realVenueProducer(t *testing.T) (*bus.Producer, *captureClient) {
	t.Helper()
	cc := &captureClient{}
	prod, err := bus.NewProducer(cc, bus.ProducerConfig{
		Source:          "venue-binance",
		ProducerVersion: "test",
		Tenant:          "fund-alpha",
	})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	return prod, cc
}

// checkEnvelope unframes one captured message and asserts the fields Validate
// enforces, plus the DERIVED schema ref.
func checkEnvelope(t *testing.T, m bus.Message, wantType, wantSchemaRef, wantKey string) *envelopepb.Envelope {
	t.Helper()
	env, _, err := bus.Unframe(m.Body)
	if err != nil {
		t.Fatalf("Unframe: %v", err)
	}
	if err := bus.Validate(env); err != nil {
		t.Fatalf("the emitted envelope fails Validate: %v", err)
	}
	if got := env.GetEventType(); got != wantType {
		t.Errorf("event_type = %q, want %q", got, wantType)
	}
	if got := env.GetPayloadSchemaRef(); got != wantSchemaRef {
		t.Errorf("payload_schema_ref = %q, want %q — this site sets none, so the producer DERIVES it "+
			"from the payload type. A wrong or empty derivation fails every publish", got, wantSchemaRef)
	}
	if got := env.GetTenantId(); got != "fund-alpha" {
		t.Errorf("tenant_id = %q, want fund-alpha", got)
	}
	if got := env.GetEventClass(); got != envelopepb.EventClass_EVENT_CLASS_FACT {
		t.Errorf("event_class = %v, want FACT", got)
	}
	if got := string(m.Key); got != wantKey {
		t.Errorf("partition key = %q, want %q", got, wantKey)
	}
	return env
}

// THE FILL FACT. Most fills arrive on this path, and a refused envelope means an
// order worked at a live exchange that the OMS never learns about.
func TestUserDataFillEmitsAValidEnvelope(t *testing.T) {
	prod, cc := realVenueProducer(t)
	report := `{"e":"executionReport","s":"BTCUSDT","c":"o1","S":"BUY","x":"TRADE","X":"FILLED",` +
		`"l":"1","L":"50000","z":"1","q":"1","t":7,"T":1700000000000}`

	ing := newUserDataIngester(UserDataConfig{
		Stream: &fakeStream{frames: [][]byte{[]byte(report)}},
		Lookup: fakeLookup{"o1": kanzOrder("o1")},
		Pub:    prod, Venue: "BINANCE", Tenant: "fund-alpha",
	})
	_ = ing.Run(context.Background()) // returns io.EOF at end of frames

	if len(cc.sent) != 1 {
		t.Fatalf("published %d messages, want 1 — the fill FACT did not reach the transport", len(cc.sent))
	}
	checkEnvelope(t, cc.sent[0], "order.order.filled", "order.v1.OrderFilled:1", "o1")
}

// THE RECONCILER'S CORRECTING FACT. It exists to fix drift; an envelope the
// broker refuses leaves the drift in place and reports nothing.
func TestReconStateHealedEmitsAValidEnvelope(t *testing.T) {
	prod, cc := realVenueProducer(t)
	f := newFakeBinance(t)
	f.queryBody = `{"symbol":"BTCUSDT","clientOrderId":"o1","status":"FILLED","executedQty":"2.00000000"}`
	exp := staticOrders{{
		OrderId: "o1", PortfolioId: "fund-alpha", InstrumentId: "BTC-USD",
		Side: orderpb.Side_SIDE_BUY, OrderType: orderpb.OrderType_ORDER_TYPE_LIMIT,
		OrderedQuantity: bdec("2"), FilledQuantity: bdec("1"),
		Status: orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED,
	}}
	bucket := newWeightBucket(1200, time.Minute, nil)
	r := newReconciler(ReconcilerConfig{
		REST:     newBinanceREST(restConfig{BaseURL: f.srv.URL, APIKey: "k", APISecret: "s", Bucket: bucket}),
		Symbols:  StaticSymbolMap{"BTC-USD": "BTCUSDT"},
		Expected: exp, Pub: prod, Venue: "BINANCE", Tenant: "fund-alpha",
	})

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(cc.sent) == 0 {
		t.Fatal("the reconciler healed drift and published nothing")
	}
	env := checkEnvelope(t, cc.sent[0], subjectStateHealed, "order.v1.StateHealed:1", "o1")

	// A correcting FACT must say it is one, or a consumer folds it as fresh truth
	// alongside the value it is correcting.
	var revised bool
	for _, q := range env.GetQualityFlags() {
		if q == envelopepb.QualityFlag_QUALITY_FLAG_REVISED {
			revised = true
		}
	}
	if !revised {
		t.Error("StateHealed carries no QUALITY_FLAG_REVISED — it is a correction, and a consumer " +
			"that cannot tell will fold it beside the value it corrects")
	}
}

// THE BALANCE BREAK. A different payload type, so a different derived schema
// ref: proving one payload's derivation says nothing about another's.
func TestReconBalanceReconciledEmitsAValidEnvelope(t *testing.T) {
	prod, cc := realVenueProducer(t)
	f := newFakeBinance(t)
	f.queryBody = `{"symbol":"BTCUSDT","clientOrderId":"o1","status":"FILLED","executedQty":"1.00000000"}`
	// The venue holds 1 BTC; Balances below expects 5. That gap is the break.
	f.accountBody = `{"balances":[{"asset":"BTC","free":"1.0","locked":"0"}]}`
	bucket := newWeightBucket(1200, time.Minute, nil)
	r := newReconciler(ReconcilerConfig{
		REST:    newBinanceREST(restConfig{BaseURL: f.srv.URL, APIKey: "k", APISecret: "s", Bucket: bucket}),
		Symbols: StaticSymbolMap{"BTC-USD": "BTCUSDT"},
		// Expected must be non-nil even when empty: reconcileOrders dereferences it
		// unconditionally (binance_recon.go). The composition root always sets
		// one, so this is a test-fixture requirement rather than a product gap.
		Expected: staticOrders{},
		Balances: staticBalances{"BTC": big.NewRat(5, 1)},
		Pub:      prod, Venue: "BINANCE", Tenant: "fund-alpha",
	})

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var found bool
	for _, m := range cc.sent {
		env, _, err := bus.Unframe(m.Body)
		if err != nil {
			t.Fatalf("Unframe: %v", err)
		}
		if env.GetEventType() != subjectBalanceRecon {
			continue
		}
		found = true
		if err := bus.Validate(env); err != nil {
			t.Errorf("the balance-break envelope fails Validate: %v", err)
		}
		if got := env.GetPayloadSchemaRef(); got != "accounting.v1.BalanceReconciled:1" {
			t.Errorf("payload_schema_ref = %q, want accounting.v1.BalanceReconciled:1", got)
		}
		if got := env.GetTenantId(); got != "fund-alpha" {
			t.Errorf("tenant_id = %q", got)
		}
	}
	if !found {
		t.Fatalf("no %s published from a balance that does not match the venue's", subjectBalanceRecon)
	}
}

// THE TENANT FALLBACK IS LOAD-BEARING, AND ITS FAILURE WOULD BE SILENT.
//
// The market-trade publish sets no TenantID, so it depends entirely on
// venueProducerConfig setting ProducerConfig.Tenant — if that were ever dropped,
// every market trade would be refused by the broker. This pins the dependency
// where someone editing either side will find it.
//
// "AND NOTHING WOULD SAY SO" WAS TRUE HERE UNTIL #673, which is why this test
// was written the way it is. The publish discarded its error, so a broker
// refusing every tick left no line and no series. It now runs through
// execution.MarkTickPublisher, which counts every drop and logs the reason
// rate-limited, and the counter is wired in this service's composition root.
func TestAVenueEventWithoutTheTenantFallbackIsRefused(t *testing.T) {
	cc := &captureClient{}
	prod, err := bus.NewProducer(cc, bus.ProducerConfig{
		Source:          "venue-binance",
		ProducerVersion: "test",
		// Tenant deliberately absent.
	})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}

	// The shape binance_connector.go builds: no TenantID of its own.
	err = prod.Publish(context.Background(), bus.Event{
		Subject: "market.crypto.trade", EventType: "market.crypto.trade",
		EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, Domain: "market",
		EventTime: time.Now().UTC(), PartitionKey: "BTC-USD",
		Payload: &accountingpb.BalanceReconciled{Asset: "BTC"}, // any payload; the tenant is what is under test
	})
	if err == nil {
		t.Fatal("an event with no TenantID published against a producer with no Tenant fallback. " +
			"the market-trade path relies on that fallback, so if this ever succeeds silently the " +
			"one thing standing between the mark feed and a broker refusal has stopped being checked")
	}
	if len(cc.sent) != 0 {
		t.Errorf("a refused envelope still reached the transport (%d message(s))", len(cc.sent))
	}
}
