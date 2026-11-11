package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// metricsSnippet is a captured-shape /metrics body: the real counter lines
// this monitor reads, in Prometheus exposition format. It deliberately:
//   - splits kanz_compliance_unpriced_orders_total across two `reason` label
//     values, to prove parseCounters sums across labels for the tile;
//   - OMITS kanz_oms_unverified_venue_account_total entirely, to prove a
//     metric a fresh pod hasn't incremented yet reads as 0, not an error —
//     the pod exports nothing for a counter until its first Inc().
const metricsSnippet = `# HELP kanz_oms_orders_quarantined_total Orders frozen because the platform could not establish what the venue did with them.
# TYPE kanz_oms_orders_quarantined_total counter
kanz_oms_orders_quarantined_total 3
# HELP kanz_compliance_ungoverned_orders_total Orders admitted or refused for a portfolio no mandate governs.
# TYPE kanz_compliance_ungoverned_orders_total counter
kanz_compliance_ungoverned_orders_total 7
# HELP kanz_compliance_unpriced_orders_total Orders refused PRICE_UNAVAILABLE.
# TYPE kanz_compliance_unpriced_orders_total counter
kanz_compliance_unpriced_orders_total{reason="never_seen"} 2
kanz_compliance_unpriced_orders_total{reason="expired"} 5
# HELP kanz_oms_shared_collateral_orders_total Orders executed against an unbound exchange account.
# TYPE kanz_oms_shared_collateral_orders_total counter
kanz_oms_shared_collateral_orders_total 1
# HELP kanz_bus_publish_total messages published, by subject and result.
# TYPE kanz_bus_publish_total counter
kanz_bus_publish_total{subject="order.order.filled",result="ok"} 42
kanz_bus_publish_total{subject="order.order.rejected",result="ok"} 4
`

func TestParseCounters(t *testing.T) {
	got := parseCounters(metricsSnippet)

	want := counterSnapshot{
		Quarantined:            3,
		Ungoverned:             7,
		Unpriced:               7, // 2 (never_seen) + 5 (expired): summed across the reason label
		SharedCollateral:       1,
		UnverifiedVenueAccount: 0, // absent from the body entirely — must read 0, not error/panic
	}

	if got != want {
		t.Fatalf("parseCounters(metricsSnippet) = %+v, want %+v", got, want)
	}
}

func TestParseCounters_EmptyBody(t *testing.T) {
	// A fresh pod that has registered its collectors but incremented nothing
	// yet exports an empty (or near-empty) body. Every field must read 0.
	got := parseCounters("")
	want := counterSnapshot{}
	if got != want {
		t.Fatalf("parseCounters(\"\") = %+v, want zero value %+v", got, want)
	}
}

func TestParseCounters_IgnoresUnrelatedLines(t *testing.T) {
	body := `# HELP go_goroutines Number of goroutines.
# TYPE go_goroutines gauge
go_goroutines 42
kanz_oms_orders_quarantined_total 9
`
	got := parseCounters(body)
	if got.Quarantined != 9 {
		t.Fatalf("Quarantined = %d, want 9", got.Quarantined)
	}
	if got.Ungoverned != 0 || got.Unpriced != 0 || got.SharedCollateral != 0 || got.UnverifiedVenueAccount != 0 {
		t.Fatalf("unrelated metric lines leaked into the snapshot: %+v", got)
	}
}

// TestPoll_EmptyMetricsURLSkipsScrape proves that cfg.MetricsURL == "" skips
// the counter scrape entirely rather than dialing "/metrics" against an
// empty host: counters must stay at their zero value and poll() must not
// panic, even though cfg.GatewayURL is also unset (so the /readyz leg fails
// too — that failure is expected and reported via err, only the counter
// scrape is required to be silently skipped).
func TestPoll_EmptyMetricsURLSkipsScrape(t *testing.T) {
	m := newModel(Config{MetricsURL: "", GatewayURL: ""})

	msg := m.poll()

	if msg.counters != (counterSnapshot{}) {
		t.Fatalf("counters = %+v, want zero value when MetricsURL is unset", msg.counters)
	}
	if len(msg.health) != 0 {
		t.Fatalf("health = %+v, want empty when GatewayURL is unset", msg.health)
	}
}

// TestPoll_ScrapesCountersFromMetricsURL proves the counter scrape is
// pointed at cfg.MetricsURL (the OMS's own registry), not cfg.GatewayURL —
// the bug this fix corrects. A test server stands in for the OMS /metrics
// endpoint; cfg.GatewayURL is left unset so the /readyz leg fails (reported
// via err) without reaching out over the network, isolating the assertion
// to the counter scrape.
func TestPoll_ScrapesCountersFromMetricsURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(metricsSnippet))
	}))
	defer srv.Close()

	m := newModel(Config{MetricsURL: srv.URL, GatewayURL: ""})

	msg := m.poll()

	want := counterSnapshot{
		Quarantined:      3,
		Ungoverned:       7,
		Unpriced:         7,
		SharedCollateral: 1,
	}
	if msg.counters != want {
		t.Fatalf("counters = %+v, want %+v", msg.counters, want)
	}
}
