package main

import (
	"encoding/json"
	"math/big"
	"reflect"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
)

func sampleBars(count int) []bar {
	day := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	bars := make([]bar, count)
	for i := range bars {
		bars[i] = bar{start: day.Add(time.Duration(i-warmupBars) * interval), open: big.NewRat(100, 1), close: &commonpb.Decimal{Coefficient: 100}}
	}
	return bars
}

func TestPaperMarksGainAndLossAfterAllCosts(t *testing.T) {
	// At a 100 open each BTC costs 100.05 * 1.001. Floor 1000 / that
	// amount to 9.98501748 BTC, with the small remainder retained in cash.
	for _, tc := range []struct {
		last                            int64
		marked, exitCost, pnl, drawdown string
	}{
		{200, "1997.00349613", "2.99450674", "994.00898938", "0.00000000"},
		{50, "499.25087413", "0.74862669", "-501.49775256", "50.07491259"},
	} {
		bars := sampleBars(21)
		for i := 15; i < 20; i++ {
			bars[i].close.Coefficient = 120
		}
		bars[20].close.Coefficient = tc.last
		r, err := simulate(t.Context(), bars)
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Fills) != 1 || r.Fills[0].Side != "buy" || r.Quantity != "9.98501748" || r.Cash != "0.00000013" ||
			r.Marked != tc.marked || r.ExitCost != tc.exitCost || r.PnL != tc.pnl || r.Drawdown != tc.drawdown ||
			r.Fees != "0.99900100" || r.Slippage != "0.49925087" {
			t.Fatalf("close %d: %+v", tc.last, r)
		}
	}
}

func TestPaperUsesNextOpenForEntryAndExit(t *testing.T) {
	bars := sampleBars(23)
	bars[20].close.Coefficient = 101
	bars[21].open.SetInt64(200)
	bars[21].close.Coefficient = 50
	bars[22].open.SetInt64(80)
	r, err := simulate(t.Context(), bars)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Fills) != 2 || r.Quantity != "0.00000000" || r.ExitCost != "0.00000000" {
		t.Fatal(r)
	}
	for i, want := range []struct{ side, price string }{{"buy", "200.100000000000"}, {"sell", "79.960000000000"}} {
		f := r.Fills[i]
		if f.Side != want.side || f.Price != want.price || !f.FillTime.Equal(bars[i+21].start) || !f.SignalTime.Equal(f.FillTime.Add(-time.Millisecond)) {
			t.Fatal(f)
		}
	}
	replayed, err := simulate(t.Context(), bars)
	if err != nil || !reflect.DeepEqual(r, replayed) {
		t.Fatal("replay diverged", err)
	}
}

func TestInputRequiresACompleteTradedUTCWindow(t *testing.T) {
	day := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rows := make([][]any, warmupBars+int(dayBars))
	for i := range rows {
		start := day.Add(time.Duration(i-warmupBars) * interval)
		rows[i] = []any{start.UnixMilli(), "100", "101", "99", "100", "1", start.Add(interval).UnixMilli() - 1, "100", 1, "0", "0", "0"}
	}
	encode := func(rows [][]any) []byte {
		t.Helper()
		data, err := json.Marshal(rows)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	if _, err := readBars(encode(rows), day); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		index int
		bad   any
	}{
		{"repeated timestamp", 0, rows[0][0]}, {"unfinished interval", 6, rows[1][0]},
		{"negative close", 4, "-10"}, {"fractional syntax", 1, "2/3"}, {"inconsistent high", 2, "98"}, {"missing volume", 5, "0"}, {"no trades", 8, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := rows[1][tc.index]
			rows[1][tc.index] = tc.bad
			defer func() { rows[1][tc.index] = old }()
			if _, err := readBars(encode(rows), day); err == nil {
				t.Fatal("invalid bar accepted")
			}
		})
	}
	if _, err := readBars(encode(rows[1:]), day); err == nil {
		t.Fatal("missing warm-up bar accepted")
	}
}
