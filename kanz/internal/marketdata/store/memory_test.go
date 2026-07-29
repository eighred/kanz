package store

import (
	"context"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
)

var day0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func day(n int) time.Time { return day0.AddDate(0, 0, n) }

func dec(coef int64, exp int32) *commonpb.Decimal {
	return &commonpb.Decimal{Coefficient: coef, Exponent: exp}
}

func obs(inst string, obsDay int, price *commonpb.Decimal, kind PriceKind, knowDay int) Observation {
	return Observation{
		InstrumentID:    inst,
		ObservationTime: day(obsDay),
		Price:           price,
		Kind:            kind,
		KnowledgeTime:   day(knowDay),
	}
}

func mustPut(t *testing.T, s Store, o ...Observation) {
	t.Helper()
	if err := s.Put(context.Background(), o); err != nil {
		t.Fatalf("Put: %v", err)
	}
}

// TestMemory_NoFutureLeakage is the MODEL-01i load-bearing property: a
// correction that arrived after the query's knowledge horizon must not leak
// into the historical read. A backtest as of T sees only what was knowable at T.
func TestMemory_NoFutureLeakage(t *testing.T) {
	s := NewMemory()
	// Original close for day 1, known on day 1; corrected on day 3.
	mustPut(t,
		s,
		obs("AAPL", 1, dec(15000, -2), PriceKindClose, 1), // 150.00 known day 1
		obs("AAPL", 1, dec(15500, -2), PriceKindClose, 3), // 155.00 restated day 3
	)

	// As of day 2: only the original is known.
	got, err := s.History(context.Background(), Query{InstrumentID: "AAPL", AsOf: day(2)})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 observation as of day 2, got %d", len(got))
	}
	if got[0].Price.Coefficient != 15000 {
		t.Fatalf("as of day 2 want 150.00 (pre-correction), got coef %d", got[0].Price.Coefficient)
	}

	// As of day 4: the correction is now visible and collapses the date to it.
	got, err = s.History(context.Background(), Query{InstrumentID: "AAPL", AsOf: day(4)})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Price.Coefficient != 15500 {
		t.Fatalf("as of day 4 want single 155.00, got %+v", got)
	}
}

// TestMemory_LiveReadCollapsesToLatestKnowledge: a zero AsOf (live read) returns
// the freshest restatement per date, one row per (observation_time, kind).
func TestMemory_LiveReadCollapsesToLatestKnowledge(t *testing.T) {
	s := NewMemory()
	mustPut(t,
		s,
		obs("AAPL", 1, dec(100, 0), PriceKindClose, 1),
		obs("AAPL", 1, dec(101, 0), PriceKindClose, 5), // restatement
		obs("AAPL", 2, dec(102, 0), PriceKindClose, 2),
	)
	got, err := s.History(context.Background(), Query{InstrumentID: "AAPL"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 dates, got %d (%+v)", len(got), got)
	}
	// Ordered ascending by observation_time; day 1 collapsed to the day-5 value.
	if !got[0].ObservationTime.Equal(day(1)) || got[0].Price.Coefficient != 101 {
		t.Fatalf("day 1 want latest-known 101, got %+v", got[0])
	}
	if !got[1].ObservationTime.Equal(day(2)) {
		t.Fatalf("want day 2 second, got %v", got[1].ObservationTime)
	}
}

func TestMemory_History_WindowAndKind(t *testing.T) {
	s := NewMemory()
	mustPut(t,
		s,
		obs("AAPL", 1, dec(100, 0), PriceKindClose, 1),
		obs("AAPL", 2, dec(101, 0), PriceKindClose, 2),
		obs("AAPL", 3, dec(102, 0), PriceKindClose, 3),
		obs("AAPL", 2, dec(999, 0), PriceKindLast, 2), // different kind, same day
	)
	got, err := s.History(context.Background(), Query{
		InstrumentID: "AAPL",
		Kind:         PriceKindClose,
		Start:        day(2),
		End:          day(3),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 (close, day 2-3), got %d (%+v)", len(got), got)
	}
	for _, o := range got {
		if o.Kind != PriceKindClose {
			t.Fatalf("kind filter leaked: %+v", o)
		}
		if o.ObservationTime.Before(day(2)) || o.ObservationTime.After(day(3)) {
			t.Fatalf("window leaked: %v", o.ObservationTime)
		}
	}
}

func TestMemory_LatestAsOf(t *testing.T) {
	s := NewMemory()
	mustPut(t,
		s,
		obs("AAPL", 1, dec(100, 0), PriceKindClose, 1),
		obs("AAPL", 3, dec(103, 0), PriceKindClose, 3),
		obs("AAPL", 5, dec(105, 0), PriceKindClose, 5),
	)
	// As of day 4: latest observation_time <= day 4 is day 3.
	o, ok, err := s.LatestAsOf(context.Background(), "AAPL", PriceKindClose, day(4))
	if err != nil || !ok {
		t.Fatalf("want hit, got ok=%v err=%v", ok, err)
	}
	if !o.ObservationTime.Equal(day(3)) || o.Price.Coefficient != 103 {
		t.Fatalf("want day-3 103, got %+v", o)
	}

	// As of day 0: nothing known yet.
	if _, ok, _ := s.LatestAsOf(context.Background(), "AAPL", PriceKindClose, day(0)); ok {
		t.Fatal("want miss before any observation")
	}

	// A correction known only later must not surface in an earlier as-of read.
	mustPut(t, s, obs("AAPL", 3, dec(199, 0), PriceKindClose, 9))
	o, _, _ = s.LatestAsOf(context.Background(), "AAPL", PriceKindClose, day(4))
	if o.Price.Coefficient != 103 {
		t.Fatalf("as of day 4 must not see the day-9 correction, got %+v", o)
	}
}

func TestMemory_Put_Idempotent(t *testing.T) {
	s := NewMemory()
	o := obs("AAPL", 1, dec(100, 0), PriceKindClose, 1)
	mustPut(t, s, o)
	mustPut(t, s, o) // exact re-put
	got, _ := s.History(context.Background(), Query{InstrumentID: "AAPL"})
	if len(got) != 1 {
		t.Fatalf("re-put must be idempotent, got %d rows", len(got))
	}
}

func TestMemory_Put_Validation(t *testing.T) {
	s := NewMemory()
	cases := map[string]Observation{
		"empty instrument": {ObservationTime: day(1), Price: dec(1, 0), Kind: PriceKindClose, KnowledgeTime: day(1)},
		"zero obs time":    {InstrumentID: "X", Price: dec(1, 0), Kind: PriceKindClose, KnowledgeTime: day(1)},
		"nil price":        {InstrumentID: "X", ObservationTime: day(1), Kind: PriceKindClose, KnowledgeTime: day(1)},
		"unspecified kind": {InstrumentID: "X", ObservationTime: day(1), Price: dec(1, 0), KnowledgeTime: day(1)},
		"zero knowledge":   {InstrumentID: "X", ObservationTime: day(1), Price: dec(1, 0), Kind: PriceKindClose},
	}
	for name, o := range cases {
		if err := s.Put(context.Background(), []Observation{o}); err == nil {
			t.Errorf("%s: want validation error", name)
		}
	}
}

// TestMemory_StoredValueIsolated: a caller mutating its input after Put, or a
// returned Decimal, must not corrupt stored state.
func TestMemory_StoredValueIsolated(t *testing.T) {
	s := NewMemory()
	o := obs("AAPL", 1, dec(100, 0), PriceKindClose, 1)
	mustPut(t, s, o)
	o.Price.Coefficient = 7 // mutate the caller's copy after Put

	got, _ := s.History(context.Background(), Query{InstrumentID: "AAPL"})
	if got[0].Price.Coefficient != 100 {
		t.Fatalf("stored value bled from caller mutation: %d", got[0].Price.Coefficient)
	}
	got[0].Price.Coefficient = 9 // mutate the returned copy
	again, _ := s.History(context.Background(), Query{InstrumentID: "AAPL"})
	if again[0].Price.Coefficient != 100 {
		t.Fatalf("stored value bled from returned-copy mutation: %d", again[0].Price.Coefficient)
	}
}
