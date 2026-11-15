// The flag layer is the only thing between an operator's typo and a real series
// loaded under the wrong identity — and unlike a bad order, a bad backfill is
// silent: the rows look exactly like good ones, and the mistake surfaces months
// later as a model that will not reproduce.
//
// main.go wiring escapes every other test in this repo, and has shipped crashes
// twice with a fully green suite, so parseFlags and newSource are deliberately
// pure and tested directly.
package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/marketdata/backfill"
)

func wellFormed() []string {
	return []string{
		"--source", "binance",
		"--instrument", "BTC-USDT",
		"--symbol", "BTCUSDT",
		"--base-url", "https://api.binance.com",
		"--from", "2025-01-01T00:00:00Z",
		"--to", "2025-01-02T00:00:00Z",
	}
}

// without returns the well-formed argument list with one flag and its value
// removed, so a required-flag case cannot pass by also breaking something else.
func without(flag string) []string {
	args := wellFormed()
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == flag {
			i++ // skip its value too
			continue
		}
		out = append(out, args[i])
	}
	return out
}

func TestEveryIdentifyingFlagIsRequired(t *testing.T) {
	// Each of these names part of WHAT is being loaded or WHERE FROM. A default
	// for any of them would be a guess written into a durable series.
	for _, flag := range []string{"--source", "--instrument", "--symbol", "--base-url", "--from", "--to"} {
		t.Run(flag, func(t *testing.T) {
			_, err := parseFlags(without(flag))
			if err == nil {
				t.Fatalf("%s was accepted as absent", flag)
			}
			if !strings.Contains(err.Error(), strings.TrimPrefix(flag, "--")) {
				t.Errorf("the error does not name the missing flag: %v", err)
			}
		})
	}
}

// THE VENUE DEFAULT IS THE JOIN. Venue is part of the bar's primary key, so
// history stamped with a different MIC than the live producer uses is a separate
// series that never joins — and the seam is invisible, because both halves look
// complete and a window spanning the switchover silently returns one of them.
func TestVenueDefaultsToWhatTheLiveProducerStamps(t *testing.T) {
	for _, tc := range []struct{ source, want string }{
		// The live producer's defaults, in market-ingest's config.Load.
		{"binance", "BINANCE"}, // BinanceMIC
		{"okx", "OKX"},         // OKXMIC
	} {
		args := without("--source")
		args = append(args, "--source", tc.source)
		opt, err := parseFlags(args)
		if err != nil {
			t.Fatalf("%s: %v", tc.source, err)
		}
		if opt.venue != tc.want {
			t.Errorf("%s venue = %q, want %q — backfilled history would not join the live series",
				tc.source, opt.venue, tc.want)
		}
	}
}

// An estate that overrode the live producer's MIC must get a backfill that still
// joins, which is why the default reads the same variable rather than hardcoding.
func TestVenueDefaultFollowsTheLiveProducersOverride(t *testing.T) {
	t.Setenv("MARKET_INGEST_BINANCE_MIC", "XBIN")

	opt, err := parseFlags(wellFormed())
	if err != nil {
		t.Fatal(err)
	}
	if opt.venue != "XBIN" {
		t.Errorf("venue = %q, want XBIN — the backfill ignored the MIC the live producer was told "+
			"to use, so the two would write different series", opt.venue)
	}
}

func TestExplicitVenueWins(t *testing.T) {
	opt, err := parseFlags(append(wellFormed(), "--venue", "XNAS"))
	if err != nil {
		t.Fatal(err)
	}
	if opt.venue != "XNAS" {
		t.Errorf("venue = %q, want XNAS", opt.venue)
	}
}

func TestRefusesAnUnknownSource(t *testing.T) {
	args := without("--source")
	args = append(args, "--source", "coinbase")
	if _, err := parseFlags(args); err == nil {
		t.Fatal("a venue with no adapter was accepted; the run would fail only after connecting")
	}
}

// An inverted or empty window is caught here rather than after opening a pool
// and paying for a connection to do nothing.
func TestRefusesAnInvertedWindow(t *testing.T) {
	args := without("--to")
	args = append(args, "--to", "2024-01-01T00:00:00Z") // before --from
	_, err := parseFlags(args)
	if err == nil {
		t.Fatal("a window ending before it starts was accepted")
	}
	if !strings.Contains(err.Error(), "inverted") {
		t.Errorf("the error does not explain the window: %v", err)
	}
}

func TestRefusesANonRFC3339Time(t *testing.T) {
	args := without("--from")
	args = append(args, "--from", "2025-01-01")
	if _, err := parseFlags(args); err == nil {
		t.Fatal("a date without a timezone was accepted — it would be read in an unstated zone")
	}
}

func TestWellFormedFlagsRoundTrip(t *testing.T) {
	opt, err := parseFlags(wellFormed())
	if err != nil {
		t.Fatalf("well-formed flags rejected: %v", err)
	}
	if opt.instrument != "BTC-USDT" || opt.symbol != "BTCUSDT" {
		t.Errorf("instrument/symbol = %q/%q; the platform id and the exchange symbol must stay "+
			"distinct (#407)", opt.instrument, opt.symbol)
	}
	if !opt.from.Equal(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("from = %s", opt.from)
	}
	if opt.timeout <= 0 {
		t.Errorf("timeout = %s, want a positive default", opt.timeout)
	}
}

// newSource must hand back the adapter the operator named. Handing back the
// wrong one would parse a different venue's row layout and either error loudly
// or, worse, map fields that happen to line up.
func TestNewSourceSelectsTheNamedAdapter(t *testing.T) {
	b, err := newSource(options{source: "binance", baseURL: "https://api.binance.com"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := b.(*backfill.BinanceSource); !ok {
		t.Errorf("--source binance built %T", b)
	}

	o, err := newSource(options{source: "okx", baseURL: "https://www.okx.com"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := o.(*backfill.OKXSource); !ok {
		t.Errorf("--source okx built %T", o)
	}
}

// A RESTATEMENT MUST NOT READ AS ROUTINE. It means the venue changed history
// under a model that may already have trained on it, and a summary line that
// buried it among the other counts is how it gets scrolled past.
func TestReportCallsOutARestatement(t *testing.T) {
	var buf bytes.Buffer
	report(&buf, backfill.Result{Fetched: 1440, Written: 12, Unchanged: 1428, Restated: 12})

	got := buf.String()
	if !strings.Contains(got, "RESTATED") {
		t.Errorf("a restatement was reported as an ordinary count:\n%s", got)
	}
	if !strings.Contains(got, "knowledge_time") {
		t.Errorf("the report does not tell the operator the old values are still there:\n%s", got)
	}
}

func TestReportStaysQuietWhenNothingWasRestated(t *testing.T) {
	var buf bytes.Buffer
	report(&buf, backfill.Result{Fetched: 1440, Written: 1440, Unchanged: 0})

	if strings.Contains(buf.String(), "RESTATED") {
		t.Errorf("a clean run raised a restatement alarm:\n%s", buf.String())
	}
}
