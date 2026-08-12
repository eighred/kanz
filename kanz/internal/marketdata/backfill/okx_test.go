package backfill

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dec"
)

// okxRow renders one candle in OKX's own shape: EVERY field a string, including
// the timestamp and the confirm flag.
func okxRow(openMs int64, o, h, l, c, v string, confirm string) string {
	return fmt.Sprintf(`["%d","%s","%s","%s","%s","%s","0","0","%s"]`,
		openMs, o, h, l, c, v, confirm)
}

func okxBody(rows ...string) string {
	return `{"code":"0","msg":"","data":[` + strings.Join(rows, ",") + `]}`
}

func okxServer(t *testing.T, pages ...string) (*httptest.Server, *[]string) {
	t.Helper()
	var queries []string
	i := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)
		body := okxBody()
		if i < len(pages) {
			body = pages[i]
		}
		i++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &queries
}

func okxAt(t *testing.T, srv *httptest.Server) *OKXSource {
	t.Helper()
	s, err := NewOKXSource(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	s.sleep = func(time.Duration) {} // no real pacing in tests
	return s
}

// PRICES STAY EXACT, the same claim proven for Binance and worth proving again
// because OKX's parse path is a separate one.
//
// 92233720368.54775807 is the value that tests it: 19 significant digits, so a
// float64 cannot hold it, and exactly int64-max at scale -8, so this platform
// can. A shorter value passes even with a float in the path.
func TestOKXKlinesParseExactly(t *testing.T) {
	srv, _ := okxServer(t, okxBody(okxRow(barStart.UnixMilli(),
		"92233720368.54775807", "92233720368.54775807", "64000.5",
		"64150.87654321", "0.00000001", "1")))

	got, err := okxAt(t, srv).Klines(context.Background(), "BTC-USDT", barStart, barStart.Add(time.Minute))
	if err != nil {
		t.Fatalf("Klines: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d bars, want 1", len(got))
	}
	b := got[0]
	for name, pair := range map[string][2]string{
		"open":   {dec.FromProto(b.Open).FloatString(8), "92233720368.54775807"},
		"high":   {dec.FromProto(b.High).FloatString(8), "92233720368.54775807"},
		"low":    {dec.FromProto(b.Low).FloatString(1), "64000.5"},
		"close":  {dec.FromProto(b.Close).FloatString(8), "64150.87654321"},
		"volume": {dec.FromProto(b.Volume).FloatString(8), "0.00000001"},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s = %s, want %s — a float somewhere in the path", name, pair[0], pair[1])
		}
	}
	if !b.BucketStart.Equal(barStart) {
		t.Errorf("bucket_start = %s, want %s", b.BucketStart, barStart)
	}
	// The SOURCE does not decide when Kanz learned it — the backfiller does.
	if !b.KnowledgeTime.IsZero() {
		t.Errorf("the source stamped knowledge_time (%s); that is the backfiller's decision",
			b.KnowledgeTime)
	}
}

// THE VENUE SAYS WHICH CANDLE IS STILL OPEN. OKX marks the in-flight one
// confirm="0", which is better evidence than comparing a close time against our
// clock — no skew, no guess. Keeping it would store a partial candle in a series
// whose contract is that a bar is what happened.
func TestOKXDropsTheUnconfirmedCandle(t *testing.T) {
	srv, _ := okxServer(t, okxBody(
		// newest first, as OKX sends: the open minute, then the settled one
		okxRow(barStart.Add(time.Minute).UnixMilli(), "105", "106", "104", "105.5", "0.2", "0"),
		okxRow(barStart.UnixMilli(), "100", "110", "90", "105", "1", "1"),
	))

	got, err := okxAt(t, srv).Klines(context.Background(), "BTC-USDT", barStart, barStart.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("Klines: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d bars, want 1 — an unconfirmed candle was stored as complete", len(got))
	}
	if !got[0].BucketStart.Equal(barStart) {
		t.Errorf("kept the wrong candle: %s", got[0].BucketStart)
	}
}

// OKX ANSWERS NEWEST FIRST. Appending rows in arrival order would hand the
// backfiller a descending series — which the store would accept without
// complaint, and every window read afterwards would be backwards.
func TestOKXReturnsAscendingOrder(t *testing.T) {
	srv, _ := okxServer(t, okxBody(
		okxRow(barStart.Add(2*time.Minute).UnixMilli(), "300", "300", "300", "300", "1", "1"),
		okxRow(barStart.Add(time.Minute).UnixMilli(), "200", "200", "200", "200", "1", "1"),
		okxRow(barStart.UnixMilli(), "100", "100", "100", "100", "1", "1"),
	))

	got, err := okxAt(t, srv).Klines(context.Background(), "BTC-USDT", barStart, barStart.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("Klines: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d bars, want 3", len(got))
	}
	for i := 1; i < len(got); i++ {
		if !got[i-1].BucketStart.Before(got[i].BucketStart) {
			t.Fatalf("bars are not ascending: %s then %s — the venue's newest-first order was kept",
				got[i-1].BucketStart, got[i].BucketStart)
		}
	}
}

// AN ERROR ARRIVES INSIDE AN HTTP 200. OKX carries refusals — an unknown
// instrument, a rate limit — in the response envelope's code field. A source
// that checked only the status would read a refusal as an empty window and
// record a market gap that never happened, which is indistinguishable from a
// quiet market afterwards.
func TestOKXSurfacesAnErrorCodeInsideA200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// 51001 is OKX's "instrument does not exist".
		_, _ = w.Write([]byte(`{"code":"51001","msg":"Instrument ID does not exist","data":[]}`))
	}))
	t.Cleanup(srv.Close)

	_, err := okxAt(t, srv).Klines(context.Background(), "NOPE-USDT", barStart, barStart.Add(time.Minute))
	if err == nil {
		t.Fatal("a venue refusal carried in a 200 was read as an empty window")
	}
	if !strings.Contains(err.Error(), "51001") {
		t.Errorf("the error does not name the venue's code: %v", err)
	}
}

// PAGING RUNS BACKWARDS: OKX's `after` asks for rows OLDER than a timestamp, so
// the walk starts at the window end and moves back. Each request must ask from
// the oldest row of the previous page, or it re-reads the same page forever.
func TestOKXPagesBackwardsUntilTheWindowIsCovered(t *testing.T) {
	p1 := okxBody(okxRow(barStart.Add(5*time.Minute).UnixMilli(), "200", "200", "200", "200", "1", "1"))
	p2 := okxBody(okxRow(barStart.UnixMilli(), "100", "100", "100", "100", "1", "1"))
	srv, queries := okxServer(t, p1, p2, okxBody())

	got, err := okxAt(t, srv).Klines(context.Background(), "BTC-USDT", barStart, barStart.Add(10*time.Minute))
	if err != nil {
		t.Fatalf("Klines: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d bars, want 2 across pages", len(got))
	}
	if len(*queries) < 2 {
		t.Fatalf("made %d requests, want at least 2 — it did not page", len(*queries))
	}
	// The second request must ask for rows older than the FIRST page's oldest row.
	want := fmt.Sprint(barStart.Add(5 * time.Minute).UnixMilli())
	if !strings.Contains((*queries)[1], "after="+want) {
		t.Errorf("second request does not resume from the first page's oldest row: %s", (*queries)[1])
	}
}

// THE BASE URL HAS NO DEFAULT — #147, and it bites hardest here: OKX serves demo
// and production from the SAME host, so nothing about a defaulted endpoint would
// say which book the candles came from.
func TestOKXRefusesAnEmptyBaseURL(t *testing.T) {
	if _, err := NewOKXSource("  "); err == nil {
		t.Fatal("an empty base URL was accepted")
	}
}

// A malformed row is an error, not a skipped candle: a gap nobody was told about
// reads downstream as a quiet market.
func TestOKXRefusesAMalformedRow(t *testing.T) {
	srv, _ := okxServer(t, `{"code":"0","msg":"","data":[["1","not-a-number","1","1","1","1","0","0","1"]]}`)

	if _, err := okxAt(t, srv).Klines(context.Background(), "BTC-USDT", barStart, barStart.Add(time.Minute)); err == nil {
		t.Fatal("a row with an unparseable price was accepted")
	}
}

// A KNOWN, TRACKED CONFLATION — pinned here so it cannot be discovered twice.
//
// OKX candles carry no trade count, and store.Bar.TradeCount is an int64 whose
// zero means "nothing traded in this interval" (0003_ohlcv_bars.sql says so
// explicitly). So an OKX-sourced minute that saw thousands of trades records
// zero, and anything reading trade_count sees OKX as permanently quiet.
//
// This test asserts the CURRENT behaviour rather than the desired one. When
// "not reported" becomes expressible end to end, this test is meant to fail —
// that is the tripwire, and the issue it belongs to is named in the failure.
func TestOKXTradeCountIsUnreportedAndCurrentlyReadsAsZero(t *testing.T) {
	srv, _ := okxServer(t, okxBody(okxRow(barStart.UnixMilli(), "100", "110", "90", "105", "1", "1")))

	got, err := okxAt(t, srv).Klines(context.Background(), "BTC-USDT", barStart, barStart.Add(time.Minute))
	if err != nil {
		t.Fatalf("Klines: %v", err)
	}
	if got[0].TradeCount != 0 {
		t.Fatalf("trade_count = %d: OKX started reporting a trade count, or the field became "+
			"nullable — either way #432 is now actionable and this pin should be replaced by the "+
			"real assertion", got[0].TradeCount)
	}
}
