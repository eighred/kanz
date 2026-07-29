package arch

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// EVERY DEPLOYABLE SERVICE IS SCRAPABLE, OR IT SAYS WHY NOT — IN WRITING.
//
// The defect this guards is not "a service is unmonitored". It is "a service
// LOOKS monitored and is not", which is strictly worse: an unmonitored service
// is a known gap, while a service carrying a scrape annotation that points at
// the wrong port produces a target Prometheus reports as DOWN in a list nobody
// reads, or — if the annotation is missing entirely — no target at all, which
// appears nowhere. Neither failure raises anything. Both look like a healthy
// estate from every surface an operator actually consults.
//
// #61's evidence was that infra/observability/ ships alerts/, dashboards/ and
// slo/ while NOTHING scrapes: no kind: Prometheus, no prom/prometheus image, and
// exactly one prometheus.io/scrape annotation in the whole repository — on
// node-exporter, which is not one of the services. Every degraded mode the SLO
// rules compute over was unobservable, and had been for the life of those files.
//
// WHAT THIS GUARD ASSERTS.
//
// For every service that is DEPLOYABLE — the filesystem's list, minus the
// written exemptions in notDeployed — three things must hold together:
//
//  1. its workload manifest carries prometheus.io/scrape: "true"
//  2. its prometheus.io/port matches a containerPort declared in that same
//     manifest
//  3. the service actually registers /metrics in Go
//
// Assertion 2 exists because a wrong port fails SILENTLY: the scrape is refused
// and the target sits DOWN. Assertion 3 exists because an annotation is a claim
// that something answers on that port; without it a target scrapes a service
// that serves no registry and the result is an empty, healthy-looking series.
// This is the same shape as TestEveryProbePointsAtARouteTheServiceServes, which
// exists because market-ingest shipped a manifest probing /healthz against a
// service serving /livez, and nothing caught it until a real cluster did.
//
// WHAT IT DELIBERATELY DOES NOT ASSERT.
//
// That Prometheus is running, or that any target is UP. Those need a cluster,
// and #98 ("Does a production environment exist at all?") is open. This guard
// proves the CONFIGURATION is complete and internally consistent for all of
// them; it cannot prove a scrape happened. Do not read a green run here as
// "the estate is observed".
//
// THE COUNT IS DERIVED, NEVER HARDCODED. #61 originally demanded "all 26
// services" — unachievable, because five are deliberately not deployed and you
// cannot scrape what never runs. The bar is whatever servicesWithEntrypoints
// minus notDeployed yields today, so adding a service or retiring an exemption
// moves it automatically rather than leaving a stale number in a test.
func TestEveryDeployableServiceIsScrapable(t *testing.T) {
	root := moduleRoot(t)

	var (
		problems   []string
		deployable int
	)
	for _, svc := range servicesWithEntrypoints(t, root) {
		if _, exempt := notDeployed[svc]; exempt {
			continue // already carries a written reason for not running at all
		}
		deployable++

		if reason, pending := pendingScrape[svc]; pending {
			if strings.TrimSpace(reason) == "" {
				problems = append(problems, svc+": listed in pendingScrape with no reason")
			}
			continue
		}

		path := manifestPath(root, svc)
		if path == "" {
			// TestEveryServiceIsDeployableOrExempt owns this failure and reports it
			// with the right message. Reporting it twice would send someone to fix
			// the annotation on a manifest that does not exist.
			continue
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		// NORMALIZE LINE ENDINGS. git on Windows (core.autocrlf=true) hands these
		// over with CRLF, and the annotation regexes below anchor on \s which would
		// otherwise still match — but the containerPort set would not, so the guard
		// would fail for every service on a Windows checkout and pass in CI. A test
		// a contributor cannot run is a test that contributor cannot trust; this is
		// the same trap already documented in TestEveryServiceIsDeployableOrExempt.
		manifest := strings.ReplaceAll(string(body), "\r\n", "\n")

		var missing []string

		if !scrapeAnnotationRe.MatchString(manifest) {
			missing = append(missing, `prometheus.io/scrape: "true"`)
		}

		portHit := scrapePortRe.FindStringSubmatch(manifest)
		switch {
		case portHit == nil:
			missing = append(missing, `prometheus.io/port`)
		default:
			byName := declaredContainerPorts(manifest)
			annotated := portHit[1]
			httpPort, haveHTTP := httpSurfacePort(byName)
			switch {
			case !haveHTTP:
				missing = append(missing, "prometheus.io/port "+annotated+
					" cannot be checked: this manifest declares no port named "+
					strings.Join(httpPortNames, " or ")+
					", so there is no way to tell which port serves HTTP")
			case annotated == byName["grpc"]:
				// The specific mistake this catches, spelled out: venue-binance
				// declares grpc 9000 FIRST and http 8091 second. Annotating the first
				// port listed yields a target Prometheus cannot scrape — gRPC does not
				// answer a GET /metrics — and the failure is a DOWN row in a list, not
				// an error anyone is paged for.
				missing = append(missing, "prometheus.io/port "+annotated+
					" is the gRPC port; /metrics is served on the HTTP port "+httpPort+
					" — a scrape against gRPC cannot succeed")
			case annotated != httpPort:
				missing = append(missing, "prometheus.io/port "+annotated+
					" is not this workload's HTTP port ("+httpPort+
					") — the scrape would be refused and the target would sit DOWN")
			}
		}

		if !registersMetrics(t, root, svc) {
			missing = append(missing, "a /metrics route in Go — the annotation would "+
				"point at a port that serves no Prometheus registry")
		}

		if len(missing) > 0 {
			problems = append(problems, svc+" is missing: "+strings.Join(missing, "; "))
		}
	}

	// NON-VACUITY. A scan that finds no deployable services passes every assertion
	// above no matter how broken the estate is. That is a broken guard reporting a
	// clean estate — the exact class of false green this file exists to prevent.
	if deployable == 0 {
		t.Fatal("found zero deployable services — the scanner is broken, not the estate")
	}

	// DEAD-ENTRY CHECK. An entry naming a service that is now scrapable, exempt, or
	// gone is stale: it suppresses a check that would pass, and makes the estate
	// read as more provisional than it is. Same shape as the dead-entry checks in
	// drPosture and metricSurfacesPendingRepair.
	present := map[string]bool{}
	for _, svc := range servicesWithEntrypoints(t, root) {
		if _, exempt := notDeployed[svc]; !exempt {
			present[svc] = true
		}
	}
	var dead []string
	for svc := range pendingScrape {
		if !present[svc] {
			dead = append(dead, svc)
		}
	}
	sort.Strings(dead)
	if len(dead) > 0 {
		t.Errorf("pendingScrape names %d service(s) that are no longer deployable services: %s\n\n"+
			"An exemption that outlives its repair is worse than no exemption: it reads as a "+
			"known gap being tracked when the thing it tracked is gone.",
			len(dead), strings.Join(dead, ", "))
	}

	sort.Strings(problems)
	if len(problems) > 0 {
		t.Fatalf("these deployable services cannot be scraped:\n\n  %s\n\n"+
			"A deployable service is SCRAPABLE (prometheus.io/scrape + a prometheus.io/port matching a "+
			"declared containerPort + a /metrics route in Go) or it is listed in pendingScrape with a "+
			"written reason and the issue that retires it. Nothing else.\n\n"+
			"Prometheus discovers pods by annotation (infra/observability/prometheus.yaml, role: pod). "+
			"A service with no annotation is not a DOWN target — it is NO target, and absent targets "+
			"appear on no dashboard and fire no alert.",
			strings.Join(problems, "\n  "))
	}
}

// registersMetrics reports whether the service actually serves a Prometheus
// registry.
//
// THIS DELIBERATELY DOES NOT USE servesPath, AND THE REASON IS A BUG THIS GUARD
// SHIPPED WITH FOR ONE ITERATION. servesPath falls back to scanning
// internal/venueadapter for EVERY service, which is right for probe paths — the
// venue adapters register /healthz and /readyz in that shared package. But
// probes.go:96 also registers "/metrics", so the fallback matched for all 21
// services and assertion 3 passed unconditionally. operator and schema-registry,
// which serve no metrics at all, were reported as fully scrapable.
//
// A guard that cannot fail is worse than no guard: it converts an open question
// into a green check. So the shared-package fallback is gated on the service
// actually importing it, rather than applied to everyone.
func registersMetrics(t *testing.T, root, svc string) bool {
	t.Helper()

	svcDir := filepath.Join(root, "services", svc)
	if treeContainsAny(svcDir, `"GET /metrics"`, `"/metrics"`) {
		return true
	}
	// The venue adapters mount their registry on the shared adapter server rather
	// than in their own tree. Only credit that package to a service that imports it.
	if treeContainsAny(svcDir, `internal/venueadapter/server`) {
		return treeContainsAny(filepath.Join(root, "internal", "venueadapter"),
			`"GET /metrics"`, `"/metrics"`)
	}
	return false
}

// treeContainsAny reports whether any non-test .go file under dir contains any of
// the given literals.
func treeContainsAny(dir string, literals ...string) bool {
	found := false
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		for _, lit := range literals {
			if strings.Contains(string(b), lit) {
				found = true
			}
		}
		return nil
	})
	return found
}

// pendingScrape is a DEFAULT-DENY allow-list: a deployable service may appear
// here only with the reason it cannot be scraped yet AND the issue that removes
// it. It exists so this guard can land BEFORE the repairs it demands, rather
// than the repairs landing with no guard to hold them — the same reason
// metricSurfacesPendingRepair exists.
//
// IT MUST TREND TO EMPTY. An entry here is a service whose degraded modes are
// invisible in production; it is not a place to park work.
var pendingScrape = map[string]string{}

// scrapeAnnotationRe matches the opt-in annotation on a pod template. Quoted and
// unquoted "true" both appear in Kubernetes manifests in the wild; node-exporter
// uses the quoted form, which is the convention this estate follows.
var scrapeAnnotationRe = regexp.MustCompile(`prometheus\.io/scrape:\s*"?true"?`)

// scrapePortRe captures the annotated scrape port. It is a STRING in YAML —
// Kubernetes annotation values are always strings — so the quotes are expected.
var scrapePortRe = regexp.MustCompile(`prometheus\.io/port:\s*"?(\d+)"?`)

// containerPortRe finds every named port the workload opens, in BOTH key orders.
//
// The estate writes these two ways and a regex anchored on one order silently
// misses the other: `{ containerPort: 9000, name: grpc }` (venue-binance) and
// `{ name: grpc, containerPort: 9090 }` (operator). An earlier draft of this
// guard matched only the first form, concluded operator's ports were unnamed,
// and would have accepted the gRPC port as the scrape target.
var containerPortRe = regexp.MustCompile(
	`containerPort:\s*(\d+),\s*name:\s*([a-z][a-z0-9-]*)|name:\s*([a-z][a-z0-9-]*),\s*containerPort:\s*(\d+)`)

// httpPortNames are the names this estate gives the port that serves HTTP — and
// therefore /metrics. Every service uses "http" except operator, whose HTTP
// surface is the /healthz + /readyz server it names "health".
var httpPortNames = []string{"http", "health"}

// declaredContainerPorts maps port NAME to port number.
//
// Keyed by name, not a bare set, because "does the annotated port appear
// somewhere in this manifest" is too weak an assertion to be worth making: it
// accepts the gRPC port, which is exactly the mistake that produces a target
// that can never be scraped.
func declaredContainerPorts(manifest string) map[string]string {
	out := map[string]string{}
	for _, hit := range containerPortRe.FindAllStringSubmatch(manifest, -1) {
		switch {
		case hit[1] != "" && hit[2] != "":
			out[hit[2]] = hit[1]
		case hit[3] != "" && hit[4] != "":
			out[hit[3]] = hit[4]
		}
	}
	return out
}

// httpSurfacePort returns the port serving HTTP, and whether one was found.
func httpSurfacePort(byName map[string]string) (string, bool) {
	for _, n := range httpPortNames {
		if p, ok := byName[n]; ok {
			return p, true
		}
	}
	return "", false
}
