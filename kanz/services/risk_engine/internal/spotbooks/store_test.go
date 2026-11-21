package spotbooks

import (
	"context"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/execution"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"
	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"
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
	accounts, err := execution.ParseBindings("pilot/alpha@XBIN=bn-alpha,pilot/alpha@XOKX=ok-alpha,pilot/beta@OKX=ok-beta,foreign/beta@BINANCE=bn-other")
	if err != nil {
		t.Fatal(err)
	}
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
		got := s.Observe(tenant, "alpha", "BTC-USDT", accounts)
		if got.Status != status || (status == "UNKNOWN" && len(got.Venues) != 0) {
			t.Fatal(got)
		}
	}
	refusal := func(tenant string, code querypb.SpotBookRefusal, venue string) {
		t.Helper()
		got := s.Observe(tenant, "alpha", "BTC-USDT", accounts)
		if got.Status != "UNKNOWN" || got.Refusal != code || got.RefusalVenue != venue || len(got.Venues) != 0 ||
			got.Source != "" || got.RightsReference != "" || got.Reason == "" {
			t.Fatal(got)
		}
		if code == querypb.SpotBookRefusal_SPOT_BOOK_REFUSAL_UNSPECIFIED {
			if got.AsOf != nil || got.Reason != "No authorized current pair of books is available." {
				t.Fatal("ungranted observation disclosed diagnostics", got)
			}
		} else if got.AsOf == nil || !got.AsOf.AsTime().Equal(now) {
			t.Fatal("missing refusal cutoff", got)
		}
	}
	at := now.Add(-time.Second)
	send("pilot", "BINANCE", "1.0.1", at, 1, 11)
	send("foreign", "OKX", "1.0.1", at, 2, 12)
	send("pilot", "OKX", "wrong", at, 2, 12)
	refusal("pilot", querypb.SpotBookRefusal_SPOT_BOOK_REFUSAL_MISSING, "OKX")
	send("pilot", "OKX", "1.0.1", at, 2, 12)
	want("pilot", "SUPPORTED")
	refusal("foreign", querypb.SpotBookRefusal_SPOT_BOOK_REFUSAL_UNSPECIFIED, "")
	initial := s.Observe("pilot", "alpha", "BTC-USDT", accounts)
	beta := s.Observe("pilot", "beta", "BTC-USDT", accounts)
	if initial.Venues[0].PortfolioAccountRef != "bn-alpha" || initial.Venues[0].PortfolioAccountMic != "XBIN" ||
		initial.Venues[1].PortfolioAccountRef != "ok-alpha" || initial.Venues[1].PortfolioAccountMic != "XOKX" ||
		beta.Venues[0].PortfolioAccountRef != "" || beta.Venues[1].PortfolioAccountRef != "ok-beta" {
		t.Fatal("book projections did not isolate the tenant, portfolio and venue")
	}
	now = now.Add(time.Millisecond * 100)
	send("pilot", "OKX", "1.0.1", at, 2, 12)
	if !proto.Equal(initial.Venues[1].KnownAt, s.Observe("pilot", "alpha", "BTC-USDT", accounts).Venues[1].KnownAt) {
		t.Fatal("duplicate refreshed receipt")
	}
	send("pilot", "OKX", "1.0.1", at, 2, 13)
	refusal("pilot", querypb.SpotBookRefusal_SPOT_BOOK_REFUSAL_INVALID, "OKX")
	send("pilot", "OKX", "1.0.1", at.Add(-time.Millisecond), 1, 12)
	want("pilot", "UNKNOWN")
	send("pilot", "OKX", "1.0.1", now, 3, 13)
	refusal("pilot", querypb.SpotBookRefusal_SPOT_BOOK_REFUSAL_SKEW, "")
	send("pilot", "BINANCE", "1.0.1", now, 4, 11)
	want("pilot", "SUPPORTED")
	now = now.Add(time.Second * 3)
	refusal("pilot", querypb.SpotBookRefusal_SPOT_BOOK_REFUSAL_STALE, "BINANCE")
	now = grants["BTC-USDT"].Until
	send("pilot", "BINANCE", "1.0.1", now, 5, 11)
	send("pilot", "OKX", "1.0.1", now, 6, 13)
	refusal("pilot", querypb.SpotBookRefusal_SPOT_BOOK_REFUSAL_UNSPECIFIED, "")
	for _, bad := range []string{"[]", grant + " {}", `[{"instrument":"BTC-USDT"}]`} {
		if _, err = ParseGrants(bad, "pilot"); err == nil {
			t.Fatalf("accepted invalid grant %q", bad)
		}
	}
}
