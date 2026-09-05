package promscrape

import (
	"bufio"
	"strings"
	"testing"
)

// The exposition subset this harness parses, exercised against the shapes the
// OMS actually exports. A parser that quietly returned an empty scrape would make
// every refusal fire for the wrong reason — safe, but it would send whoever read
// it to the wrong process.

const omsSample = `# HELP kanz_oms_live_venue_adapters Out-of-process venue.v1 adapters.
# TYPE kanz_oms_live_venue_adapters gauge
kanz_oms_live_venue_adapters 0
kanz_oms_simulated_venues 1
kanz_bus_pending_messages{group="oms",subject="order.order.amend"} 3
kanz_bus_pending_messages{group="oms",subject="order.order.submit"} 158
kanz_bus_pending_messages{group="oms-tca",subject="order.order.filled"} 7
kanz_compliance_ungoverned_orders_total 10
`

func TestParseScrapeReadsLabelledAndUnlabelledSeries(t *testing.T) {
	s, err := Parse(bufio.NewScanner(strings.NewReader(omsSample)))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if v, ok := s.Value("kanz_oms_simulated_venues"); !ok || v != 1 {
		t.Errorf("kanz_oms_simulated_venues = %v (present=%v), want 1", v, ok)
	}
	if _, ok := s.Value("kanz_oms_venue_margin_uncovered_total"); ok {
		t.Error("a family the endpoint never exported was reported as present — absent and zero must " +
			"stay distinguishable, because every refusal in preflight.go turns on it")
	}
	got, matched, present := s.Sum("kanz_bus_pending_messages",
		`group="oms"`, `subject="order.order.submit"`)
	if !present || matched != 1 || got != 158 {
		t.Errorf("the order-command backlog = %v (matched=%d present=%v), want 158 from exactly one "+
			"series — a filter that also caught the amend or the -tca group would report a backlog "+
			"that is not the one the write path queues on", got, matched, present)
	}
}

// A BODY THIS PARSER CANNOT READ IS AN ERROR. An HTML error page, a proxy's login
// form or a truncated response would otherwise parse to an empty scrape, which
// every caller reads as "the metric is absent".
func TestUnreadableMetricsBodiesAreErrors(t *testing.T) {
	for _, tt := range []struct{ name, text string }{
		{"an HTML error page", "<html><body>502 Bad Gateway</body></html>\n"},
		{"a value that is not a number", "kanz_oms_simulated_venues one\n"},
		{"nothing but comments", "# HELP x y\n# TYPE x gauge\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Parse(bufio.NewScanner(strings.NewReader(tt.text))); err == nil {
				t.Fatal("parsed without error, so the caller would see an empty scrape and report " +
					"every metric as absent")
			}
		})
	}
}
