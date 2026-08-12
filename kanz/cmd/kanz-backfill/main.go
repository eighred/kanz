// Command kanz-backfill loads a venue's historical 1-minute candles into the
// bar store.
//
// WHY IT EXISTS (#425, A3). The live producer folds trades into candles from the
// moment it starts (internal/marketedge/bars), so the series begins the day the
// process did. Every question the alpha direction is made of — does this signal
// work, what did this indicator say last March, would this strategy have made
// money — needs history the platform was not running for. Without this, the
// answer to all of them is "we have four days of data".
//
// Load a year of Binance BTC/USDT:
//
//	kanz-backfill --source binance --instrument BTC-USDT --symbol BTCUSDT \
//	  --base-url https://api.binance.com \
//	  --from 2025-01-01T00:00:00Z --to 2026-01-01T00:00:00Z
//
// Do a day first and read the summary — recommended on any series you have not
// loaded before, because it is the cheapest way to find out that --symbol or
// --venue is wrong while only a day of rows is affected.
//
// RE-RUNNING IS SAFE, AND IS THE POINT. The run writes only candles the store
// does not already agree with, so re-running a loaded range writes nothing and
// reports it as unchanged. An interrupted run is resumed by running it again;
// there is no partial state to clean up.
//
// A RESTATED COUNT ABOVE ZERO IS NOT ROUTINE — it means the venue answered
// differently than it did before, so history changed under a model that may
// already have trained on it. The old value is still stored under its original
// knowledge_time precisely so the two can be compared. Investigate before
// loading more.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/eighred/kanz/internal/marketdata/backfill"
	"github.com/eighred/kanz/internal/marketdata/store"
	"github.com/eighred/kanz/internal/pg"
	"github.com/eighred/kanz/pkg/secret"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "kanz-backfill: %v\n", err)
		os.Exit(1)
	}
}

type options struct {
	source     string
	instrument string
	symbol     string
	venue      string
	baseURL    string
	from       time.Time
	to         time.Time
	timeout    time.Duration
}

func run(args []string, out io.Writer) error {
	opt, err := parseFlags(args)
	if err != nil {
		return err
	}

	src, err := newSource(opt)
	if err != nil {
		return err
	}

	// SIGINT/SIGTERM cancels rather than kills. A long backfill is a thing an
	// operator interrupts, and cancelling mid-run is safe here for the same
	// reason re-running is: every candle already written is durable, and the
	// next run re-reads the range and writes only what is still missing.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, opt.timeout)
	defer cancel()

	// The SAME DSN as the service that owns the table — a backfill writing to a
	// different database than market-data reads from would report success against
	// rows nothing will ever serve.
	dsn, err := secret.Read("MARKET_DATA_DATABASE_URL")
	if err != nil {
		return err
	}
	if dsn == "" {
		return errors.New("no DSN: set MARKET_DATA_DATABASE_URL_FILE (preferred) or " +
			"MARKET_DATA_DATABASE_URL — the same database market-data reads from")
	}

	// GLOBAL, NOT TENANT-SCOPED, matching the table: a candle is universal market
	// fact, not tenant-owned state (services/market-data/migrations/0003_ohlcv_bars.sql).
	pool, err := pg.NewGlobalPool(ctx, dsn,
		"ohlcv_bars is not tenant-scoped: a venue's candles are market fact, identical for every tenant")
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	bf, err := backfill.New(store.NewPostgres(pool), time.Now)
	if err != nil {
		return err
	}

	series := backfill.Series{
		InstrumentID: opt.instrument,
		Symbol:       opt.symbol,
		Venue:        opt.venue,
	}
	fmt.Fprintf(out, "loading %s (%s on %s) from %s to %s\n",
		series.InstrumentID, series.Symbol, series.Venue,
		opt.from.Format(time.RFC3339), opt.to.Format(time.RFC3339))

	res, err := bf.Run(ctx, src, series, opt.from, opt.to)
	// The counts are printed even on failure: a run that fetched 400k candles and
	// then lost the connection still wrote most of them, and an operator deciding
	// what to do next needs to know that rather than assume nothing happened.
	report(out, res)
	if err != nil {
		return err
	}
	return nil
}

func report(out io.Writer, res backfill.Result) {
	fmt.Fprintf(out, "fetched %d, wrote %d, unchanged %d\n", res.Fetched, res.Written, res.Unchanged)
	if res.Restated > 0 {
		// LOUD, because it is the one outcome that is not routine.
		fmt.Fprintf(out, "\nRESTATED %d candle(s): the venue answered differently than it did "+
			"before.\nHistory changed under anything already trained on it. The previous values are "+
			"still\nstored under their original knowledge_time — compare them before loading more.\n",
			res.Restated)
	}
}

// newSource builds the venue adapter. Kept separate from run() so a test can
// reach it without a database.
func newSource(opt options) (backfill.Source, error) {
	switch opt.source {
	case "binance":
		return backfill.NewBinanceSource(opt.baseURL)
	case "okx":
		return backfill.NewOKXSource(opt.baseURL)
	default:
		return nil, fmt.Errorf("--source %q is not a venue this tool can read; want binance or okx",
			opt.source)
	}
}

// defaultVenueMIC returns the MIC the LIVE producer stamps for this source.
//
// THE DEFAULT IS NOT COSMETIC. Venue is part of the bar's primary key, so
// backfilled history stamped "XBIN" and live candles stamped "BINANCE" are two
// different series that never join — and the seam is invisible: both halves look
// complete, and a window query spanning the switchover silently returns only
// one. This reads the SAME environment variable market-ingest reads
// (services/market-ingest/internal/config/config.go), with the same fallback, so
// an estate that overrode the MIC gets a backfill that still joins.
func defaultVenueMIC(source string) string {
	switch source {
	case "binance":
		return envOr("MARKET_INGEST_BINANCE_MIC", "BINANCE")
	case "okx":
		return envOr("MARKET_INGEST_OKX_MIC", "OKX")
	default:
		return ""
	}
}

func parseFlags(args []string) (options, error) {
	var opt options
	var from, to string

	fs := flag.NewFlagSet("kanz-backfill", flag.ContinueOnError)
	fs.StringVar(&opt.source, "source", "", "the venue to read history from: binance or okx — REQUIRED.")
	fs.StringVar(&opt.instrument, "instrument", "", "the PLATFORM's canonical instrument id, e.g. BTC-USDT — REQUIRED.\n"+
		"\tThis is what an order carries. It is not always what the exchange calls\n"+
		"\tthe pair, which is why --symbol is separate (#407).")
	fs.StringVar(&opt.symbol, "symbol", "", "the EXCHANGE's symbol for the same pair, e.g. BTCUSDT on Binance,\n"+
		"\tBTC-USDT on OKX — REQUIRED. Getting this wrong loads a real series under\n"+
		"\tthe wrong instrument id, which looks like data until someone trades on it.")
	fs.StringVar(&opt.baseURL, "base-url", "", "the venue's API base URL, e.g. https://api.binance.com — REQUIRED,\n"+
		"\tno default. It selects the environment, and OKX in particular serves demo\n"+
		"\tand production from the same host (#147). A series loaded from the wrong\n"+
		"\tone is indistinguishable afterwards.")
	fs.StringVar(&opt.venue, "venue", "", "the MIC to store these candles under. Defaults to what the LIVE\n"+
		"\tproducer stamps for --source, which is what makes backfilled history and\n"+
		"\tlive candles one series. Override only if you know why.")
	fs.StringVar(&from, "from", "", "start of the window, RFC3339, inclusive — REQUIRED. e.g. 2025-01-01T00:00:00Z")
	fs.StringVar(&to, "to", "", "end of the window, RFC3339, EXCLUSIVE — REQUIRED.")
	fs.DurationVar(&opt.timeout, "timeout", 2*time.Hour, "deadline for the whole run.\n"+
		"\tDeliberately generous: requests are paced to stay well inside the venue's\n"+
		"\trate budget, which the LIVE adapters share, and a year of minutes is\n"+
		"\tthousands of pages on OKX. An interrupted run is resumed by re-running it.")

	if err := fs.Parse(args); err != nil {
		return options{}, err
	}

	switch {
	case opt.source == "":
		return options{}, errors.New("--source is required: binance or okx")
	case opt.instrument == "":
		return options{}, errors.New("--instrument is required: the platform's canonical id, e.g. BTC-USDT")
	case opt.symbol == "":
		return options{}, errors.New("--symbol is required: the exchange's own symbol, e.g. BTCUSDT")
	case opt.baseURL == "":
		return options{}, errors.New("--base-url is required and has no default: it selects the " +
			"environment, and history loaded from the wrong one is indistinguishable afterwards")
	case from == "":
		return options{}, errors.New("--from is required, RFC3339, e.g. 2025-01-01T00:00:00Z")
	case to == "":
		return options{}, errors.New("--to is required, RFC3339, e.g. 2026-01-01T00:00:00Z")
	}

	if opt.source != "binance" && opt.source != "okx" {
		return options{}, fmt.Errorf("--source %q is not a venue this tool can read; want binance or okx",
			opt.source)
	}

	var err error
	if opt.from, err = time.Parse(time.RFC3339, from); err != nil {
		return options{}, fmt.Errorf("--from %q is not RFC3339: %w", from, err)
	}
	if opt.to, err = time.Parse(time.RFC3339, to); err != nil {
		return options{}, fmt.Errorf("--to %q is not RFC3339: %w", to, err)
	}
	if !opt.from.Before(opt.to) {
		return options{}, fmt.Errorf("--from %s is not before --to %s: the window is empty or inverted",
			from, to)
	}
	if opt.timeout <= 0 {
		return options{}, fmt.Errorf("--timeout %s is not positive", opt.timeout)
	}

	if opt.venue == "" {
		opt.venue = defaultVenueMIC(opt.source)
	}
	return opt, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
