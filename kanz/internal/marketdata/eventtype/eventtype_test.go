package eventtype_test

import (
	"testing"

	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"

	"github.com/eighred/kanz/internal/marketdata/eventtype"
)

// THE VARIANT TOKENS ARE THE ONEOF'S FIELD NAMES, AND THIS DERIVES THEM RATHER
// THAN RESTATING THEM.
//
// services/market-data/internal/feed/bussink.go builds the third event_type
// token from the arm actually set, using the field's own name. So a fourth arm
// added to market.v1.MarketDataEvent.data ships a fourth subject variant on the
// same day — and Of, being a closed switch over three literals, would answer
// None for it. Every fold would then refuse the new variant silently: no error,
// no DLQ, no metric, just a price series that stops arriving.
//
// A list of the three tokens written out here would be a further copy of the
// thing that broke. The descriptor is the source of truth, so the descriptor is
// what this reads.
func TestEveryOneofArmHasAVariant(t *testing.T) {
	oneof := (&marketpb.MarketDataEvent{}).ProtoReflect().Descriptor().Oneofs().ByName("data")
	if oneof == nil {
		t.Fatal("market.v1.MarketDataEvent has no `data` oneof — the taxonomy this package " +
			"classifies is derived from it, so this guard is blind")
	}
	if oneof.Fields().Len() == 0 {
		t.Fatal("market.v1.MarketDataEvent.data has no arms — this guard would pass having checked nothing")
	}
	for i := range oneof.Fields().Len() {
		arm := string(oneof.Fields().Get(i).Name())
		if got := eventtype.Of("market.crypto." + arm); got == eventtype.None {
			t.Errorf("Of(%q) = None, but %q is an arm of market.v1.MarketDataEvent.data and "+
				"bussink.go stamps that arm's field name as the event_type's variant token. Every "+
				"fold on market.> would refuse this variant before unmarshalling it — silently, "+
				"because a refusal on that path is an ack", "market.crypto."+arm, arm)
		}
	}
}

// THE POISONING CASE, CLOSED AT THE CLASSIFIER. market.book.snapshot carries an
// OrderBookSnapshot, which decodes cleanly as a MarketDataEvent (see the package
// doc) — so this is the one answer that has to be None for a reason no decode
// can supply.
func TestBookSnapshotAnnouncesNoVariant(t *testing.T) {
	if got := eventtype.Of("market.book.snapshot"); got != eventtype.None {
		t.Fatalf("Of(\"market.book.snapshot\") = %d, want None. An OrderBookSnapshot is "+
			"wire-compatible with MarketDataEvent, so admitting it here folds the deepest "+
			"resting book level as a price", got)
	}
}

// The rest of the `market.` domain a fold on the wildcard actually receives, and
// the malformed shapes around the taxonomy. Each must be None, and the three
// real variants must not be — a classifier that answered None for everything
// would pass the case above while taking every fold out of service.
func TestOfClassifiesTheWildcardTraffic(t *testing.T) {
	for _, tc := range []struct {
		eventType string
		want      eventtype.Variant
		why       string
	}{
		{"market.crypto.trade", eventtype.Trade, "a genuine print"},
		{"market.equity.quote", eventtype.Quote, "a genuine quote, and any asset class token"},
		{"market.crypto.bar", eventtype.Bar, "a genuine candle — livequote's midPrice reads a bar's close"},
		{"market.crypto.ingestion_coverage", eventtype.None, "a different message on the same domain"},
		{"market.crypto.volume_profile", eventtype.None, "another different message on the same domain"},
		{"market.crypto.trade.v2", eventtype.None, "a fourth token is not the shape bussink stamps"},
		{"market.trade", eventtype.None, "two tokens: no asset class, so no taxonomy"},
		{"market.", eventtype.None, "a trailing dot is not a variant"},
		{"market", eventtype.None, "the domain alone"},
		{"", eventtype.None, "an envelope with no event_type at all"},
		{"marketing.crypto.trade", eventtype.None, "a domain that merely starts the same way"},
		{"risk.position.changed", eventtype.None, "another domain entirely"},
	} {
		if got := eventtype.Of(tc.eventType); got != tc.want {
			t.Errorf("Of(%q) = %d, want %d — %s", tc.eventType, got, tc.want, tc.why)
		}
	}
}

// Of sits on the per-tick fold of two subscribers to the whole market spine, so
// it must not allocate: an allocation here is one per tick, per fold, forever.
func TestOfDoesNotAllocate(t *testing.T) {
	if n := testing.AllocsPerRun(100, func() {
		sink = eventtype.Of("market.crypto.trade")
	}); n != 0 {
		t.Fatalf("Of allocates %v time(s) per call — it runs once per market tick on both the "+
			"mark fold and the calibration fold", n)
	}
}

// sink defeats the compiler eliminating the call the allocation test measures.
var sink eventtype.Variant
