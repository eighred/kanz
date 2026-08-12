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

// binanceKlineJSON renders one row in Binance's own shape: numbers unquoted,
// prices and sizes as STRINGS — which is what lets them be parsed exactly.
func binanceKlineJSON(openMs int64, o, h, l, c, v string, trades int) string {
	return fmt.Sprintf(`[%d,"%s","%s","%s","%s","%s",%d,"0",%d,"0","0","0"]`,
		openMs, o, h, l, c, v, openMs+59_999, trades)
}

func binanceServer(t *testing.T, pages ...string) (*httptest.Server, *[]string) {
	t.Helper()
	var queries []string
	i := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)
		body := "[]"
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

func binanceAt(t *testing.T, srv *httptest.Server, now time.Time) *BinanceSource {
	t.Helper()
	s, err := NewBinanceSource(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return now }
	s.sleep = func(time.Duration) {} // no real pacing in tests
	return s
}

var barStart = time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)

// PRICES ARRIVE AS STRINGS AND MUST STAY EXACT. Binance sends them quoted
// precisely so they survive transport; parsing one through float64 would undo
// that on arrival, and the value would look right while being wrong in the last
// places — in a series a model trains on.
func TestBinanceKlinesParseExactly(t *testing.T) {
	// THE VALUE MATTERS, and the first two attempts at it both proved the wrong
	// thing.
	//
	// 64123.12345678 is 13 significant digits and sits comfortably inside a
	// float64, so the assertion passed even with strconv.ParseFloat in the path —
	// it proved "a double was adequate here", not "the path is exact".
	//
	// 18 decimals then failed against the REAL code, because internal/dec has a
	// fixed scale of 8 and ToProtoScaled deliberately preserves MAGNITUDE over
	// precision beyond it ("you do not need eight decimal places on $100bn"). The
	// platform never promised 18.
	//
	// 92233720368.54775807 is the value that tests the actual claim: 19
	// significant digits, so a float64 cannot hold it, and exactly int64-max at
	// scale -8, so the platform can — the widest value this path is required to
	// carry losslessly.
	row := binanceKlineJSON(barStart.UnixMilli(),
		"92233720368.54775807", "92233720368.54775807", "64000.5",
		"64150.87654321", "0.00000001", 42)
	srv, _ := binanceServer(t, "["+row+"]")

	src := binanceAt(t, srv, barStart.Add(time.Hour))
	got, err := src.Klines(context.Background(), "BTCUSDT", barStart, barStart.Add(time.Minute))
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
	if b.TradeCount != 42 {
		t.Errorf("trade_count = %d, want 42", b.TradeCount)
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

// THE MINUTE IN PROGRESS IS DROPPED. A request whose window reaches the present
// gets it back as a normal-looking row — same shape, same fields, a close that
// is merely the last trade so far. Stored, it is a partial candle in a series
// whose contract is that a bar is what happened, and nothing downstream can tell.
func TestBinanceDropsTheCandleStillInProgress(t *testing.T) {
	complete := binanceKlineJSON(barStart.UnixMilli(), "100", "110", "90", "105", "1", 5)
	inProgress := binanceKlineJSON(barStart.Add(time.Minute).UnixMilli(), "105", "106", "104", "105.5", "0.2", 2)
	srv, _ := binanceServer(t, "["+complete+","+inProgress+"]")

	// now is 30 seconds INTO the second candle: its close time has not passed.
	src := binanceAt(t, srv, barStart.Add(90*time.Second))
	got, err := src.Klines(context.Background(), "BTCUSDT", barStart, barStart.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("Klines: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d bars, want 1 — the minute still in progress was stored as a complete candle",
			len(got))
	}
	if !got[0].BucketStart.Equal(barStart) {
		t.Errorf("kept the wrong candle: %s", got[0].BucketStart)
	}
}

// PAGING ADVANCES PAST THE LAST CANDLE RETURNED, not by a fixed page size: a
// window with gaps returns fewer rows than the limit, and stepping by the limit
// would skip the minutes the venue did have.
func TestBinancePagesUntilTheWindowIsCovered(t *testing.T) {
	p1 := "[" + binanceKlineJSON(barStart.UnixMilli(), "100", "100", "100", "100", "1", 1) + "]"
	p2 := "[" + binanceKlineJSON(barStart.Add(5*time.Minute).UnixMilli(), "200", "200", "200", "200", "1", 1) + "]"
	srv, queries := binanceServer(t, p1, p2, "[]")

	src := binanceAt(t, srv, barStart.Add(time.Hour))
	got, err := src.Klines(context.Background(), "BTCUSDT", barStart, barStart.Add(10*time.Minute))
	if err != nil {
		t.Fatalf("Klines: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d bars, want 2 across pages", len(got))
	}
	if len(*queries) < 2 {
		t.Fatalf("made %d requests, want at least 2 — it did not page", len(*queries))
	}
	// The second request must start AFTER the last candle of the first page,
	// not at a fixed offset.
	want := fmt.Sprint(barStart.Add(time.Minute).UnixMilli())
	if !strings.Contains((*queries)[1], "startTime="+want) {
		t.Errorf("second request startTime is not one minute past the first page: %s", (*queries)[1])
	}
}

// A RATE-LIMIT RESPONSE SURFACES, and is not retried here. 418 and 429 are
// Binance's ban responses, and a backfill that quietly retried through one would
// deepen a ban the LIVE adapters share.
func TestBinanceSurfacesARateLimitResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	src := binanceAt(t, srv, barStart.Add(time.Hour))
	_, err := src.Klines(context.Background(), "BTCUSDT", barStart, barStart.Add(time.Minute))
	if err == nil {
		t.Fatal("a 429 was swallowed")
	}
	if !strings.Contains(err.Error(), "429") {
		t.Errorf("the error does not name the status: %v", err)
	}
}

// THE BASE URL HAS NO DEFAULT. Defaulting it would silently choose between the
// live exchange and a testnet whose candles are fiction — and a series loaded
// from the wrong one is indistinguishable afterwards.
func TestBinanceRefusesAnEmptyBaseURL(t *testing.T) {
	if _, err := NewBinanceSource("  "); err == nil {
		t.Fatal("an empty base URL was accepted")
	}
}

// A malformed row is an error, not a skipped candle: a gap nobody was told about
// reads downstream as a quiet market.
func TestBinanceRefusesAMalformedRow(t *testing.T) {
	srv, _ := binanceServer(t, `[[1,"not-a-number","1","1","1","1",2,"0",1,"0","0","0"]]`)
	src := binanceAt(t, srv, barStart.Add(time.Hour))

	if _, err := src.Klines(context.Background(), "BTCUSDT", barStart, barStart.Add(time.Minute)); err == nil {
		t.Fatal("a row with an unparseable price was accepted")
	}
}
