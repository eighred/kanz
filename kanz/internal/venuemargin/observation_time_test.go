package venuemargin

import (
	"testing"
	"time"

	collateralpb "github.com/eighred/kanz/kanz-schemas-go/collateral/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func malformedMarginTimes() map[string]*timestamppb.Timestamp {
	return map[string]*timestamppb.Timestamp{
		"negative_nanos":  {Seconds: observed.Unix(), Nanos: -1},
		"overflow_nanos":  {Seconds: observed.Unix() - 1, Nanos: 1_000_000_000},
		"before_year_one": {Seconds: -62135596801},
		"after_year_9999": {Seconds: 253402300800},
		"minimum_seconds": {Seconds: -1 << 63},
		"maximum_seconds": {Seconds: 1<<63 - 1},
	}
}

// Assert every public reader: a complete coverage claim or a flat liquidation
// list must not remain usable when the numeric lookups correctly say UNKNOWN.
func assertMarginLevel(t *testing.T, v *View, want *collateralpb.VenueMarginState) {
	t.Helper()
	for name, read := range map[string]func() (Quantity, bool){
		"maintenance": func() (Quantity, bool) { return v.MaintenanceMargin("OKX", "acct-1") },
		"ratio":       func() (Quantity, bool) { return v.MarginRatio("OKX", "acct-1") },
		"liquidation": func() (Quantity, bool) { return v.LiquidationPrice("OKX", "acct-1", "BTC-USDT-SWAP") },
	} {
		q, ok := read()
		if want == nil {
			if ok || q.Value() != nil || !q.ObservedAt().IsZero() {
				t.Errorf("%s became usable without a valid observation: %+v", name, q)
			}
			continue
		}
		values := map[string]string{"maintenance": "1250.5", "ratio": "3.75", "liquidation": "41000"}
		if !ok || q.Value() == nil || q.Value().Cmp(rat(t, values[name])) != 0 || !q.ObservedAt().Equal(want.ObservedAt.AsTime()) {
			t.Errorf("%s lost the original level/time: value=%v time=%s ok=%v", name, q.Value(), q.ObservedAt(), ok)
		}
	}
	cov, covered := v.Coverage("OKX", "acct-1")
	positions, positionCoverage, positionsKnown := v.Liquidations("OKX", "acct-1")
	if want == nil {
		if covered || cov.Reported() || positionsKnown || positionCoverage.Reported() || len(positions) != 0 {
			t.Errorf("unknown account exposed coverage/liquidations: %+v %v %+v %v", cov, covered, positions, positionsKnown)
		}
		return
	}
	if !covered || !cov.Complete() || !positionsKnown || !positionCoverage.Complete() || len(positions) != 1 {
		t.Fatalf("original coverage/liquidations changed: %+v %v %+v %v", cov, covered, positions, positionsKnown)
	}
	if positions[0].VenueSymbol != "BTC-USDT-SWAP" || positions[0].InstrumentID != want.LiquidationPrices[0].InstrumentId || positions[0].Price.Value().Cmp(rat(t, "41000")) != 0 || !positions[0].Price.ObservedAt().Equal(want.ObservedAt.AsTime()) {
		t.Errorf("original liquidation attribution/value/time changed: %+v", positions[0])
	}
}

func TestMalformedMarginObservationTimeCannotReplaceAccountState(t *testing.T) {
	for name, stamp := range malformedMarginTimes() {
		t.Run(name, func(t *testing.T) {
			if stamp.CheckValid() == nil {
				t.Fatal("fixture must be outside the protobuf timestamp domain")
			}
			for _, seeded := range []bool{false, true} {
				label := "unobserved"
				if seeded {
					label = "prior_level"
				}
				t.Run(label, func(t *testing.T) {
					now := frozen()
					undated, uncovered := 0, 0
					v := New(WithClock(func() time.Time { return now }),
						WithOnUndated(func(string, string) { undated++ }),
						WithOnUncovered(func(string, string, uint32) { uncovered++ }))
					var prior *collateralpb.VenueMarginState
					if seeded {
						prior = state(t)
						prior.LiquidationPrices[0].InstrumentId = "BTC-PERP"
						fold(t, v, prior)
					}
					bad := state(t)
					bad.ObservedAt = stamp
					bad.MaintenanceMargin = decimal(t, "0")
					bad.MarginRatio = decimal(t, "999")
					bad.LiquidationPrices = nil
					bad.Coverage.ExcludedCount = 1
					fold(t, v, bad) // Permanent payload refusal keeps the ACK/discard contract.
					assertMarginLevel(t, v, prior)
					if undated != 1 || uncovered != 0 {
						t.Fatalf("callbacks: undated=%d uncovered=%d, want rejection only", undated, uncovered)
					}
					now = observed.Add(DefaultMaxAge + time.Nanosecond)
					assertMarginLevel(t, v, nil)
					recovered := state(t)
					recovered.ObservedAt = timestamppb.New(now)
					fold(t, v, recovered)
					assertMarginLevel(t, v, recovered)
				})
			}
		})
	}
}

func TestMarginObservationTimePreservesAbsentAndValidTimePolicies(t *testing.T) {
	for name, stamp := range map[string]*timestamppb.Timestamp{
		"absent": nil, "year_one": timestamppb.New(time.Time{}),
	} {
		t.Run(name, func(t *testing.T) {
			undated := 0
			v := New(WithClock(frozen), WithOnUndated(func(string, string) { undated++ }))
			msg := state(t)
			msg.ObservedAt = stamp
			fold(t, v, msg)
			assertMarginLevel(t, v, nil)
			if undated != 1 {
				t.Fatalf("undated callback = %d, want 1", undated)
			}
		})
	}
	for name, stamp := range map[string]*timestamppb.Timestamp{
		"epoch": {}, "epoch_nanos": {Nanos: 1},
		"negative_epoch_nanos": {Seconds: -1, Nanos: 999_999_999},
		"year_one_nanos":       {Seconds: -62135596800, Nanos: 1},
		"last_valid_instant":   {Seconds: 253402300799, Nanos: 999_999_999},
	} {
		t.Run(name, func(t *testing.T) {
			if err := stamp.CheckValid(); err != nil {
				t.Fatal(err)
			}
			v := New(WithClock(func() time.Time { return stamp.AsTime().Add(time.Second) }))
			msg := state(t)
			msg.ObservedAt = stamp
			fold(t, v, msg)
			assertMarginLevel(t, v, msg)
		})
	}
}
