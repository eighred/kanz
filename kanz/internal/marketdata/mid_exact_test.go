package marketdata

import (
	"context"
	"math"
	"math/big"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/marketdata/store"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"
)

func midpointQuote(id string, bid, ask *commonpb.Decimal) *marketpb.MarketDataEvent {
	return &marketpb.MarketDataEvent{InstrumentId: id, EventTime: timestamppb.New(evtTime),
		Data: &marketpb.MarketDataEvent_Quote{Quote: &marketpb.Quote{BidPrice: bid, AskPrice: ask}}}
}

type midpointCase struct {
	name     string
	bid, ask *commonpb.Decimal
	want     string
}

func exactMidpointCases() []midpointCase {
	return []midpointCase{
		{"equal redundant scale", decv(2_000_000_000_000_000_000, -18), decv(2_000_000_000_000_000_000, -18), "2"},
		{"equal compact scale", decv(2, 0), decv(2, 0), "2"},
		{"mixed scales", decv(155, -2), decv(1560, -3), "311/200"},
		{"mixed large scales", decv(1, 19), decv(1_000_000_000_000_000_000, 1), "10000000000000000000"},
		{"alignment overflow cancels exactly", decv(1, 19), decv(-8_000_000_000_000_000_000, 0), "1000000000000000000"},
		{"largest coefficient", decv(math.MaxInt64, 0), decv(math.MaxInt64, 0), "9223372036854775807"},
		{"large magnitude", decv(math.MaxInt64, 60), decv(math.MaxInt64, 60), "9223372036854775807" + strings.Repeat("0", 60)},
		{"tiny exact lower boundary", decv(1, -60), decv(1, -60), "1/1" + strings.Repeat("0", 60)},
		{"upper exact boundary", decv(1, 60), decv(1, 60), "1" + strings.Repeat("0", 60)},
		{"inside lower boundary", decv(1, -59), decv(1, -59), "1/1" + strings.Repeat("0", 59)},
		{"inside upper boundary", decv(1, 59), decv(1, 59), "1" + strings.Repeat("0", 59)},
		{"alternate small representation", decv(10, -61), decv(10, -61), "1/1" + strings.Repeat("0", 60)},
		{"lower ingress cancels exactly", decv(1, -64), decv(-1, -64), "0"},
		{"upper ingress cancels exactly", decv(1, 64), decv(-1, 64), "0"},
		{"negative signed mean", decv(-2_000_000_000_000_000_000, -18), decv(-2_000_000_000_000_000_000, -18), "-2"},
		{"reported zero", decv(0, 0), decv(0, 0), "0"},
	}
}

func TestMIDPreservesEveryDigitOfSupportedExactValues(t *testing.T) {
	for _, tc := range exactMidpointCases() {
		t.Run(tc.name, func(t *testing.T) {
			ev := midpointQuote("MID", tc.bid, tc.ask)
			before := proto.Clone(ev)
			obs, err := TranslateEvent(env("market.crypto.quote"), ev)
			if err != nil {
				t.Fatal(err)
			}
			want, ok := new(big.Rat).SetString(tc.want)
			if !ok {
				t.Fatal("invalid independent rational fixture")
			}
			got, ok := dec.FromProtoChecked(obs.Price)
			if !ok || got.Cmp(want) != 0 || obs.Kind != store.PriceKindMid {
				t.Fatalf("MID=%v, want exact %s; observation=%+v", got, want, obs)
			}
			if !proto.Equal(ev, before) {
				t.Fatal("midpoint arithmetic mutated the quote")
			}
		})
	}
}

func TestMIDRefusesUnsupportedExactEncodingBeforeWriting(t *testing.T) {
	for _, tc := range []struct {
		name     string
		bid, ask *commonpb.Decimal
	}{
		{"coefficient cannot encode half unit", decv(math.MaxInt64-1, 0), decv(math.MaxInt64, 0)},
		{"below exact exponent boundary", decv(1, -61), decv(1, -61)},
		{"above exact exponent boundary", decv(1, 61), decv(1, 61)},
		// The ingress computational bound is wider than ToProtoExact's
		// normalized output contract. These values are mathematically valid
		// Decimals; this existing exact encoder cannot emit them.
		{"ingress lower boundary", decv(1, -64), decv(1, -64)},
		{"ingress upper boundary", decv(1, 64), decv(1, 64)},
		{"alternate high representation", decv(10, 60), decv(10, 60)},
		{"midpoint needs exponent below boundary", decv(1, -60), decv(2, -60)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &fakeWriter{}
			ing, err := NewIngestor(w)
			if err != nil {
				t.Fatal(err)
			}
			payload, err := proto.Marshal(midpointQuote("MID", tc.bid, tc.ask))
			if err != nil {
				t.Fatal(err)
			}
			err = ing.Handler(context.Background(), env("market.crypto.quote"), payload)
			if err == nil || !strings.Contains(err.Error(), "unsupported exact MID encoding") {
				t.Fatalf("unsupported midpoint=%v, want explicit exact-encoding refusal", err)
			}
			if len(w.got) != 0 || len(w.bars) != 0 || len(w.cov) != 0 {
				t.Fatal("unsupported midpoint reached a writer")
			}
		})
	}
}

func TestMIDMissingAndOutOfDomainInputsNeverBecomeZero(t *testing.T) {
	for _, tc := range []struct {
		name     string
		bid, ask *commonpb.Decimal
	}{
		{"missing bid", nil, decv(2, 0)},
		{"missing ask", decv(2, 0), nil},
		{"both missing", nil, nil},
		{"outside lower ingress boundary", decv(1, -65), decv(1, -65)},
		{"outside upper ingress boundary", decv(1, 65), decv(1, 65)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev := midpointQuote("MID", tc.bid, tc.ask)
			if obs, err := TranslateEvent(env("market.crypto.quote"), ev); err == nil || obs.Price != nil {
				t.Fatalf("missing/unusable quote became a midpoint: %+v, err=%v", obs, err)
			}
			w := &fakeWriter{}
			ing, err := NewIngestor(w)
			if err != nil {
				t.Fatal(err)
			}
			payload, err := proto.Marshal(ev)
			if err != nil {
				t.Fatal(err)
			}
			if err := ing.Handler(context.Background(), env("market.crypto.quote"), payload); err == nil {
				t.Fatal("missing/unusable quote was acknowledged")
			}
			if len(w.got) != 0 || len(w.bars) != 0 || len(w.cov) != 0 {
				t.Fatal("missing/unusable quote reached a writer")
			}
		})
	}
}
