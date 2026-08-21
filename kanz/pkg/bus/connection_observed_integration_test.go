package bus_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/pkg/bus"
)

// LOSING THE SPINE IS OBSERVABLE WITHOUT ANYONE HAVING ASKED FOR IT (#636).
//
// # What this is really testing
//
// Not "the handlers work" — that they are installed AT ALL when the caller
// supplies nothing. Until #636, DialNATS installed DisconnectErrHandler and
// ClosedHandler only inside `if cfg.OnDisconnect != nil`, and 23 of 24
// composition roots under services/ left that field nil. So in twenty services
// the nats.Conn could close for good with the process still running, /readyz
// still answering 200, and not one line written anywhere: MaxReconnects
// defaults to -1 so there is no crash, and the Go NATS client logs nothing of
// its own.
//
// EVERY NATSConfig BELOW THEREFORE OMITS OnDisconnect AND OnReconnect. A test
// that supplied them would be exercising the one path that already worked, and
// would have passed for the whole time the defect was live. The omission is the
// experiment.
//
// # Why it scrapes rather than reading the collector
//
// startScrapeEndpoint, for the reason its own doc gives: "the family is exported
// with no series" is a property of the exposition output and of nothing else,
// and testutil.ToFloat64 was green while #283 was live. That distinction is
// load-bearing HERE in particular — the whole reason DialNATS sets the gauge to
// 1 on connect is that a metric which does not exist is not zero, so
// `kanz_bus_connected == 0` would match nothing and an alert over it would stay
// silent for exactly the process that never reported.
func TestIntegration_ConnectionStateIsObservedWithoutCallbacks(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("set TEST_NATS_URL to observe connection state against a real spine")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	reg := prometheus.NewRegistry()
	metrics := bus.NewBusMetrics(reg)
	scrape := startScrapeEndpoint(t, reg)

	const client = "conn-observed-it"
	c, err := bus.DialNATS(ctx, bus.NATSConfig{
		URL:     url,
		Name:    client,
		Metrics: metrics,
		// NO OnDisconnect. NO OnReconnect. See the doc comment: that is the point.
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	// UP, AND PRESENT IN THE EXPOSITION. Both halves matter: a healthy process
	// must export the series so that its absence later is meaningful.
	body := scrape(t)
	want := `kanz_bus_connected{client="` + client + `"} 1`
	if !strings.Contains(body, want) {
		t.Fatalf("after a successful dial the scrape does not contain %q.\n\n"+
			"A gauge with no series is not the same as a gauge reading 0: "+
			"`kanz_bus_connected == 0` matches nothing, so an alert over it stays silent for "+
			"precisely the process that never managed to report.\n\nscrape:\n%s",
			want, connectedLines(body))
	}

	// THE TERMINAL CASE, which is the one the pre-#636 comment named as "the one
	// disconnect that is permanent is the one we miss". Close() fires
	// ClosedHandler; with no OnDisconnect supplied, nothing before this change
	// installed that handler at all.
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// WAIT ON THE TERMINAL EVENT, NOT ON THE GAUGE. Close() drives BOTH handlers:
	// the client reports a disconnect first and only then reports the connection
	// closed for good. An earlier draft of this test polled until the gauge read
	// 0 and stopped there — which is satisfied by the DISCONNECT alone, so it
	// would have passed with ClosedHandler still uninstalled. That is precisely
	// the handler the pre-#636 comment singled out: "the one disconnect that is
	// permanent is the one we miss."
	wantEvent := `kanz_bus_connection_events_total{client="` + client + `",event="closed"} 1`
	deadline := time.Now().Add(5 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		last = scrape(t)
		if strings.Contains(last, wantEvent) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(last, wantEvent) {
		t.Errorf("scrape does not contain %q — the connection closed for good and the TERMINAL "+
			"handler never ran. A client that exhausts MaxReconnects closes without firing "+
			"DisconnectErrHandler again, so this is the case that leaves a process permanently "+
			"detached from the spine with nothing written anywhere.\n\nscrape:\n%s",
			wantEvent, connectedLines(last))
	}

	// The gauge ends at 0 whichever transition got there last.
	if !strings.Contains(last, `kanz_bus_connected{client="`+client+`"} 0`) {
		t.Errorf("the connection closed and kanz_bus_connected never went to 0.\n\n"+
			"This is the state twenty services shipped in: the nats.Conn is gone, the process is "+
			"still running, /readyz still answers 200, and nothing anywhere says so.\n\nscrape:\n%s",
			connectedLines(last))
	}

	// THE EVENT COUNTER IS NOT REDUNDANT WITH THE GAUGE. scrape_interval is 30s,
	// so a drop-and-recover inside one interval leaves the gauge reading 1 at
	// every scrape — flapping, the shape that precedes an outage, is invisible to
	// the gauge alone. A counter cannot miss an edge.
	wantDisconnect := `kanz_bus_connection_events_total{client="` + client + `",event="disconnected"} 1`
	if !strings.Contains(last, wantDisconnect) {
		t.Errorf("scrape does not contain %q — a disconnect shorter than one scrape interval "+
			"would leave no trace at all.\n\nscrape:\n%s", wantDisconnect, connectedLines(last))
	}
}

// THE CALLER'S HOOK STILL RUNS, and it runs IN ADDITION rather than instead.
//
// #636 moved the handlers out of `if cfg.OnDisconnect != nil`. The risk in that
// change is the opposite of the defect it fixes: webhook-ingest wires
// OnDisconnect to trip the kill-switch gate on bus loss, and a refactor that
// installed the default handler INSTEAD of the caller's would have silently
// disarmed the one service that was already doing the right thing.
func TestIntegration_OnDisconnectStillFiresAlongsideTheDefaults(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("set TEST_NATS_URL to observe connection state against a real spine")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	reg := prometheus.NewRegistry()
	metrics := bus.NewBusMetrics(reg)
	scrape := startScrapeEndpoint(t, reg)

	fired := make(chan error, 4)
	const client = "conn-hook-it"
	c, err := bus.DialNATS(ctx, bus.NATSConfig{
		URL:          url,
		Name:         client,
		Metrics:      metrics,
		OnDisconnect: func(err error) { fired <- err },
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		t.Fatal("OnDisconnect never fired on a closed connection — the #636 refactor replaced the " +
			"caller's hook instead of composing with it, which disarms webhook-ingest's " +
			"TripOnBusLoss and every future service that fails closed on bus loss")
	}

	// And the metric moved too, on the same event: the hook is additive, not a
	// substitute for the default observability.
	deadline := time.Now().Add(5 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		last = scrape(t)
		if strings.Contains(last, `kanz_bus_connected{client="`+client+`"} 0`) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("OnDisconnect fired but kanz_bus_connected did not go to 0 — supplying a hook must not "+
		"cost the default observability.\n\nscrape:\n%s", connectedLines(last))
}

// connectedLines trims a scrape to the families this test is about, so a failure
// shows the relevant three lines instead of several hundred.
func connectedLines(body string) string {
	var keep []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "kanz_bus_connected") || strings.HasPrefix(line, "kanz_bus_connection_events_total") {
			keep = append(keep, line)
		}
	}
	if len(keep) == 0 {
		return "  (no kanz_bus_connected* series in the exposition at all)"
	}
	return "  " + strings.Join(keep, "\n  ")
}
