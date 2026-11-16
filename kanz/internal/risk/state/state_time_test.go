package state_test

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/state"
	"github.com/eighred/kanz/internal/risk/state/persist"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Each form reaches a different conversion/reset branch in the state owner.
// The last-position case puts valid replacement data before the bad entry.
var stateTimeForms = []string{"portfolio", "position", "snapshot-portfolio", "snapshot-first-position", "snapshot-last-position"}

func applyStateTime(s *state.Store, form, portfolio, key string, ts *timestamppb.Timestamp) error {
	p := &domainpb.PortfolioState{PortfolioId: portfolio, DisplayName: "replacement", BaseCurrency: "USD", CashBalance: money(91, "USD"), PositionCount: 2, AsOf: offset(time.Hour)}
	a := &domainpb.PositionState{PortfolioId: portfolio, InstrumentId: "A", Quantity: &commonpb.Decimal{Coefficient: 7}, AveragePrice: &commonpb.Decimal{Coefficient: 10}, MarketValue: money(70, "USD"), AsOf: offset(time.Hour)}
	b := &domainpb.PositionState{PortfolioId: portfolio, InstrumentId: "B", Quantity: &commonpb.Decimal{Coefficient: 3}, AveragePrice: &commonpb.Decimal{Coefficient: 10}, MarketValue: money(30, "USD"), AsOf: offset(time.Hour)}
	switch form {
	case "portfolio":
		p.AsOf = ts
		return s.ApplyPortfolioRevalued(context.Background(), env(key), p)
	case "position":
		a.AsOf = ts
		return s.ApplyPositionChanged(context.Background(), env(key), a)
	case "snapshot-portfolio":
		p.AsOf = ts
	case "snapshot-first-position":
		a.AsOf = ts
	case "snapshot-last-position":
		b.AsOf = ts
	default:
		panic("unknown state time fixture")
	}
	return s.ApplyPortfolioSnapshot(context.Background(), env(key), &domainpb.PortfolioSnapshot{Portfolio: p, Positions: []*domainpb.PositionState{a, b}, LogPosition: &commonpb.LogPosition{Topic: "risk.portfolio.snapshot", Offset: 123}})
}

func stateTimeRecord(t *testing.T, s *state.Store, id string) persist.PortfolioRecord {
	t.Helper()
	p, keys, ok := s.SnapshotWithKeys(v1.PortfolioID(id))
	if !ok {
		t.Fatal("missing fixture portfolio")
	}
	return persist.FromPortfolio(p, keys)
}

func seedStateTime(t *testing.T, s *state.Store, id string) {
	t.Helper()
	if err := s.ApplyPortfolioRevalued(context.Background(), env("seed-aggregate"), &domainpb.PortfolioState{PortfolioId: id, DisplayName: "original", BaseCurrency: "USD", CashBalance: money(11, "USD"), AsOf: offset(0)}); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyPositionChanged(context.Background(), env("seed-position"), &domainpb.PositionState{PortfolioId: id, InstrumentId: "OLD", Quantity: &commonpb.Decimal{Coefficient: 5}, AveragePrice: &commonpb.Decimal{Coefficient: 10}, MarketValue: money(50, "USD"), AsOf: offset(0)}); err != nil {
		t.Fatal(err)
	}
}

func TestStateTimeRefusalPrecedesCreationMutationAndDedup(t *testing.T) {
	bad := map[string]*timestamppb.Timestamp{
		"seconds-before-year-one": {Seconds: -62135596801},
		"seconds-after-year-9999": {Seconds: 253402300800},
		"negative-nanos":          {Seconds: baseTime.Unix(), Nanos: -1},
		"overflow-nanos":          {Seconds: baseTime.Unix(), Nanos: 1_000_000_000},
	}
	for _, form := range stateTimeForms {
		for name, ts := range bad {
			t.Run(form+"/"+name, func(t *testing.T) {
				for _, existing := range []bool{false, true} {
					s := state.NewStore()
					var before persist.PortfolioRecord
					if existing {
						seedStateTime(t, s, "PF")
						before = stateTimeRecord(t, s, "PF")
					}
					if err := applyStateTime(s, form, "PF", "retry-key", ts); err == nil || !strings.Contains(err.Error(), "as_of") {
						t.Fatalf("existing=%v: invalid time accepted or unidentified: %v", existing, err)
					}
					if existing {
						if got := stateTimeRecord(t, s, "PF"); !reflect.DeepEqual(got, before) {
							t.Fatal("rejected input changed state, snapshot coordinate or applied keys")
						}
					} else if len(s.IDs()) != 0 {
						t.Fatal("rejected input lazy-created a portfolio")
					}
					if err := applyStateTime(s, form, "PF", "retry-key", offset(time.Hour)); err != nil {
						t.Fatalf("valid repair with the same key failed: %v", err)
					}
					after := stateTimeRecord(t, s, "PF")
					if !after.AsOf.Equal(baseTime.Add(time.Hour)) {
						t.Fatal("failed apply poisoned dedup; valid repair was not applied")
					}
					if err := applyStateTime(s, form, "PF", "retry-key", ts); err == nil {
						t.Fatal("a successful key hid malformed duplicate bytes")
					}
					if got := stateTimeRecord(t, s, "PF"); !reflect.DeepEqual(got, after) {
						t.Fatal("malformed duplicate changed repaired state")
					}
					if err := applyStateTime(s, form, "PF", "retry-key", offset(2*time.Hour)); err != nil {
						t.Fatal(err)
					}
					if got := stateTimeRecord(t, s, "PF"); !reflect.DeepEqual(got, after) {
						t.Fatal("valid duplicate changed state")
					}
				}
			})
		}
	}
}

func TestStateTimePreservesAbsentAndValidInstants(t *testing.T) {
	valid := map[string]*timestamppb.Timestamp{
		"absent":          nil,
		"year-one":        {Seconds: -62135596800},
		"epoch":           {},
		"nanoseconds":     {Seconds: baseTime.Unix(), Nanos: 123456789},
		"last-nanosecond": {Seconds: 253402300799, Nanos: 999999999},
	}
	for _, form := range stateTimeForms {
		for name, ts := range valid {
			t.Run(form+"/"+name, func(t *testing.T) {
				s := state.NewStore()
				if err := applyStateTime(s, form, "PF", "valid", ts); err != nil {
					t.Fatal(err)
				}
				want := time.Time{}
				if ts != nil {
					want = ts.AsTime()
				}
				got := stateTimeRecord(t, s, "PF")
				actual := got.AsOf
				if form == "position" || strings.Contains(form, "position") {
					instrument := "A"
					if form == "snapshot-last-position" {
						instrument = "B"
					}
					for _, p := range got.Positions {
						if string(p.InstrumentID) == instrument {
							actual = p.AsOf
						}
					}
				}
				if !actual.Equal(want) {
					t.Fatalf("instant=%v want %v", actual, want)
				}
			})
		}
	}
}

func TestStateTimeValidatesNestedTimesBeforeStaleSnapshotSkip(t *testing.T) {
	s := state.NewStore()
	seedStateTime(t, s, "PF")
	before := stateTimeRecord(t, s, "PF")
	p := &domainpb.PortfolioSnapshot{Portfolio: &domainpb.PortfolioState{PortfolioId: "PF", AsOf: offset(-time.Hour)}, Positions: []*domainpb.PositionState{{PortfolioId: "PF", InstrumentId: "STALE", AsOf: &timestamppb.Timestamp{Nanos: -1}}}}
	if err := s.ApplyPortfolioSnapshot(context.Background(), env("stale"), p); err == nil {
		t.Fatal("stale snapshot hid malformed nested time")
	}
	if got := stateTimeRecord(t, s, "PF"); !reflect.DeepEqual(got, before) {
		t.Fatal("invalid stale snapshot changed state or dedup")
	}
	p.Positions[0].AsOf = offset(-time.Hour)
	if err := s.ApplyPortfolioSnapshot(context.Background(), env("stale"), p); err != nil {
		t.Fatal(err)
	}
	got := stateTimeRecord(t, s, "PF")
	got.AppliedKeys = before.AppliedKeys
	if !reflect.DeepEqual(got, before) {
		t.Fatal("valid stale snapshot no longer preserves current book")
	}
}
