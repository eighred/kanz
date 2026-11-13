package performance

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/marketdata/store"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
)

// A corrupt reader can expose an absent Decimal even though Memory.Put refuses
// one. The performance boundary must distinguish that from a measured zero.
type pitReader struct {
	store.Store
	history []store.Observation
	err     error
	reads   int
}

func (s *pitReader) History(context.Context, store.Query) ([]store.Observation, error) {
	s.reads++
	return s.history, s.err
}

func TestValuerRefusesIncompletePITMarks(t *testing.T) {
	for _, tc := range []struct {
		name    string
		history []store.Observation
	}{
		{"missing", nil},
		{"absent_decimal", []store.Observation{{Price: nil}}},
		{"unreadable_decimal", []store.Observation{{Price: &commonpb.Decimal{Coefficient: 1, Exponent: 65}}}},
		{"invalid_latest_not_prior_mark", []store.Observation{{Price: px(100)}, {Price: nil}}},
	} {
		for _, quantity := range []float64{10, -3} {
			t.Run(tc.name+holdingSign(quantity), func(t *testing.T) {
				s := &pitReader{history: tc.history}
				v := NewValuer(s, staticPositions{"P": {{InstrumentID: "A", Quantity: quantity}}}, store.PriceKindClose)
				value, err := v.Value(context.Background(), "P", time.Time{}, time.Time{})
				if err == nil || value != 0 {
					t.Fatalf("unusable mark produced a clean value: value=%v err=%v", value, err)
				}
			})
		}
	}
	// A priced lot must not conceal a nonzero lot missing from the store.
	s := seedStore(t)
	v := NewValuer(s, staticPositions{"P": {{InstrumentID: "AAA", Quantity: 10}, {InstrumentID: "MISSING", Quantity: -3}}}, store.PriceKindClose)
	if value, err := v.Value(context.Background(), "P", time.Time{}, time.Time{}); err == nil || value != 0 {
		t.Fatalf("partial portfolio escaped: value=%v err=%v", value, err)
	}
}

func holdingSign(quantity float64) string {
	if quantity < 0 {
		return "/short"
	}
	return "/long"
}

func TestValuerDistinguishesKnownZeroAndFlatHoldings(t *testing.T) {
	for _, quantity := range []float64{10, -3} {
		t.Run("zero_mark"+holdingSign(quantity), func(t *testing.T) {
			s := &pitReader{history: []store.Observation{{Price: px(0)}}}
			value, err := NewValuer(s, staticPositions{"P": {{InstrumentID: "A", Quantity: quantity}}}, store.PriceKindClose).Value(context.Background(), "P", time.Time{}, time.Time{})
			if err != nil || value != 0 || s.reads != 1 {
				t.Fatalf("measured zero must remain known: value=%v err=%v reads=%d", value, err, s.reads)
			}
		})
	}
	s := &pitReader{err: errors.New("a flat holding needs no mark")}
	value, err := NewValuer(s, staticPositions{"P": {{InstrumentID: "A", Quantity: 0}}}, store.PriceKindClose).Value(context.Background(), "P", time.Time{}, time.Time{})
	if err != nil || value != 0 || s.reads != 0 {
		t.Fatalf("flat holding required a mark: value=%v err=%v reads=%d", value, err, s.reads)
	}
}

func TestValuerRefusesNonfiniteInputsAndArithmetic(t *testing.T) {
	for _, tc := range []struct {
		name       string
		quantities []float64
		price      int64
	}{
		{"nan_quantity", []float64{math.NaN()}, 0},
		{"positive_infinite_quantity", []float64{math.Inf(1)}, 0},
		{"negative_infinite_quantity", []float64{math.Inf(-1)}, 0},
		{"positive_product_overflow", []float64{math.MaxFloat64}, 2},
		{"negative_product_overflow", []float64{-math.MaxFloat64}, 2},
		{"positive_total_overflow", []float64{math.MaxFloat64, math.MaxFloat64}, 1},
		{"negative_total_overflow", []float64{-math.MaxFloat64, -math.MaxFloat64}, 1},
		{"overflow_before_later_offset", []float64{math.MaxFloat64, math.MaxFloat64, -math.MaxFloat64}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			holdings := make([]Holding, len(tc.quantities))
			for i, q := range tc.quantities {
				holdings[i] = Holding{InstrumentID: "A", Quantity: q}
			}
			s := &pitReader{history: []store.Observation{{Price: px(tc.price)}}}
			value, err := NewValuer(s, staticPositions{"P": holdings}, store.PriceKindClose).Value(context.Background(), "P", time.Time{}, time.Time{})
			if err == nil || value != 0 {
				t.Fatalf("nonfinite valuation escaped: value=%v err=%v", value, err)
			}
		})
	}
}

func TestValuerSignedPITConservationAndKnowledge(t *testing.T) {
	ctx := context.Background()
	day := func(d int) time.Time { return time.Date(2024, 1, d, 0, 0, 0, 0, time.UTC) }
	s := store.NewMemory()
	if err := s.Put(ctx, []store.Observation{
		{InstrumentID: "A", ObservationTime: day(1), KnowledgeTime: day(1), Price: px(100), Kind: store.PriceKindClose},
		{InstrumentID: "B", ObservationTime: day(1), KnowledgeTime: day(1), Price: px(20), Kind: store.PriceKindClose},
		{InstrumentID: "A", ObservationTime: day(1), KnowledgeTime: day(20), Price: px(110), Kind: store.PriceKindClose},
	}); err != nil {
		t.Fatal(err)
	}
	v := NewValuer(s, staticPositions{"P": {{InstrumentID: "A", Quantity: 10}, {InstrumentID: "B", Quantity: -3}}}, store.PriceKindClose)
	// Integer oracles: 10*100 - 3*20 = 940; after correction, 10*110 - 3*20 = 1040.
	for _, tc := range []struct {
		name string
		asOf time.Time
		want float64
	}{
		{"before_correction", day(10), 940},
		{"correction_known", day(20), 1040},
		{"latest_knowledge", time.Time{}, 1040},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := v.Value(ctx, "P", day(10), tc.asOf)
			if err != nil || got != tc.want {
				t.Fatalf("value=%v want=%v err=%v", got, tc.want, err)
			}
		})
	}
	series, err := v.ValueSeries(ctx, "P", []time.Time{day(10), day(1)}, day(10))
	if err != nil || len(series) != 2 || series[0] != 940 || series[1] != 940 {
		t.Fatalf("PIT series=%v err=%v", series, err)
	}
	// The earlier date has no mark: do not expose a partial series.
	if partial, err := v.ValueSeries(ctx, "P", []time.Time{day(10), day(1).Add(-time.Nanosecond)}, day(10)); err == nil || partial != nil {
		t.Fatalf("incomplete series escaped: series=%v err=%v", partial, err)
	}
}

func TestPITPriceDistinguishesMissingZeroAndUnreadable(t *testing.T) {
	for _, tc := range []struct {
		name    string
		history []store.Observation
		wantOK  bool
		wantErr bool
	}{
		{"missing", nil, false, false},
		{"explicit_zero", []store.Observation{{Price: px(0)}}, true, false},
		{"absent_decimal", []store.Observation{{Price: nil}}, false, true},
		{"extreme_positive_exponent", []store.Observation{{Price: &commonpb.Decimal{Coefficient: 1, Exponent: 1000}}}, false, true},
		{"extreme_negative_exponent", []store.Observation{{Price: &commonpb.Decimal{Coefficient: 1, Exponent: -1000}}}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok, err := pointInTimePrice(context.Background(), &pitReader{history: tc.history}, "A", store.PriceKindClose, time.Time{}, time.Time{})
			if got != 0 || ok != tc.wantOK || (err != nil) != tc.wantErr {
				t.Fatalf("price=%v ok=%v err=%v", got, ok, err)
			}
		})
	}
}

func TestInstrumentReturnRefusesUnreadableEndpoint(t *testing.T) {
	ctx := context.Background()
	day := func(d int) time.Time { return time.Date(2024, 1, d, 0, 0, 0, 0, time.UTC) }
	for _, badAt := range []int{1, 10} {
		for _, exponent := range []int32{-1000, 65, 1000} {
			t.Run(fmt.Sprintf("day%d/exponent%d", badAt, exponent), func(t *testing.T) {
				s := store.NewMemory()
				observations := []store.Observation{
					{InstrumentID: "A", ObservationTime: day(1), KnowledgeTime: day(1), Price: px(100), Kind: store.PriceKindClose},
					{InstrumentID: "A", ObservationTime: day(10), KnowledgeTime: day(10), Price: px(110), Kind: store.PriceKindClose},
				}
				idx := 0
				if badAt == 10 {
					idx = 1
				}
				observations[idx].Price = &commonpb.Decimal{Coefficient: 1, Exponent: exponent}
				if err := s.Put(ctx, observations); err != nil {
					t.Fatal(err)
				}
				if value, ok, err := InstrumentReturn(ctx, s, "A", store.PriceKindClose, day(1), day(10), day(10)); ok || err == nil || value != 0 {
					t.Fatalf("unreadable endpoint day=%d exp=%d produced return=%v ok=%v err=%v", badAt, exponent, value, ok, err)
				}
			})
		}
	}
	if value, ok, err := InstrumentReturn(ctx, store.NewMemory(), "A", store.PriceKindClose, day(1), day(10), day(10)); value != 0 || ok || err != nil {
		t.Fatalf("missing endpoint contract changed: value=%v ok=%v err=%v", value, ok, err)
	}
}

func TestInstrumentReturnPreservesUsableEndpoints(t *testing.T) {
	ctx := context.Background()
	day := func(d int) time.Time { return time.Date(2024, 1, d, 0, 0, 0, 0, time.UTC) }
	for _, tc := range []struct {
		name       string
		start, end *commonpb.Decimal
		want       float64
		wantOK     bool
	}{
		{"zero_end", px(100), px(0), -1, true},
		{"zero_start", px(0), px(100), 0, false},
		{"finite_signed_prices", px(-100), px(-110), 0.1, true},
		{"tiny_prices", &commonpb.Decimal{Coefficient: 1, Exponent: -12}, &commonpb.Decimal{Coefficient: 2, Exponent: -12}, 1, true},
		{"safe_domain_extremes", &commonpb.Decimal{Coefficient: 1, Exponent: -64}, &commonpb.Decimal{Coefficient: 1, Exponent: 64}, 1e128, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := store.NewMemory()
			if err := s.Put(ctx, []store.Observation{
				{InstrumentID: "A", ObservationTime: day(1), KnowledgeTime: day(1), Price: tc.start, Kind: store.PriceKindClose},
				{InstrumentID: "A", ObservationTime: day(10), KnowledgeTime: day(10), Price: tc.end, Kind: store.PriceKindClose},
			}); err != nil {
				t.Fatal(err)
			}
			got, ok, err := InstrumentReturn(ctx, s, "A", store.PriceKindClose, day(1), day(10), day(10))
			// Allow binary64 rounding with 1e-15 relative tolerance and an
			// absolute 1e-15 floor near zero; the integer valuation oracle is exact.
			tolerance := 1e-15 * math.Max(1, math.Abs(tc.want))
			if err != nil || ok != tc.wantOK || math.IsNaN(got) || math.IsInf(got, 0) || math.Abs(got-tc.want) > tolerance {
				t.Fatalf("return=%v ok=%v err=%v want=%v ok=%v", got, ok, err, tc.want, tc.wantOK)
			}
		})
	}
}

func TestPITReadErrorsRemainIdentifiable(t *testing.T) {
	want := context.Canceled
	s := &pitReader{err: want}
	v := NewValuer(s, staticPositions{"P": {{InstrumentID: "A", Quantity: 1}}}, store.PriceKindClose)
	if value, err := v.Value(context.Background(), "P", time.Time{}, time.Time{}); value != 0 || !errors.Is(err, want) {
		t.Fatalf("valuation lost read error: value=%v err=%v", value, err)
	}
	if value, ok, err := InstrumentReturn(context.Background(), s, "A", store.PriceKindClose, time.Time{}, time.Time{}, time.Time{}); value != 0 || ok || !errors.Is(err, want) {
		t.Fatalf("return lost read error: value=%v ok=%v err=%v", value, ok, err)
	}
}
