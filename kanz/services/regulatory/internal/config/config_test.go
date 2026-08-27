package config

import (
	"strings"
	"testing"
)

// THE TWO LISTENERS MUST DIFFER, AND THE REFUSAL IS AT STARTUP (#765).
//
// The filing routes used to share a port with /metrics, and
// allow-observability-scrape admits that port across every pod in
// kanz-services. A filing is not a read — it is signed and appends a link to the
// AUDIT-01 hash chain — so the monitoring plane could write to the compliance
// record. The split fixes it; this refusal is what stops a deployment undoing it
// with one environment variable.
//
// It refuses rather than warns for the reason the estate refuses everywhere
// else on this shape: a service that started anyway would look identical in
// every log and dashboard to one that was configured correctly.

func TestListenersMustDiffer(t *testing.T) {
	t.Setenv("REGULATORY_LISTEN", ":8099")
	t.Setenv("REGULATORY_METRICS_LISTEN", ":8099")

	_, err := Load()
	if err == nil {
		t.Fatal("Load accepted an identical API and metrics listener — that puts the filing routes " +
			"back on the port allow-observability-scrape admits namespace-wide, which is #765 " +
			"exactly")
	}
	// The message has to name the cause, not merely fail: an operator reading it
	// at 3am needs to know which variable and why it matters.
	for _, want := range []string{"REGULATORY_LISTEN", "REGULATORY_METRICS_LISTEN", "8099"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// THE DEFAULTS ARE ALREADY SPLIT, so a deployment that sets neither variable is
// correct rather than merely unrefused. This is the arm that would fail if the
// defaults were ever collapsed back onto one port.
func TestDefaultListenersAreSplitAndMetricsKeepsTheScrapedPort(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load with defaults: %v", err)
	}
	if cfg.Listen == cfg.MetricsListen {
		t.Fatalf("default listeners are both %q", cfg.Listen)
	}
	// THE "IS THIS PORT SCRAPED" QUESTION IS NOT ASKED HERE, deliberately. This
	// package cannot see allow-observability-scrape, so an assertion like
	// `Listen != ":8083"` would name ONE bad port and pass for every other bad
	// one — a mutation moving the default to :8084 (web-bff's, also admitted)
	// survived exactly that check. The real property is owned by
	// test/arch/split_listener_api_is_off_the_scrape_list_test.go, which reads
	// the port list out of the manifest.
	if cfg.MetricsListen != ":8083" {
		t.Errorf("metrics listener = %q, want :8083 — moving it instead of the API would need a "+
			"NEW port admitted to allow-observability-scrape for no gain", cfg.MetricsListen)
	}
}

// NON-VACUITY: an explicitly split pair is accepted. Without this, the refusal
// above is satisfied by a Load that rejects everything.
func TestExplicitlySplitListenersAreAccepted(t *testing.T) {
	t.Setenv("REGULATORY_LISTEN", ":9101")
	t.Setenv("REGULATORY_METRICS_LISTEN", ":9102")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load rejected a validly split pair: %v", err)
	}
	if cfg.Listen != ":9101" || cfg.MetricsListen != ":9102" {
		t.Fatalf("listeners = %q / %q, want :9101 / :9102", cfg.Listen, cfg.MetricsListen)
	}
}
