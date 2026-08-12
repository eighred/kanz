package backfill

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/exchange/netdial"
	"github.com/eighred/kanz/internal/marketdata/store"
)

// okxMaxCandles is the most rows /api/v5/market/history-candles returns.
const okxMaxCandles = 100

// okxPace is the delay between paged requests — the same reasoning as Binance's:
// a backfill has no deadline, and the cost of being wrong the other way is a
// rate-limit ban shared with the LIVE adapter.
const okxPace = 250 * time.Millisecond

// OKXSource fetches 1-minute candles from OKX's public REST API.
//
// THREE THINGS DIFFER FROM BINANCE and each one is a way to get this wrong:
//
//  1. The response is a {code, msg, data} envelope. A non-"0" code is an ERROR
//     carried inside an HTTP 200, so checking the status alone would read a
//     refusal as an empty window.
//  2. Rows come NEWEST FIRST. Appending them in arrival order would hand the
//     backfiller a descending series.
//  3. Completeness is an explicit `confirm` flag rather than something inferred
//     from the clock — which is better, and is used instead of a time comparison.
//
// PUBLIC DATA, NO CREDENTIAL: this holds no API key and cannot place an order.
type OKXSource struct {
	baseURL string
	http    *http.Client
	sleep   func(time.Duration)
}

// NewOKXSource returns a source reading from baseURL.
//
// REQUIRED, NO DEFAULT — #147's ruling, and it bites harder here than anywhere:
// OKX serves demo and production from the SAME host and distinguishes them by a
// request header, so the endpoint alone cannot say which book a caller meant.
func NewOKXSource(baseURL string) (*OKXSource, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, fmt.Errorf("backfill: okx base URL is required and has no default (#147) — it " +
			"selects the environment, and a series loaded from the wrong one is indistinguishable " +
			"afterwards")
	}
	return &OKXSource{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    netdial.NewHTTPClient(5 * time.Minute),
		sleep:   func(d time.Duration) { time.Sleep(d) },
	}, nil
}

// Klines returns completed 1-minute candles whose BucketStart is in [from, to),
// ascending.
//
// PAGING RUNS BACKWARDS because OKX's history endpoint does: `after` asks for
// rows older than a timestamp. The walk starts at `to` and moves back until it
// passes `from`, then the whole result is sorted ascending — the order the
// backfiller and the store expect.
func (o *OKXSource) Klines(ctx context.Context, instID string, from, to time.Time) ([]store.Bar, error) {
	var out []store.Bar
	cursor := to.UTC()

	for cursor.After(from) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		page, oldest, err := o.page(ctx, instID, cursor)
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			break // nothing older; an unlisted instrument or a venue gap is data
		}
		for _, b := range page {
			if !b.BucketStart.Before(from) && b.BucketStart.Before(to) {
				out = append(out, b)
			}
		}
		if !oldest.Before(cursor) {
			return nil, fmt.Errorf("backfill: okx returned a page whose oldest row (%s) does not "+
				"precede the cursor (%s) — refusing to loop", oldest, cursor)
		}
		cursor = oldest
		if cursor.After(from) {
			o.sleep(okxPace)
		}
	}
	// ASCENDING, because the venue answered newest-first and everything
	// downstream — the backfiller's bucket comparison, the store's series read —
	// is written for time order.
	sort.Slice(out, func(i, j int) bool { return out[i].BucketStart.Before(out[j].BucketStart) })
	return out, nil
}

// page fetches up to okxMaxCandles rows older than before, and reports the
// oldest bucket it saw — including rows it discarded as incomplete, so the walk
// still advances past them.
func (o *OKXSource) page(ctx context.Context, instID string, before time.Time) ([]store.Bar, time.Time, error) {
	q := url.Values{
		"instId": {instID},
		"bar":    {"1m"},
		// `after` means "rows older than this timestamp" in OKX's pagination.
		"after": {fmt.Sprint(before.UTC().UnixMilli())},
		"limit": {fmt.Sprint(okxMaxCandles)},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		o.baseURL+"/api/v5/market/history-candles?"+q.Encode(), nil)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("backfill: build request: %w", err)
	}
	resp, err := o.http.Do(req)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("backfill: okx candles %s: %w", instID, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, time.Time{}, fmt.Errorf("backfill: okx candles %s: HTTP %d", instID, resp.StatusCode)
	}

	var body struct {
		Code string     `json:"code"`
		Msg  string     `json:"msg"`
		Data [][]string `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, time.Time{}, fmt.Errorf("backfill: decode okx candles %s: %w", instID, err)
	}
	// AN ERROR INSIDE A 200. OKX reports refusals in the envelope, so a status
	// check alone would read "instrument does not exist" or "rate limited" as an
	// empty window and record a gap that never happened.
	if body.Code != "0" {
		return nil, time.Time{}, fmt.Errorf("backfill: okx candles %s: code %s: %s",
			instID, body.Code, body.Msg)
	}

	out := make([]store.Bar, 0, len(body.Data))
	oldest := before
	for _, row := range body.Data {
		bar, complete, err := okxCandle(row)
		if err != nil {
			return nil, time.Time{}, fmt.Errorf("backfill: okx candle for %s: %w", instID, err)
		}
		// The cursor moves past every row SEEN, complete or not: an incomplete
		// candle still tells us how far back this page reached, and skipping it
		// in the cursor would re-request the same page forever.
		if bar.BucketStart.Before(oldest) {
			oldest = bar.BucketStart
		}
		if complete {
			out = append(out, bar)
		}
	}
	return out, oldest, nil
}

// okxCandle maps one row: [ts, o, h, l, c, vol, volCcy, volCcyQuote, confirm].
//
// COMPLETENESS COMES FROM THE VENUE, not from our clock. OKX marks an in-flight
// candle with confirm="0", which is strictly better than comparing a close time
// against local time — no clock skew, no guess.
//
// TRADE COUNT IS NOT REPORTED BY OKX, and this is a known conflation rather than
// an oversight: store.Bar.TradeCount is an int64 whose zero means "nothing
// traded in this interval" (bar.go says so explicitly), so an OKX candle records
// zero trades for a minute that may have had thousands. Anything reading
// trade_count will see OKX as permanently quiet. Making "not reported"
// expressible needs the field to become nullable end to end — migration, store,
// producer — which is #432, filed rather than smuggled in here because it
// changes a shipped migration and the live bar producer.
func okxCandle(row []string) (store.Bar, bool, error) {
	if len(row) < 9 {
		return store.Bar{}, false, fmt.Errorf("expected at least 9 fields, got %d", len(row))
	}
	ms, err := strconv.ParseInt(row[0], 10, 64)
	if err != nil {
		return store.Bar{}, false, fmt.Errorf("timestamp %q: %w", row[0], err)
	}

	vals := make([]*commonpb.Decimal, 0, 5)
	for i, name := range []string{"open", "high", "low", "close", "volume"} {
		// Index 5 is `vol`, the base-currency volume for a SPOT instrument —
		// which is what a candle's volume means everywhere else on this platform.
		idx := i + 1
		r, err := dec.ParseRat(row[idx])
		if err != nil {
			return store.Bar{}, false, fmt.Errorf("%s %q: %w", name, row[idx], err)
		}
		d, ok := dec.ToProtoScaled(r)
		if !ok {
			return store.Bar{}, false, fmt.Errorf("%s %s is not representable as an exact Decimal",
				name, r.RatString())
		}
		vals = append(vals, d)
	}

	return store.Bar{
		BucketStart: time.UnixMilli(ms).UTC(),
		Open:        vals[0],
		High:        vals[1],
		Low:         vals[2],
		Close:       vals[3],
		Volume:      vals[4],
		TradeCount:  0, // not reported by OKX — #432, and the doc comment above
	}, row[8] == "1", nil
}
