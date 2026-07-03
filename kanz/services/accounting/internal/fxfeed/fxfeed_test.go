package fxfeed

import (
	"context"
	"math/big"
	"testing"

	"google.golang.org/protobuf/proto"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
	marketpb "github.com/kanz-eng/kanz-schemas-go/market/v1"
)

func decimal(coeff int64, exp int32) *commonpb.Decimal {
	return &commonpb.Decimal{Coefficient: coeff, Exponent: exp}
}

func fxQuote(id string, bid, ask *commonpb.Decimal) []byte {
	ev := &marketpb.MarketDataEvent{
		InstrumentId: id,
		Data:         &marketpb.MarketDataEvent_Quote{Quote: &marketpb.Quote{BidPrice: bid, AskPrice: ask}},
	}
	b, _ := proto.Marshal(ev)
	return b
}

// A folded FX quote mints a converter whose rate is the exact bid/ask mid for
// the pair's foreign currency; the reporting currency is always 1.
func TestLiveFXFoldsQuoteIntoConverter(t *testing.T) {
	l := New("USD", map[string]string{"EURUSD": "EUR"})
	env := &envelopepb.Envelope{EventType: "market.fx.quote"}
	// EUR/USD bid 1.08, ask 1.10 → mid 1.09.
	if err := l.Handler(context.Background(), env, fxQuote("EURUSD", decimal(108, -2), decimal(110, -2))); err != nil {
		t.Fatalf("Handler: %v", err)
	}
	conv := l.Converter()
	rate, ok := conv.Rate("EUR")
	if !ok {
		t.Fatal("no EUR rate after folding a EUR/USD quote")
	}
	if rate.Cmp(big.NewRat(109, 100)) != 0 {
		t.Fatalf("EUR rate = %s, want 1.09", rate.RatString())
	}
	if r, _ := conv.Rate("USD"); r.Cmp(big.NewRat(1, 1)) != 0 {
		t.Fatalf("reporting rate = %s, want 1", r.RatString())
	}
}

// A converter is an immutable snapshot: a tick after it is minted updates the
// cache, not the already-issued converter.
func TestConverterSnapshotIsImmutable(t *testing.T) {
	l := New("USD", map[string]string{"EURUSD": "EUR"})
	env := &envelopepb.Envelope{EventType: "market.fx.quote"}
	_ = l.Handler(context.Background(), env, fxQuote("EURUSD", decimal(108, -2), decimal(108, -2)))
	before := l.Converter()
	_ = l.Handler(context.Background(), env, fxQuote("EURUSD", decimal(120, -2), decimal(120, -2)))

	r, _ := before.Rate("EUR")
	if r.Cmp(big.NewRat(108, 100)) != 0 {
		t.Fatalf("snapshot rate = %s, want the 1.08 it was minted with", r.RatString())
	}
	after, _ := l.Converter().Rate("EUR")
	if after.Cmp(big.NewRat(120, 100)) != 0 {
		t.Fatalf("fresh converter rate = %s, want 1.20", after.RatString())
	}
}

// Events for unconfigured instruments are ignored (the FX stream may carry
// other instruments); an unseen currency has no rate.
func TestLiveFXIgnoresUnconfiguredInstruments(t *testing.T) {
	l := New("USD", map[string]string{"EURUSD": "EUR"})
	env := &envelopepb.Envelope{EventType: "market.fx.quote"}
	if err := l.Handler(context.Background(), env, fxQuote("GBPUSD", decimal(125, -2), decimal(125, -2))); err != nil {
		t.Fatalf("Handler: %v", err)
	}
	if _, ok := l.Converter().Rate("GBP"); ok {
		t.Fatal("GBP should have no rate — GBPUSD is not configured")
	}
}

func TestLiveFXRejectsMalformedPayload(t *testing.T) {
	l := New("USD", map[string]string{"EURUSD": "EUR"})
	env := &envelopepb.Envelope{EventType: "market.fx.quote"}
	if err := l.Handler(context.Background(), env, []byte("not a proto")); err == nil {
		t.Fatal("expected a decode error for a malformed payload")
	}
}

func TestParsePairs(t *testing.T) {
	got, err := ParsePairs(" EURUSD:EUR , GBPUSD:GBP ")
	if err != nil {
		t.Fatalf("ParsePairs: %v", err)
	}
	if len(got) != 2 || got["EURUSD"] != "EUR" || got["GBPUSD"] != "GBP" {
		t.Fatalf("ParsePairs = %v", got)
	}
}

func TestParsePairsEmptyAndMalformed(t *testing.T) {
	if got, err := ParsePairs("  "); err != nil || got != nil {
		t.Fatalf("empty spec: got %v, %v; want nil,nil", got, err)
	}
	for _, spec := range []string{"EURUSD", "EURUSD:EUR:extra", ":EUR", "EURUSD:"} {
		if _, err := ParsePairs(spec); err == nil {
			t.Errorf("expected error for %q", spec)
		}
	}
}
