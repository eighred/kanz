package arch

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A COUNT THE VENUE NEVER GAVE US MUST NOT BE STORABLE AS A NUMBER (#432).
//
// ohlcv_bars.trade_count shipped as BIGINT NOT NULL DEFAULT 0, with a comment
// beside it in 0003_ohlcv_bars.sql insisting on the distinction it could not
// hold:
//
//	ZERO IS MEANINGFUL: an interval in which nothing traded is a real
//	observation, not a gap. An indicator that cannot tell the two apart
//	invents movement where the market was simply quiet.
//
// OKX candles carry no trade count — the row is
// [ts,o,h,l,c,vol,volCcy,volCcyQuote,confirm], there is no field to map — so the
// backfill wrote 0 for every OKX bar, and a minute with thousands of trades was
// recorded as a dead market. 0004 made the column NULLable and store.Bar.
// TradeCount a *int64, so "not reported" and "nothing traded" are finally
// different rows.
//
// WHAT THIS GUARD STOPS. The pointer is the whole mechanism, and it is the kind
// of thing a later reader "simplifies": *int64 looks like ceremony next to an
// int64, the compiler then demands every producer be updated, and the quickest
// way to satisfy it is to write 0 again. That change compiles, passes every
// existing test, and silently restores the conflation — while the bars it
// corrupts are the history a model trains on years later, which is what makes
// this expensive to discover late and cheap to prevent here.
//
// WHY THE STORE AND NOT THE PROTO: market.v1.Bar.trade_count is a plain scalar
// with no unset state, and that is sound only because every publisher on that
// path counts trades itself. The backfill sources reach the store directly and
// are the ones that know they were never told.

const (
	barTypeFile     = "internal/marketdata/store/bar.go"
	barMigrationDir = "services/market-data/migrations"
)

var (
	// tradeCountFieldRe captures the declared type of the TradeCount field.
	tradeCountFieldRe = regexp.MustCompile(`(?m)^\s*TradeCount\s+(\S+)\s*$`)
	// notNullTradeCountRe finds a migration re-imposing NOT NULL on the column.
	// Deliberately matches the COLUMN DEFINITION shape (name, type, NOT NULL) so
	// 0003's original CREATE is found too — it is then excused by sequence below.
	notNullTradeCountRe = regexp.MustCompile(`trade_count\s+BIGINT\s+NOT\s+NULL`)
	// dropsNotNullRe finds the ALTER that made it nullable.
	dropsNotNullRe = regexp.MustCompile(`ALTER\s+COLUMN\s+trade_count\s+DROP\s+NOT\s+NULL`)
)

func TestAnUnreportedTradeCountCannotBeStoredAsZero(t *testing.T) {
	root := moduleRoot(t)

	// THE GO HALF: the field must be a pointer.
	body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(barTypeFile)))
	if err != nil {
		t.Fatalf("read %s: %v", barTypeFile, err)
	}
	m := tradeCountFieldRe.FindStringSubmatch(string(body))
	if m == nil {
		t.Fatalf("could not find the TradeCount field in %s — it was renamed or reshaped, and "+
			"this guard can no longer see it", barTypeFile)
	}
	if got := m[1]; got != "*int64" {
		t.Errorf("store.Bar.TradeCount is %s, want *int64.\n"+
			"A non-pointer count cannot say \"the venue did not report one\": OKX candles carry no "+
			"trade-count field, so every OKX bar would again be stored as a minute in which nothing "+
			"traded. Nothing reads this column today, which is exactly why the damage is invisible "+
			"— the bars being written now are the history a model trains on later, and re-fetching "+
			"backfilled history is expensive where it is possible at all (#432).", got)
	}

	// THE SQL HALF: the column must still be nullable. The Go pointer is useless
	// if a later migration re-imposes NOT NULL — writes would start failing, or
	// worse, someone would "fix" that by defaulting the nil back to zero.
	migrations, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(barMigrationDir), "*.sql"))
	if err != nil {
		t.Fatalf("glob %s: %v", barMigrationDir, err)
	}
	// NON-VACUITY: a moved migration tree returns nothing and this half passes
	// having read no SQL at all.
	if len(migrations) == 0 {
		t.Fatalf("found zero migrations under %s — the scanner is broken, not the estate",
			barMigrationDir)
	}

	var lastNotNull, lastDrop string
	sawColumn := false
	for _, p := range migrations {
		sql, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		// Normalise line endings: git on Windows hands these over with CRLF, and a
		// guard a contributor cannot run is one they cannot trust.
		text := strings.ReplaceAll(string(sql), "\r\n", "\n")
		base := filepath.Base(p)
		if strings.Contains(text, "trade_count") {
			sawColumn = true
		}
		// Glob returns lexical order, which for zero-padded migration names is
		// apply order — so the LAST match wins, exactly as Postgres would see it.
		if notNullTradeCountRe.MatchString(text) {
			lastNotNull = base
		}
		if dropsNotNullRe.MatchString(text) {
			lastDrop = base
		}
	}

	// NON-VACUITY, the match half: if the column is renamed, neither pattern fires
	// and this passes while asserting nothing.
	if !sawColumn {
		t.Fatalf("no migration under %s mentions trade_count — the column was renamed and this "+
			"guard is asserting nothing", barMigrationDir)
	}
	if lastDrop == "" {
		t.Fatalf("no migration drops NOT NULL from trade_count — 0004 is missing, so the column " +
			"cannot hold \"not reported\" and the *int64 above has nowhere to write a nil (#432)")
	}
	if lastNotNull > lastDrop {
		t.Errorf("%s re-imposes NOT NULL on trade_count, after %s made it nullable.\n"+
			"An unreported count then has no representation in the column: the write fails, and "+
			"the cheapest way to make it pass again is to substitute a zero — which is the exact "+
			"conflation #432 removed.", lastNotNull, lastDrop)
	}
}
