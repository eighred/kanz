package arch

import (
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"
)

// THE BACKLOG POLLER'S INTERVAL IS ONLY MEANINGFUL RELATIVE TO THE SCRAPE
// INTERVAL, SO THE TWO MUST NOT DRIFT.
//
// pkg/bus/backlog.go asserts, at COMPILE time, that backlogPollInterval does not
// exceed metricsScrapeInterval — invert them and the package stops building.
// That assertion is only as good as metricsScrapeInterval, which is a 30s
// constant copied out of infra/observability/prometheus.yaml. A number copied
// from a config file is exactly the dated evidence AGENTS.md warns about: raise
// the scrape interval in the YAML and the Go constant keeps asserting against a
// value the estate no longer uses, while the compiler goes on reporting success.
//
// This test is the missing half. The compiler owns the ORDERING; this owns the
// PREMISE.
//
// Why the ordering matters at all: if the poll were slower than the scrape,
// consecutive scrapes would read the same poll and the series would be a
// staircase whose samples are older than they look. KEDA then reads it a third
// time removed — its own pollingInterval queries Prometheus — so the poll term
// is the one this repository controls and the one that should stay smallest.
func TestBacklogPollIntervalMatchesTheScrapeInterval(t *testing.T) {
	root := moduleRoot(t)

	src := readFile(t, filepath.Join(root, "pkg", "bus", "backlog.go"))
	declared := goDurationConst(t, src, "metricsScrapeInterval")
	poll := goDurationConst(t, src, "backlogPollInterval")
	timeout := goDurationConst(t, src, "backlogPollTimeout")

	promYAML := readFile(t, filepath.Join(root, "infra", "observability", "prometheus.yaml"))
	m := regexp.MustCompile(`(?m)^\s*scrape_interval:\s*([0-9]+)([smh])\s*$`).FindStringSubmatch(promYAML)
	if m == nil {
		t.Fatal("no `scrape_interval:` in infra/observability/prometheus.yaml — the scanner is broken, or " +
			"the global scrape interval moved. pkg/bus/backlog.go's metricsScrapeInterval is derived from it " +
			"and is now unanchored.")
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("scrape_interval %q: %v", m[0], err)
	}
	units := map[string]time.Duration{"s": time.Second, "m": time.Minute, "h": time.Hour}
	actual := time.Duration(n) * units[m[2]]

	if declared != actual {
		t.Errorf("pkg/bus/backlog.go declares metricsScrapeInterval = %s but "+
			"infra/observability/prometheus.yaml scrapes every %s.\n\n"+
			"The compile-time assertion in backlog.go checks backlogPollInterval against the Go constant, so "+
			"a stale constant means the ordering is being asserted against a scrape interval this estate does "+
			"not use — the guard reports success while the backlog series it protects goes stale between "+
			"scrapes. Update the constant with the YAML, or the YAML with the constant.",
			declared, actual)
	}

	// Restating the compile-time orderings here is deliberate. They cannot fail
	// independently — the package would not build — but a future edit that
	// deletes the `const _ = uint(...)` lines to "clean up" would silently take
	// the whole relationship with it, and this is what notices.
	if poll > actual {
		t.Errorf("backlogPollInterval (%s) exceeds the scrape interval (%s): every scrape would not see a "+
			"fresh poll", poll, actual)
	}
	if timeout >= poll {
		t.Errorf("backlogPollTimeout (%s) is not shorter than backlogPollInterval (%s): one hung poll would "+
			"outlive its own slot and the series would go stale with no failure recorded", timeout, poll)
	}
}

// goDurationConst reads `name = <n> * time.<Unit>` out of Go source.
func goDurationConst(t *testing.T, src, name string) time.Duration {
	t.Helper()
	re := regexp.MustCompile(`(?m)^\s*(?:const\s+)?` + regexp.QuoteMeta(name) + `\s*=\s*([0-9]+)\s*\*\s*time\.(Second|Minute|Hour)\b`)
	m := re.FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("no `%s = N * time.Unit` declaration in pkg/bus/backlog.go — it was renamed or its form "+
			"changed, and this guard now checks nothing", name)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	switch m[2] {
	case "Second":
		return time.Duration(n) * time.Second
	case "Minute":
		return time.Duration(n) * time.Minute
	default:
		return time.Duration(n) * time.Hour
	}
}
