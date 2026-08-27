package arch

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A SERVICE THAT SPLIT ITS LISTENERS MUST KEEP ITS API OFF THE SCRAPED PORT
// (#232, #765).
//
// # The failure this was written for
//
// allow-observability-scrape has `podSelector: {}` — EVERY pod in kanz-services
// — and admits a list of ports from the kanz-observability namespace. Any
// service serving its API on one of those ports is reachable by the whole
// monitoring plane, whatever that API does.
//
// Four services have paid to leave: optimization (#409), accounting (#447),
// audit (#627) and mcp (#743). regulatory was the fifth and had not: it served
// five POST /v1/filings/* routes on :8083, which this rule admits for audit's
// metrics — and a filing is not a read. It is signed and appends a link to the
// AUDIT-01 hash chain, so a pod in the monitoring namespace could write to the
// compliance record, against a service that reads no principal header and so had
// no authentication step to fail (#765).
//
// # Why this guard binds only the services that split
//
// Most of the estate serves its API on a scraped port, and that is not
// automatically wrong — a read surface behind the gateway with nothing
// tenant-scoped on it is a different risk from a filing route. A blanket rule
// would need an exemption for half the services, and a rule mostly made of
// exemptions is one nobody reads.
//
// So the scope is DECLARED BY THE SERVICE ITSELF: a manifest that sets both a
// metrics listener and an API listener has already decided its API must live
// somewhere private. This guard holds it to that decision. Nothing has to be
// added here when a fifth service splits — it comes under the rule by splitting,
// which is the same self-selecting shape
// governed_measure_reaches_the_agent_test.go uses for agent-facing planes.
//
// # The two idioms, both matched
//
//	X_LISTEN + X_METRICS_LISTEN   API is X_LISTEN      (accounting, mcp, optimization, regulatory)
//	X_LISTEN + X_API_LISTEN       API is X_API_LISTEN  (audit, compliance)
//
// Missing either would silently shrink the guard to the services using the other
// spelling, which is how a rule ends up watching half of what its name claims.
var (
	envListen        = regexp.MustCompile(`name:\s*([A-Z0-9_]+)_LISTEN,\s*value:\s*"?:(\d+)"?`)
	envMetricsListen = regexp.MustCompile(`name:\s*([A-Z0-9_]+)_METRICS_LISTEN,\s*value:\s*"?:(\d+)"?`)
	envAPIListen     = regexp.MustCompile(`name:\s*([A-Z0-9_]+)_API_LISTEN,\s*value:\s*"?:(\d+)"?`)
	scrapePortLine   = regexp.MustCompile(`-\s*\{\s*protocol:\s*TCP,\s*port:\s*(\d+)\s*\}`)
)

func TestASplitServiceKeepsItsAPIOffTheScrapeList(t *testing.T) {
	root := moduleRoot(t)
	scraped := scrapeAdmittedPorts(t, root)

	// NON-VACUITY, first half. An empty port set would clear every service.
	if len(scraped) < 8 {
		t.Fatalf("allow-observability-scrape parsed to %d port(s) %v — the rule lists far more, so "+
			"the parse is broken rather than the estate", len(scraped), sortedIntList(scraped))
	}

	dir := filepath.Join(root, "infra", "deploy")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	split := map[string]string{} // service file -> "apiPort"
	var problems []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr != nil {
			t.Fatalf("read %s: %v", e.Name(), rerr)
		}
		body := string(b)

		listen := envListen.FindStringSubmatch(body)
		metrics := envMetricsListen.FindStringSubmatch(body)
		apiListen := envAPIListen.FindStringSubmatch(body)

		var apiPort, why string
		switch {
		case apiListen != nil:
			// X_LISTEN serves metrics; X_API_LISTEN serves the API.
			apiPort, why = apiListen[2], apiListen[1]+"_API_LISTEN"
		case metrics != nil && listen != nil:
			// X_METRICS_LISTEN serves metrics; X_LISTEN serves the API.
			apiPort, why = listen[2], listen[1]+"_LISTEN"
		default:
			continue // not a split service; out of scope by its own declaration
		}
		split[e.Name()] = apiPort
		if scraped[apiPort] {
			problems = append(problems, e.Name()+" declares split listeners but serves its API on :"+
				apiPort+" via "+why+", which allow-observability-scrape admits across every pod in "+
				"kanz-services — so the whole kanz-observability namespace can reach it, which is "+
				"the exposure the split was made to close")
		}
	}

	// NON-VACUITY, second half. Six services have split; finding none means the
	// env spellings changed and this guard is watching nothing.
	if len(split) < 5 {
		t.Fatalf("found %d split service(s) %v — accounting, audit, compliance, mcp, optimization "+
			"and regulatory all declare two listeners, so the scan is broken", len(split), sortedKeys(split))
	}

	sort.Strings(problems)
	for _, p := range problems {
		t.Error(p)
	}
}

// scrapeAdmittedPorts reads the port list out of allow-observability-scrape.
func scrapeAdmittedPorts(t *testing.T, root string) map[string]bool {
	t.Helper()
	path := filepath.Join(root, "infra", "security", "runtime", "network-policies.yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read network-policies.yaml: %v", err)
	}
	docs := strings.Split(string(b), "\n---")
	for _, d := range docs {
		if !strings.Contains(d, "name: allow-observability-scrape") {
			continue
		}
		out := map[string]bool{}
		for _, m := range scrapePortLine.FindAllStringSubmatch(d, -1) {
			out[m[1]] = true
		}
		return out
	}
	t.Fatal("allow-observability-scrape is not in network-policies.yaml — it was renamed or " +
		"removed, and every service would then look clear of a rule that no longer exists")
	return nil
}

func sortedIntList(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
