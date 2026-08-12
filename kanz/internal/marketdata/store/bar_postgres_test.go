// Bar store tests, gated on TEST_POSTGRES_URL.
//
// Every assertion here is about SQL behaviour — the bitemporal primary key, the
// DISTINCT ON collapse, the knowledge horizon — so a fake would be testing the
// fake. kanz/test/backing/up.sh provides the database, and its role must be
// NOSUPERUSER or the isolation this store relies on is bypassed silently.
//
// Each test builds its own schema and applies the REAL migration, so what is
// proven is the file that ships rather than a hand-written CREATE TABLE that
// could drift from it.
package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
)

var barT0 = time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)

func newBarPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("set TEST_POSTGRES_URL (kanz/test/backing/up.sh) to run the bar store tests")
	}
	ctx := context.Background()

	schema := fmt.Sprintf("bars_test_%d", time.Now().UnixNano())
	boot, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect (bootstrap): %v", err)
	}
	defer boot.Close()
	if _, err := boot.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatalf("create schema: %v", err)
	}

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	// search_path WITHOUT public, so an unqualified name resolves here and
	// nowhere else — a missing object errors instead of silently hitting a
	// shared table (#212).
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		drop, derr := pgxpool.New(context.Background(), url)
		if derr != nil {
			t.Errorf("reconnect to drop schema %s: %v — it is now residue", schema, derr)
			return
		}
		defer drop.Close()
		if _, derr := drop.Exec(context.Background(),
			`DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`); derr != nil {
			t.Errorf("drop schema %s: %v — it is now residue", schema, derr)
		}
	})

	b, err := os.ReadFile(filepath.Join("..", "..", "..", "services", "market-data", "migrations",
		"0003_ohlcv_bars.sql"))
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	if _, err := pool.Exec(ctx, string(b)); err != nil {
		t.Fatalf("apply migration: %v", err)
	}
	return pool
}

func d(coef int64, exp int32) *commonpb.Decimal {
	return &commonpb.Decimal{Coefficient: coef, Exponent: exp}
}

// bar builds a well-formed candle: open 100, high 110, low 90, close c.
func bar(bucketMin int, closeCoef int64, knowMin int) Bar {
	return Bar{
		InstrumentID:  "BTC-USDT",
		Venue:         "XBIN",
		Resolution:    Resolution1m,
		BucketStart:   barT0.Add(time.Duration(bucketMin) * time.Minute),
		Open:          d(10000, -2),
		High:          d(11000, -2),
		Low:           d(9000, -2),
		Close:         d(closeCoef, -2),
		Volume:        d(15, -1),
		TradeCount:    42,
		KnowledgeTime: barT0.Add(time.Duration(knowMin) * time.Minute),
	}
}

// EVERY FIELD SURVIVES PERSISTENCE. The defect this store closes is that ingest
// kept the close and discarded open, high, low, volume and trade_count — so a
// round trip that only checked the close would pass on the very bug being fixed.
func TestPostgresBarsRoundTripEveryField(t *testing.T) {
	pool := newBarPool(t)
	st := NewPostgres(pool)
	ctx := context.Background()

	want := bar(0, 10500, 1)
	if err := st.PutBars(ctx, []Bar{want}); err != nil {
		t.Fatalf("PutBars: %v", err)
	}

	got, err := st.Bars(ctx, BarQuery{InstrumentID: "BTC-USDT", Venue: "XBIN", Resolution: Resolution1m})
	if err != nil {
		t.Fatalf("Bars: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("bars = %d, want 1", len(got))
	}
	g := got[0]
	for _, f := range []struct {
		name      string
		got, want *commonpb.Decimal
	}{
		{"open", g.Open, want.Open}, {"high", g.High, want.High}, {"low", g.Low, want.Low},
		{"close", g.Close, want.Close}, {"volume", g.Volume, want.Volume},
	} {
		if f.got.GetCoefficient() != f.want.GetCoefficient() || f.got.GetExponent() != f.want.GetExponent() {
			t.Errorf("%s = %v, want %v — this is the field the old ingest discarded", f.name, f.got, f.want)
		}
	}
	if g.TradeCount != want.TradeCount {
		t.Errorf("trade_count = %d, want %d", g.TradeCount, want.TradeCount)
	}
	if !g.BucketStart.Equal(want.BucketStart) {
		t.Errorf("bucket_start = %s, want %s", g.BucketStart, want.BucketStart)
	}
}

// A RESTATEMENT COEXISTS WITH THE ORIGINAL, and a read AS OF the earlier moment
// still sees the earlier value.
//
// This is the load-bearing property of the whole store. Without it a backtest
// dated last year silently reads a correction that arrived last week and reports
// a strategy that could not have existed — optimistic, in the direction nobody
// checks.
func TestPostgresBarsAreBitemporal(t *testing.T) {
	pool := newBarPool(t)
	st := NewPostgres(pool)
	ctx := context.Background()

	original := bar(0, 10500, 1)  // close 105.00, known at T+1m
	restated := bar(0, 10900, 30) // same bucket, close 109.00, known at T+30m
	if err := st.PutBars(ctx, []Bar{original, restated}); err != nil {
		t.Fatalf("PutBars: %v", err)
	}

	// As of T+5m, only the original was knowable.
	early, err := st.Bars(ctx, BarQuery{
		InstrumentID: "BTC-USDT", Venue: "XBIN", Resolution: Resolution1m,
		AsOf: barT0.Add(5 * time.Minute),
	})
	if err != nil {
		t.Fatalf("Bars: %v", err)
	}
	if len(early) != 1 || early[0].Close.GetCoefficient() != 10500 {
		t.Fatalf("as-of T+5m close = %v, want 10500 — a correction that arrived at T+30m leaked "+
			"backwards into a read that predates it", early)
	}

	// With no horizon, the latest known version wins.
	latest, err := st.Bars(ctx, BarQuery{InstrumentID: "BTC-USDT", Venue: "XBIN", Resolution: Resolution1m})
	if err != nil {
		t.Fatalf("Bars: %v", err)
	}
	if len(latest) != 1 || latest[0].Close.GetCoefficient() != 10900 {
		t.Fatalf("latest close = %v, want 10900", latest)
	}
}

// A RE-PUT OF THE IDENTICAL TUPLE IS A NO-OP. Redelivery is normal on this
// platform (a nacked bus delivery is retried), so an ingest that ran twice must
// not double a bar into the series.
func TestPostgresBarsPutIsIdempotent(t *testing.T) {
	pool := newBarPool(t)
	st := NewPostgres(pool)
	ctx := context.Background()

	b := bar(0, 10500, 1)
	for i := 0; i < 3; i++ {
		if err := st.PutBars(ctx, []Bar{b}); err != nil {
			t.Fatalf("PutBars #%d: %v", i, err)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ohlcv_bars`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("rows = %d, want 1 — a redelivered bar was stored again", n)
	}
}

// THE VENUE IS PART OF THE SERIES' IDENTITY. Two exchanges' candles for the same
// minute must not collapse into one: they have different books and different
// closes, and a model trained on a blend of them executes against neither.
func TestPostgresBarsAreSeparatedByVenue(t *testing.T) {
	pool := newBarPool(t)
	st := NewPostgres(pool)
	ctx := context.Background()

	binance := bar(0, 10500, 1)
	okx := bar(0, 10600, 1)
	okx.Venue = "XOKX"
	if err := st.PutBars(ctx, []Bar{binance, okx}); err != nil {
		t.Fatalf("PutBars: %v", err)
	}

	for _, tc := range []struct {
		venue string
		want  int64
	}{{"XBIN", 10500}, {"XOKX", 10600}} {
		got, err := st.Bars(ctx, BarQuery{InstrumentID: "BTC-USDT", Venue: tc.venue, Resolution: Resolution1m})
		if err != nil {
			t.Fatalf("Bars(%s): %v", tc.venue, err)
		}
		if len(got) != 1 {
			t.Fatalf("%s: bars = %d, want 1 — the venues collapsed into one series", tc.venue, len(got))
		}
		if got[0].Close.GetCoefficient() != tc.want {
			t.Errorf("%s close = %d, want %d — this venue is being served another venue's candle",
				tc.venue, got[0].Close.GetCoefficient(), tc.want)
		}
	}
}

// Resolutions do not collapse either: a 1m candle and the 1h candle covering it
// are different observations of the same market at the same instant.
func TestPostgresBarsAreSeparatedByResolution(t *testing.T) {
	pool := newBarPool(t)
	st := NewPostgres(pool)
	ctx := context.Background()

	minute := bar(0, 10500, 1)
	hour := bar(0, 10700, 1)
	hour.Resolution = Resolution1h
	if err := st.PutBars(ctx, []Bar{minute, hour}); err != nil {
		t.Fatalf("PutBars: %v", err)
	}

	got, err := st.Bars(ctx, BarQuery{InstrumentID: "BTC-USDT", Venue: "XBIN", Resolution: Resolution1m})
	if err != nil {
		t.Fatalf("Bars: %v", err)
	}
	if len(got) != 1 || got[0].Close.GetCoefficient() != 10500 {
		t.Fatalf("1m series = %v, want only the 1m candle", got)
	}
}

// The window is [From, To): a bar starting exactly at To belongs to the next
// window, so consecutive queries neither drop nor double-count a bucket.
func TestPostgresBarsWindowIsHalfOpen(t *testing.T) {
	pool := newBarPool(t)
	st := NewPostgres(pool)
	ctx := context.Background()

	if err := st.PutBars(ctx, []Bar{bar(0, 10100, 1), bar(1, 10200, 1), bar(2, 10300, 1)}); err != nil {
		t.Fatalf("PutBars: %v", err)
	}
	got, err := st.Bars(ctx, BarQuery{
		InstrumentID: "BTC-USDT", Venue: "XBIN", Resolution: Resolution1m,
		From: barT0, To: barT0.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatalf("Bars: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("bars = %d, want 2 (buckets 0 and 1; bucket 2 starts at To)", len(got))
	}
	if !got[0].BucketStart.Before(got[1].BucketStart) {
		t.Error("bars are not ordered by bucket_start ascending")
	}
}

// A BAR NO MARKET PRODUCED IS REFUSED BEFORE IT REACHES THE SERIES, and the
// whole batch fails with it: half a batch written is a hole in a series, and a
// hole is invisible downstream — an indicator just computes a different number.
func TestPostgresBarsRefuseAnImpossibleCandle(t *testing.T) {
	pool := newBarPool(t)
	st := NewPostgres(pool)
	ctx := context.Background()

	bad := bar(1, 10500, 1)
	bad.Low = d(12000, -2) // low above high

	if err := st.PutBars(ctx, []Bar{bar(0, 10100, 1), bad}); err == nil {
		t.Fatal("a candle with low above high was accepted")
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ohlcv_bars`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("rows = %d, want 0 — the valid half of a rejected batch was written, leaving a "+
			"series with a hole nothing downstream can see", n)
	}
}
