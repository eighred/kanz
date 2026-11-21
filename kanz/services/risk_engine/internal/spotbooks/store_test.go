package spotbooks

import (
	"context"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestCaptureRequiresCurrentConsistentTenantBooks(t *testing.T) {
	const grant = `[{"tenant":"pilot","instrument":"BTC-USDT","product_type":"CRYPTO_SPOT","source":"market","producer_version":"1.0.1","rights_reference":"demo","base_asset_ref":"bitcoin","quote_asset_ref":"tether-usd","valid_until":"2026-11-22T00:00:00Z"}]`
	grants, err := ParseGrants(grant, "pilot")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseGrants(grant, "foreign"); err == nil {
		t.Fatal("another tenant accepted the grant")
	}
	s := New("pilot", grants)
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	send := func(tenant, venue, version string, at time.Time, seq uint64, ask int64) {
		t.Helper()
		symbol := "BTCUSDT"
		if venue == "OKX" {
			symbol = "BTC-USDT"
		}
		level := func(x int64) *marketpb.PriceLevel {
			return &marketpb.PriceLevel{Price: &commonpb.Decimal{Coefficient: x}, Size: &commonpb.Decimal{Coefficient: 1}}
		}
		b := &marketpb.OrderBookSnapshot{InstrumentId: "BTC-USDT", Symbol: symbol, Mic: venue, LastUpdateSequence: seq, EventTime: timestamppb.New(at), Bids: []*marketpb.PriceLevel{level(10)}, Asks: []*marketpb.PriceLevel{level(ask)}}
		wire, err := proto.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		env := &envelopepb.Envelope{TenantId: tenant, Source: "market", ProducerVersion: version, PartitionKey: b.InstrumentId, SchemaVersion: 1, PayloadSchemaRef: "market.v1.OrderBookSnapshot:1", EventType: Subject, Domain: "market", EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, EventId: "event-" + venue, IngestionTime: timestamppb.New(now), PublishTime: timestamppb.New(now)}
		if err = s.Handle(context.Background(), env, wire); err != nil {
			t.Fatal(err)
		}
	}
	want := func(tenant, status string) {
		t.Helper()
		got := s.Observe(tenant, "BTC-USDT")
		if got.Status != status || (status == "UNKNOWN" && len(got.Venues) != 0) {
			t.Fatal(got)
		}
	}
	at := now.Add(-time.Second)
	send("pilot", "BINANCE", "1.0.1", at, 1, 11)
	send("foreign", "OKX", "1.0.1", at, 2, 12)
	send("pilot", "OKX", "wrong", at, 2, 12)
	want("pilot", "UNKNOWN")
	send("pilot", "OKX", "1.0.1", at, 2, 12)
	want("pilot", "SUPPORTED")
	want("foreign", "UNKNOWN")
	initial := s.Observe("pilot", "BTC-USDT")
	now = now.Add(time.Millisecond * 100)
	send("pilot", "OKX", "1.0.1", at, 2, 12)
	if !proto.Equal(initial.Venues[1].KnownAt, s.Observe("pilot", "BTC-USDT").Venues[1].KnownAt) {
		t.Fatal("duplicate refreshed receipt")
	}
	send("pilot", "OKX", "1.0.1", at, 2, 13)
	want("pilot", "UNKNOWN")
	send("pilot", "OKX", "1.0.1", at.Add(-time.Millisecond), 1, 12)
	want("pilot", "UNKNOWN")
	send("pilot", "OKX", "1.0.1", now, 3, 13)
	want("pilot", "UNKNOWN") // more than one second of venue skew
	send("pilot", "BINANCE", "1.0.1", now, 4, 11)
	want("pilot", "SUPPORTED")
	now = now.Add(time.Second * 3)
	want("pilot", "UNKNOWN")
	now = grants["BTC-USDT"].Until
	send("pilot", "BINANCE", "1.0.1", now, 5, 11)
	send("pilot", "OKX", "1.0.1", now, 6, 13)
	want("pilot", "UNKNOWN")
	for _, bad := range []string{"[]", grant + " {}", `[{"instrument":"BTC-USDT"}]`} {
		if _, err = ParseGrants(bad, "pilot"); err == nil {
			t.Fatalf("accepted invalid grant %q", bad)
		}
	}
}
