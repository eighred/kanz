package backfill

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/exchange/netdial"
	"github.com/eighred/kanz/internal/marketdata/store"
)

// binanceMaxKlines is the most candles one /api/v3/klines call returns.
const binanceMaxKlines = 1000

// binancePace is the delay between paged requests.
//
// DELIBERATELY GENEROUS. A backfill is an operator-run job with no deadline —
// nobody is waiting on a millisecond — and the cost of being wrong in the other
// direction is an IP ban that takes out the LIVE adapters sharing this
// exchange's rate budget. Slower is free here; faster is not.
const binancePace = 250 * time.Millisecond

// BinanceSource fetches 1-minute klines from Binance's public REST API.
//
// PUBLIC DATA, NO CREDENTIAL. /api/v3/klines is unauthenticated, so this holds
// no API key and cannot place an order even by accident — worth stating because
// the venue adapter next door holds a key that can.
type BinanceSource struct {
	baseURL string
	http    *http.Client
	sleep   func(time.Duration)
	// now is injected so the incomplete-candle rule below is testable. A rule
	// that can only be exercised by waiting for a real minute to pass is a rule
	// nothing tests.
	now func() time.Time
}

// NewBinanceSource returns a source reading from baseURL.
//
// THE BASE URL IS REQUIRED AND HAS NO DEFAULT, the same stance #147 established
// for OKX: defaulting it would silently choose an environment, and the two that
// matter here are "the real exchange" and "a testnet whose history is fiction".
// A backfill that loaded testnet candles into the series a model trains on would
// be undetectable afterwards — the rows look identical.
func NewBinanceSource(baseURL string) (*BinanceSource, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, fmt.Errorf("backfill: binance base URL is required and has no default — it " +
			"selects between the live exchange and a testnet whose candles are fiction, and a " +
			"series loaded from the wrong one is indistinguishable afterwards")
	}
	return &BinanceSource{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    netdial.NewHTTPClient(5 * time.Minute),
		sleep:   func(d time.Duration) { time.Sleep(d) },
		now:     time.Now,
	}, nil
}

// Klines returns completed 1-minute candles whose BucketStart is in [from, to).
//
// IT PAGES, because one call returns at most 1000 candles — under 17 hours of
// minutes. A day of history is two calls; a year is well over five hundred, which
// is why the pace above matters.
func (b *BinanceSource) Klines(ctx context.Context, symbol string, from, to time.Time) ([]store.Bar, error) {
	var out []store.Bar
	cursor := from.UTC()

	for cursor.Before(to) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		page, err := b.page(ctx, symbol, cursor, to)
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			// The venue has nothing further in this window. Not an error: an
			// instrument that had not listed yet, or a gap the exchange itself
			// has, is data.
			break
		}
		out = append(out, page...)

		// ADVANCE PAST THE LAST CANDLE RETURNED, not by a fixed page size. A
		// window with gaps returns fewer than the limit, and stepping by the
		// limit would skip the minutes the venue did have. Stepping by one
		// resolution past the last bucket cannot loop: the next request starts
		// strictly later.
		next := page[len(page)-1].BucketStart.Add(time.Minute)
		if !next.After(cursor) {
			return nil, fmt.Errorf("backfill: binance returned a page ending at %s which does not "+
				"advance past %s — refusing to loop", page[len(page)-1].BucketStart, cursor)
		}
		cursor = next
		if cursor.Before(to) {
			b.sleep(binancePace)
		}
	}
	return out, nil
}

func (b *BinanceSource) page(ctx context.Context, symbol string, from, to time.Time) ([]store.Bar, error) {
	q := url.Values{
		"symbol":    {symbol},
		"interval":  {"1m"},
		"startTime": {fmt.Sprint(from.UTC().UnixMilli())},
		"endTime":   {fmt.Sprint(to.UTC().UnixMilli() - 1)}, // endTime is INCLUSIVE upstream
		"limit":     {fmt.Sprint(binanceMaxKlines)},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.baseURL+"/api/v3/klines?"+q.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("backfill: build request: %w", err)
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("backfill: binance klines %s: %w", symbol, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// SURFACED WITH THE STATUS, not retried here. 418 and 429 are Binance's
		// rate-limit and ban responses, and a backfill that quietly retried
		// through one would deepen the ban that the live adapters share.
		return nil, fmt.Errorf("backfill: binance klines %s: HTTP %d", symbol, resp.StatusCode)
	}

	// Binance returns an array of arrays; every numeric field is a STRING, which
	// is what lets them be parsed exactly.
	var raw [][]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("backfill: decode binance klines %s: %w", symbol, err)
	}

	out := make([]store.Bar, 0, len(raw))
	for _, k := range raw {
		bar, ok, err := binanceKline(k, b.now().UTC())
		if err != nil {
			return nil, fmt.Errorf("backfill: binance kline for %s: %w", symbol, err)
		}
		if ok {
			out = append(out, bar)
		}
	}
	return out, nil
}

// binanceKline maps one kline row.
//
// ok IS FALSE FOR AN INCOMPLETE CANDLE, and that is the trap this function
// exists to avoid. A request whose window reaches the present gets the MINUTE IN
// PROGRESS back as a normal-looking row — same shape, same fields, a close that
// is merely the last trade so far. Stored, it is a partial bar in a series whose
// contract is that a candle is what happened, and nothing downstream can tell.
// A row whose close time has not passed is dropped.
func binanceKline(k []json.RawMessage, now time.Time) (store.Bar, bool, error) {
	// [ openTime, open, high, low, close, volume, closeTime, quoteVolume, trades, ... ]
	if len(k) < 9 {
		return store.Bar{}, false, fmt.Errorf("expected at least 9 fields, got %d", len(k))
	}
	openMs, err := jsonInt(k[0])
	if err != nil {
		return store.Bar{}, false, fmt.Errorf("open time: %w", err)
	}
	closeMs, err := jsonInt(k[6])
	if err != nil {
		return store.Bar{}, false, fmt.Errorf("close time: %w", err)
	}
	trades, err := jsonInt(k[8])
	if err != nil {
		return store.Bar{}, false, fmt.Errorf("trade count: %w", err)
	}

	prices := make([]*big.Rat, 0, 5)
	for i, name := range []string{"open", "high", "low", "close", "volume"} {
		r, err := jsonDecimal(k[i+1])
		if err != nil {
			return store.Bar{}, false, fmt.Errorf("%s: %w", name, err)
		}
		prices = append(prices, r)
	}

	open := time.UnixMilli(openMs).UTC()
	// Binance's closeTime is the last millisecond of the interval, so a 1m candle
	// spans [openTime, closeTime] with closeTime = openTime + 59_999ms.
	if !time.UnixMilli(closeMs).UTC().Before(now) {
		return store.Bar{}, false, nil // still in progress
	}

	toProto := func(r *big.Rat, name string) (*commonpb.Decimal, error) {
		d, ok := dec.ToProtoScaled(r)
		if !ok {
			return nil, fmt.Errorf("%s %s is not representable as an exact Decimal", name, r.RatString())
		}
		return d, nil
	}
	o, err := toProto(prices[0], "open")
	if err != nil {
		return store.Bar{}, false, err
	}
	h, err := toProto(prices[1], "high")
	if err != nil {
		return store.Bar{}, false, err
	}
	l, err := toProto(prices[2], "low")
	if err != nil {
		return store.Bar{}, false, err
	}
	c, err := toProto(prices[3], "close")
	if err != nil {
		return store.Bar{}, false, err
	}
	v, err := toProto(prices[4], "volume")
	if err != nil {
		return store.Bar{}, false, err
	}

	return store.Bar{
		BucketStart: open,
		Open:        o,
		High:        h,
		Low:         l,
		Close:       c,
		Volume:      v,
		// BINANCE DOES REPORT ONE (kline field 8), so this is never nil — a real
		// zero here means the venue told us nothing traded, which is an
		// observation worth keeping distinct from OKX's silence (#432).
		TradeCount: &trades,
	}, true, nil
}

// jsonInt reads a JSON number that Binance sends unquoted.
func jsonInt(raw json.RawMessage) (int64, error) {
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, fmt.Errorf("not an integer: %s", string(raw))
	}
	return n, nil
}

// jsonDecimal reads a JSON string as an EXACT rational.
//
// Binance sends prices and sizes as strings precisely so they survive
// transport, and parsing one through float64 would undo that on arrival — the
// value would look right and be wrong in the last places, in a series a model
// trains on.
func jsonDecimal(raw json.RawMessage) (*big.Rat, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("not a quoted decimal: %s", string(raw))
	}
	return dec.ParseRat(s)
}
