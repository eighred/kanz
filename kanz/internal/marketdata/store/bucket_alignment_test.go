package store

import (
	"errors"
	"testing"
	"time"
)

func TestWindowNeedsUTCAlignedMarketEvidence(t *testing.T) {
	origin := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
	for _, res := range []Resolution{Resolution1m, Resolution1h, Resolution1d} {
		t.Run(string(res), func(t *testing.T) {
			step, _ := res.Interval()
			good := bar(0, 10000, 3)
			good.Resolution = res
			good.BucketStart = origin.In(time.FixedZone("fixture", 19800))
			covered := cov(0, step, "fixture")
			covered.Resolution = res
			covered.BucketStart = origin.Add(step)
			if err := good.Validate(); err != nil {
				t.Fatal(err)
			}
			if err := covered.Validate(); err != nil {
				t.Fatal(err)
			}
			for _, offset := range []time.Duration{-time.Nanosecond, time.Nanosecond} {
				bad := good
				bad.BucketStart = good.BucketStart.Add(step + offset)
				badCoverage := covered
				badCoverage.BucketStart = covered.BucketStart.Add(offset)
				if !errors.Is(bad.Validate(), ErrInvalidBar) || !errors.Is(badCoverage.Validate(), ErrInvalidCoverage) || badCoverage.Whole() {
					t.Fatal("off-grid evidence accepted")
				}
			}
			shifted := good
			shifted.BucketStart = origin.Add(time.Second)
			shifted2 := shifted
			shifted2.BucketStart = shifted.BucketStart.Add(step)
			badCoverage := covered
			badCoverage.BucketStart = origin.Add(time.Second)
			// Two different starts in bucket zero formerly counted as two observed buckets.
			att, ok := AttestedWindowOf([]Bar{good, shifted}, []Coverage{badCoverage}, origin, origin.Add(2*step), res)
			if !ok || att.Buckets != 2 || att.Observed != 1 || att.Quiet != 0 || att.Unknown != 1 || att.Sound() {
				t.Fatalf("false coverage: %+v", att)
			}
			if len(ContiguousSuffix([]Bar{shifted, shifted2})) != 0 {
				t.Fatal("shifted closes reached indicator suffix")
			}
			att, ok = AttestedWindowOf([]Bar{good}, []Coverage{covered}, origin, origin.Add(2*step), res)
			if !ok || !att.Sound() || att.Observed != 1 || att.Quiet != 1 || att.Unknown != 0 {
				t.Fatalf("valid coverage: %+v", att)
			}
		})
	}
}

func TestBarAndCoverageWritesRejectShiftedBatches(t *testing.T) {
	b := bar(0, 10000, 1)
	bad := b
	bad.BucketStart = bad.BucketStart.Add(time.Second)
	c := cov(0, time.Minute, "fixture")
	badC := c
	badC.BucketStart = badC.BucketStart.Add(time.Second)
	checkBars := func(t *testing.T, st BarStore) {
		t.Helper()
		if err := st.PutBars(t.Context(), []Bar{b, bad}); !errors.Is(err, ErrInvalidBar) {
			t.Fatalf("put bars: %v", err)
		}
		rows, err := st.Bars(t.Context(), BarQuery{InstrumentID: b.InstrumentID, Venue: b.Venue, Resolution: b.Resolution})
		if err != nil || len(rows) != 0 {
			t.Fatalf("partial bar write: %v, %v", rows, err)
		}
	}
	checkCoverage := func(t *testing.T, st CoverageStore) {
		t.Helper()
		if err := st.PutCoverage(t.Context(), []Coverage{c, badC}); !errors.Is(err, ErrInvalidCoverage) {
			t.Fatalf("put coverage: %v", err)
		}
		rows, err := st.Coverage(t.Context(), CoverageQuery{InstrumentID: c.InstrumentID, Venue: c.Venue, Resolution: c.Resolution})
		if err != nil || len(rows) != 0 {
			t.Fatalf("partial coverage write: %v, %v", rows, err)
		}
	}
	t.Run("memory bars", func(t *testing.T) { checkBars(t, NewMemory()) })
	t.Run("memory coverage", func(t *testing.T) { checkCoverage(t, NewMemory()) })
	t.Run("postgres bars", func(t *testing.T) { checkBars(t, NewPostgres(newBarPool(t))) })
	t.Run("postgres coverage", func(t *testing.T) { checkCoverage(t, NewPostgres(newCoveragePool(t))) })
}

func TestPostgresRefusesPreviouslyStoredShiftedBuckets(t *testing.T) {
	b := bar(0, 10000, 1)
	bars := NewPostgres(newBarPool(t))
	if err := bars.PutBars(t.Context(), []Bar{b}); err != nil {
		t.Fatal(err)
	}
	if _, err := bars.pool.Exec(t.Context(), "UPDATE ohlcv_bars SET bucket_start=bucket_start+interval '1 second'"); err != nil {
		t.Fatal(err)
	}
	if rows, err := bars.Bars(t.Context(), BarQuery{InstrumentID: b.InstrumentID, Venue: b.Venue, Resolution: b.Resolution}); !errors.Is(err, ErrInvalidBar) || len(rows) != 0 {
		t.Fatalf("read legacy bar: %v, %v", rows, err)
	}
	c := cov(0, time.Minute, "fixture")
	coverage := NewPostgres(newCoveragePool(t))
	if err := coverage.PutCoverage(t.Context(), []Coverage{c}); err != nil {
		t.Fatal(err)
	}
	if _, err := coverage.pool.Exec(t.Context(), "UPDATE ingestion_coverage SET bucket_start=bucket_start+interval '1 second'"); err != nil {
		t.Fatal(err)
	}
	if rows, err := coverage.Coverage(t.Context(), CoverageQuery{InstrumentID: c.InstrumentID, Venue: c.Venue, Resolution: c.Resolution}); !errors.Is(err, ErrInvalidCoverage) || len(rows) != 0 {
		t.Fatalf("read legacy coverage: %v, %v", rows, err)
	}
}
