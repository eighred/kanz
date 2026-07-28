package main

// The feed selector's job is to bind REAL exchange feeds. Its other job — the one
// worth a test — is to refuse to invent prices when it cannot.
//
// SimFeed does not read a market, it GENERATES prices, and those prices publish to
// the bus as market.v1 FACTs that risk, NAV and the OMS's pricing all mark
// against. It used to be a SILENT fallback: no exchange feed compiled in ⇒ swap in
// the simulator, log at Info, report healthy. That is "inject data" wearing a
// healthy dashboard, and it is exactly what the never-fabricate rule forbids.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/market-ingest/internal/config"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestFeedsRefusesToSimulateWhenNoSymbolsAreMapped(t *testing.T) {
	// Instruments requested, but no venue symbol mapped for any of them. The old
	// behaviour published generated prices. The correct behaviour is to refuse.
	cfg := config.Config{Instruments: []string{"BTC-USD"}}

	got, err := feeds(cfg, quietLogger())
	if err == nil {
		t.Fatal("feeds() silently substituted simulated prices for a real market feed — that is fabricated data on the bus")
	}
	if len(got) != 0 {
		t.Fatalf("feeds() returned %d feeds alongside the error", len(got))
	}
	if !strings.Contains(err.Error(), "SIMULATED") {
		t.Fatalf("the error must say plainly what it is refusing to do; got: %v", err)
	}
}

func TestFeedsSimulatesOnlyWhenExplicitlyAllowed(t *testing.T) {
	// Opt-in is still possible — for tests and local dev — but it must be asked
	// for, never arrived at by forgetting to configure a symbol map.
	cfg := config.Config{Instruments: []string{"BTC-USD"}, AllowSim: true}

	got, err := feeds(cfg, quietLogger())
	if err != nil {
		t.Fatalf("feeds() with AllowSim: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d feeds, want 1 sim feed", len(got))
	}
	if got[0].MIC != "SIM" {
		t.Fatalf("simulated feed is stamped MIC %q, want SIM — downstream must be able to tell it apart", got[0].MIC)
	}
}

func TestFeedsBindsTheRealVenueWhenASymbolIsMapped(t *testing.T) {
	// The exchange feeds are compiled in unconditionally now (no build tags). What
	// selects them is the symbol map — the runtime gate that was always there.
	cfg := config.Config{
		Instruments:    []string{"BTC-USD"},
		BinanceSymbols: map[string]string{"BTC-USD": "BTCUSDT"},
		BinanceMIC:     "BINANCE",
	}

	got, err := feeds(cfg, quietLogger())
	if err != nil {
		t.Fatalf("feeds(): %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d feeds, want 1", len(got))
	}
	if got[0].MIC == "SIM" {
		t.Fatal("a mapped instrument bound the SIMULATOR instead of the live venue")
	}
	if got[0].InstrumentID != "BTC-USD" {
		t.Fatalf("feed instrument = %q", got[0].InstrumentID)
	}
}

func TestFeedsWithNoInstrumentsIsNotAnError(t *testing.T) {
	// Nothing asked for, nothing ingested, nothing invented.
	got, err := feeds(config.Config{}, quietLogger())
	if err != nil {
		t.Fatalf("feeds() with no instruments: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d feeds, want 0", len(got))
	}
}

// TestConfigCarriesATenant guards a bug that made this service silently useless:
// bus.Validate REJECTS an envelope without a tenant_id, and market-ingest never
// set one. It connected to the exchange, folded the book correctly, and then
// failed EVERY publish with "tenant_id required" — while /readyz kept returning
// 200. It ingested perfectly and emitted nothing.
func TestConfigCarriesATenant(t *testing.T) {
	cfg := config.Load()
	if cfg.Tenant == "" {
		t.Fatal("no tenant configured — bus.Validate rejects every envelope and this service publishes NOTHING while reporting ready")
	}
}

// --- /readyz must reflect PUBLISH health, not just process liveness ---

type failingPub struct{ err error }

func (f *failingPub) Publish(context.Context, bus.Event) error { return f.err }

func TestReadyzGoesUnreadyWhenEveryPublishFails(t *testing.T) {
	// The bug this exists for: market-ingest folded its book perfectly, had every
	// envelope rejected, and answered /readyz 200 the whole time. Nothing it
	// produced ever reached the bus, and nothing noticed.
	health := bus.NewHealthPublisher(&failingPub{err: errors.New("envelope validation: tenant_id required")}, 3)
	ready := &readiness{}
	ready.set(true) // startup completed — the process is perfectly "alive"

	mux := healthMux(ready, health, nil)

	// Still healthy before the failures accumulate.
	if code := probe(t, mux, "/readyz"); code != http.StatusOK {
		t.Fatalf("/readyz = %d before any publish, want 200", code)
	}

	for i := 0; i < 3; i++ {
		_ = health.Publish(context.Background(), bus.Event{})
	}

	code, body := probeBody(t, mux, "/readyz")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz = %d after every publish failed, want 503 — the pod would stay in its Service emitting nothing", code)
	}
	// And it must say WHY, or an operator has to go dig through logs for what this
	// endpoint already knows.
	if !strings.Contains(body, "publish failures") || !strings.Contains(body, "tenant_id") {
		t.Fatalf("/readyz body does not explain the failure: %q", body)
	}

	// Liveness stays OK: restarting the pod does not fix a bad tenant, and a
	// crash-loop would only bury the reason.
	if code := probe(t, mux, "/livez"); code != http.StatusOK {
		t.Fatalf("/livez = %d, want 200 — a restart cannot fix a misconfigured envelope", code)
	}
}

func TestReadyzRecoversWhenPublishingRecovers(t *testing.T) {
	inner := &failingPub{err: errors.New("nats: connection lost")}
	health := bus.NewHealthPublisher(inner, 2)
	ready := &readiness{}
	ready.set(true)
	mux := healthMux(ready, health, nil)

	for i := 0; i < 2; i++ {
		_ = health.Publish(context.Background(), bus.Event{})
	}
	if code := probe(t, mux, "/readyz"); code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz = %d while the bus is down, want 503", code)
	}

	inner.err = nil // the bus came back
	_ = health.Publish(context.Background(), bus.Event{})

	if code := probe(t, mux, "/readyz"); code != http.StatusOK {
		t.Fatalf("/readyz = %d after publishing recovered, want 200 — the pod would need a restart to rejoin its Service", code)
	}
}

func probe(t *testing.T, h http.Handler, path string) int {
	t.Helper()
	code, _ := probeBody(t, h, path)
	return code
}

func probeBody(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code, rec.Body.String()
}
