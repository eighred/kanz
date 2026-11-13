package fxfeed

import (
	"math/big"
	"sync"
	"testing"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func deliverTimeQuote(t *testing.T, live *LiveFX, id string, at time.Time, coefficient int64) error {
	t.Helper()
	return live.Handler(t.Context(), &envelopepb.Envelope{EventType: "market.fx.quote"}, fxQuote(id, decimal(coefficient, -2), decimal(coefficient, -2), at))
}

func requireTimeRate(t *testing.T, live *LiveFX, coefficient int64) {
	t.Helper()
	got, ok := live.Converter().Rate("EUR")
	if !ok || got == nil || got.Cmp(big.NewRat(coefficient, 100)) != 0 {
		t.Fatalf("EUR rate got=%v known=%v want=%d/100", got, ok, coefficient)
	}
}

func TestLiveFXTimeRequiresValidPayloadTimestamp(t *testing.T) {
	for _, tc := range []struct {
		name string
		at   *timestamppb.Timestamp
	}{
		{"missing", nil},
		{"invalid seconds", &timestamppb.Timestamp{Seconds: 253402300800}},
		{"invalid nanos", &timestamppb.Timestamp{Nanos: -1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			live := New("USD", map[string]string{"EURUSD": "EUR"})
			var event marketpb.MarketDataEvent
			if err := proto.Unmarshal(fxQuote("EURUSD", decimal(120, -2), decimal(120, -2)), &event); err != nil {
				t.Fatal(err)
			}
			event.EventTime = tc.at
			payload, err := proto.Marshal(&event)
			if err != nil {
				t.Fatal(err)
			}
			if err := live.Handler(t.Context(), &envelopepb.Envelope{}, payload); err == nil {
				t.Fatal("configured FX accepted a missing or malformed payload event_time")
			}
			if _, ok := live.Converter().Rate("EUR"); ok {
				t.Fatal("an invalid timestamp established a known foreign rate")
			}
			if err := deliverTimeQuote(t, live, "EURUSD", fxFixtureTime, 120); err != nil {
				t.Fatal(err)
			}
			if err := live.Handler(t.Context(), &envelopepb.Envelope{}, payload); err == nil {
				t.Fatal("invalid timestamp replaced an established observation")
			}
			requireTimeRate(t, live, 120)
		})
	}
}

func TestLiveFXTimeConflictRetainsWatermarkUntilNewerValidQuote(t *testing.T) {
	live := New("USD", map[string]string{"EURUSD": "EUR"})
	at := fxFixtureTime
	if err := deliverTimeQuote(t, live, "EURUSD", at, 120); err != nil {
		t.Fatal(err)
	}
	frozen := live.Converter()
	if err := deliverTimeQuote(t, live, "EURUSD", at.Add(-time.Second), 110); err != nil {
		t.Fatal(err)
	}
	requireTimeRate(t, live, 120)
	if err := deliverTimeQuote(t, live, "EURUSD", at, 120); err != nil {
		t.Fatalf("exact replay refused: %v", err)
	}
	if err := live.Handler(t.Context(), &envelopepb.Envelope{}, fxQuote("EURUSD", decimal(12, -1), decimal(12, -1), at)); err != nil {
		t.Fatalf("equivalent exact Decimal encoding refused: %v", err)
	}
	requireTimeRate(t, live, 120)
	if err := deliverTimeQuote(t, live, "EURUSD", at, 125); err == nil {
		t.Fatal("contradictory same-time rate was not refused")
	}
	unknown := live.Converter()
	if _, ok := unknown.Rate("EUR"); ok {
		t.Fatal("a same-time conflict left a clean FX rate")
	}
	if err := deliverTimeQuote(t, live, "EURUSD", at.Add(-time.Second), 110); err != nil {
		t.Fatal(err)
	}
	if err := deliverTimeQuote(t, live, "EURUSD", at, 120); err == nil {
		t.Fatal("same-time replay cleared an unresolved conflict")
	}
	if _, ok := live.Converter().Rate("EUR"); ok {
		t.Fatal("older/repeated clean quote revived an ambiguous currency")
	}
	if err := deliverTimeQuote(t, live, "EURUSD", at.Add(time.Second), 130); err != nil {
		t.Fatal(err)
	}
	requireTimeRate(t, live, 130)
	if rate, ok := frozen.Rate("EUR"); !ok || rate.Cmp(big.NewRat(6, 5)) != 0 {
		t.Fatal("later conflict changed an already-issued converter")
	}
	if _, ok := unknown.Rate("EUR"); ok {
		t.Fatal("later repair changed an already-issued unknown converter")
	}
	if rate, ok := live.Converter().Rate("USD"); !ok || rate.Cmp(big.NewRat(1, 1)) != 0 {
		t.Fatal("reporting currency lost its identity rate")
	}
	if _, ok := live.Converter().Rate("GBP"); ok {
		t.Fatal("unobserved foreign currency became known")
	}
}

func TestLiveFXTimeMultipleConfiguredPairsShareCurrencyWatermark(t *testing.T) {
	live := New("USD", map[string]string{"EURUSD": "EUR", "EURUSD-OTHER": "EUR"})
	if err := deliverTimeQuote(t, live, "EURUSD", fxFixtureTime, 120); err != nil {
		t.Fatal(err)
	}
	if err := deliverTimeQuote(t, live, "EURUSD-OTHER", fxFixtureTime, 120); err != nil {
		t.Fatalf("equivalent configured quote refused: %v", err)
	}
	if err := deliverTimeQuote(t, live, "EURUSD-OTHER", fxFixtureTime, 125); err == nil {
		t.Fatal("a second configured pair silently chose the currency rate")
	}
	if _, ok := live.Converter().Rate("EUR"); ok {
		t.Fatal("contradictory configured pairs did not leave currency unknown")
	}
	if err := deliverTimeQuote(t, live, "EURUSD", fxFixtureTime.Add(time.Second), 130); err != nil {
		t.Fatal(err)
	}
	requireTimeRate(t, live, 130)
}

func TestLiveFXTimePreservesUnusableRateAndUnconfiguredSemantics(t *testing.T) {
	live := New("USD", map[string]string{"EURUSD": "EUR"})
	for _, coefficient := range []int64{0, -1} {
		if err := deliverTimeQuote(t, live, "EURUSD", fxFixtureTime, coefficient); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := live.Converter().Rate("EUR"); ok {
		t.Fatal("nonpositive input supplied a foreign rate")
	}
	if err := deliverTimeQuote(t, live, "EURUSD", fxFixtureTime, 120); err != nil {
		t.Fatal(err)
	}
	if err := deliverTimeQuote(t, live, "EURUSD", fxFixtureTime.Add(time.Hour), 0); err != nil {
		t.Fatal(err)
	}
	requireTimeRate(t, live, 120)
	if err := live.Handler(t.Context(), &envelopepb.Envelope{}, fxQuote("GBPUSD", nil, nil)); err != nil {
		t.Fatalf("unconfigured event changed behavior: %v", err)
	}
	// Preserve the owner's existing one-sided bid fallback and exact mid.
	if err := live.Handler(t.Context(), &envelopepb.Envelope{}, fxQuote("EURUSD", decimal(130, -2), nil, fxFixtureTime.Add(time.Second))); err != nil {
		t.Fatal(err)
	}
	requireTimeRate(t, live, 130)
}

func TestLiveFXTimeConcurrentObservationsAndSnapshots(t *testing.T) {
	live := New("USD", map[string]string{"EURUSD": "EUR"})
	var workers sync.WaitGroup
	for worker := range 8 {
		workers.Go(func() {
			for i := range 32 {
				index := (i + worker) % 32
				if err := deliverTimeQuote(t, live, "EURUSD", fxFixtureTime.Add(time.Duration(index)*time.Nanosecond), int64(120+index)); err != nil {
					t.Error(err)
				}
				if _, ok := live.Converter().Rate("USD"); !ok {
					t.Error("concurrent snapshot lost reporting currency")
				}
			}
		})
	}
	workers.Wait()
	requireTimeRate(t, live, 151)
}
