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
// For every workload that is DEPLOYABLE — the filesystem's list, minus the
// written exemptions in notDeployed — three things must hold together:
//
//  1. its workload manifest carries prometheus.io/scrape: "true"
//  2. its prometheus.io/port matches a containerPort declared in that same
//     manifest
//  3. the workload actually registers /metrics in its own source, whatever
//     language that is (see scrapeTargets: Prometheus scrapes pods, and one of
//     this estate's pods is Python)
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
// cannot scrape what never runs. The bar is whatever scrapeTargets yields today,
// so adding a service or retiring an exemption moves it automatically rather
// than leaving a stale number in a test.
func TestEveryDeployableServiceIsScrapable(t *testing.T) {
	root := moduleRoot(t)

	targets := scrapeTargets(t, root)

	var problems []string
	for _, svc := range targets {
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
					" is not the port this workload serves /metrics on ("+httpPort+
					") — the scrape would be refused and the target would sit DOWN")
			}
		}

		if !registersMetrics(t, root, svc) {
			missing = append(missing, "a /metrics route in its source — the annotation "+
				"would point at a port that serves no Prometheus registry")
		}

		if len(missing) > 0 {
			problems = append(problems, svc+" is missing: "+strings.Join(missing, "; "))
		}
	}

	// NON-VACUITY. A scan that finds no deployable services passes every assertion
	// above no matter how broken the estate is. That is a broken guard reporting a
	// clean estate — the exact class of false green this file exists to prevent.
	if len(targets) == 0 {
		t.Fatal("found zero deployable services — the scanner is broken, not the estate")
	}

	// DEAD-ENTRY CHECK. An entry naming a service that is now scrapable, exempt, or
	// gone is stale: it suppresses a check that would pass, and makes the estate
	// read as more provisional than it is. Same shape as the dead-entry checks in
	// drPosture and metricSurfacesPendingRepair.
	//
	// It reads the SAME scrapeTargets list the loop above walked. An earlier draft
	// rebuilt the set from servicesWithEntrypoints here, which meant a non-Go
	// workload could be exempted above and then reported dead below — the guard
	// would have been unable to hold any exemption it was widened to need.
	present := map[string]bool{}
	for _, svc := range targets {
		present[svc] = true
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
			"declared containerPort + a /metrics route in its source) or it is listed in pendingScrape "+
			"with a written reason and the issue that retires it. Nothing else.\n\n"+
			"Prometheus discovers pods by annotation (infra/observability/prometheus.yaml, role: pod). "+
			"A service with no annotation is not a DOWN target — it is NO target, and absent targets "+
			"appear on no dashboard and fire no alert.",
			strings.Join(problems, "\n  "))
	}
}

// scrapeTargets lists every workload the estate actually runs: the Go services
// with a cmd/ entrypoint that are not exempt in notDeployed, PLUS every workload
// that has a manifest under infra/deploy but no Go package behind it.
//
// WHY THIS IS NOT servicesWithEntrypoints. That helper answers "which
// services/<name> build a Go binary". That is the right question for the
// deployability and NATS guards, and the WRONG one here: what Prometheus scrapes
// is a POD, and a pod does not have to be Go. The estate deploys one that is not
// — the Python inference service, which lives in kanz-py/ and is deployed by
// infra/deploy/inference-deploy.yaml. Enumerating from the Go tree made it
// structurally invisible to this guard, so a workload with no scrape annotation
// and no HTTP surface at all read as a clean estate rather than an unmonitored
// one, and the guard could not even have listed it as pending.
//
// This is the same cross-language blindness kafka_topology_test.go documents
// from the other side: a check that walks only Go pronounces safe what it cannot
// see. Enumerate from the thing that decides whether a pod runs — the manifest.
//
// Derived from the filesystem in both halves, never listed: a second Python (or
// Rust, or anything) workload is covered the day its manifest lands, rather than
// the day somebody remembers to add it here.
func scrapeTargets(t *testing.T, root string) []string {
	t.Helper()

	seen := map[string]bool{}
	var out []string
	for _, svc := range servicesWithEntrypoints(t, root) {
		if _, exempt := notDeployed[svc]; exempt {
			continue // already carries a written reason for not running at all
		}
		seen[svc] = true
		out = append(out, svc)
	}

	manifests, err := filepath.Glob(filepath.Join(root, "infra", "deploy", "*.yaml"))
	if err != nil {
		t.Fatalf("glob infra/deploy: %v", err)
	}
	// NON-VACUITY, the manifest half. If this directory moves or is renamed, the
	// glob returns nothing and every non-Go workload silently stops being checked
	// — the widening would undo itself and look green doing it.
	if len(manifests) == 0 {
		t.Fatal("found zero manifests under infra/deploy — the scanner is broken, not the estate")
	}
	for _, m := range manifests {
		base := filepath.Base(m)
		trimmed := strings.TrimSuffix(base, "-deploy.yaml")
		if trimmed == base {
			trimmed = strings.TrimSuffix(base, "-rollout.yaml")
		}
		if trimmed == base {
			continue // not a workload manifest (ingress, scaling policy, dev secrets, ...)
		}
		if seen[trimmed] {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, "services", trimmed)); err == nil {
			continue // a Go service; servicesWithEntrypoints and notDeployed already ruled on it
		}
		seen[trimmed] = true
		out = append(out, trimmed)
	}

	sort.Strings(out)
	return out
}

// registersMetrics reports whether the service actually serves a Prometheus
// registry.
//
// THIS DELIBERATELY DOES NOT USE servesPath, AND THE REASON IS A BUG THIS GUARD
// SHIPPED WITH FOR ONE ITERATION. servesPath falls back to scanning
// internal/venueadapter for EVERY service, which is right for probe paths — the
// venue adapters register /healthz and /readyz in that shared package. But
// probes.go also registers "/metrics", so the fallback matched for all 21
// services and assertion 3 passed unconditionally. operator and schema-registry,
// which serve no metrics at all, were reported as fully scrapable.
//
// A guard that cannot fail is worse than no guard: it converts an open question
// into a green check. So the shared-package fallback is gated on the service
// actually importing it, rather than applied to everyone.
func registersMetrics(t *testing.T, root, svc string) bool {
	t.Helper()

	svcDir := filepath.Join(root, "services", svc)
	if _, err := os.Stat(svcDir); err != nil {
		return registersMetricsOutsideGo(t, root, svc)
	}
	if treeContainsAny(svcDir, ".go", `"GET /metrics"`, `"/metrics"`) {
		return true
	}
	// The venue adapters mount their registry on the shared adapter server rather
	// than in their own tree. Only credit that package to a service that imports it.
	if treeContainsAny(svcDir, ".go", `internal/venueadapter/server`) {
		return treeContainsAny(filepath.Join(root, "internal", "venueadapter"), ".go",
			`"GET /metrics"`, `"/metrics"`)
	}
	return false
}

// registersMetricsOutsideGo answers assertion 3 for a deployed workload that has
// no Go package — today, the Python inference service in kanz-py/.
//
// IT REFUSES TO GUESS. A workload whose source this cannot locate makes the
// guard fail, loudly, rather than return false (which would read as "checked,
// and it serves no metrics") or true (which would be a fabricated pass). The
// distinction matters here more than usual: this whole function exists because a
// service the guard could not see was indistinguishable from a service the guard
// had cleared.
func registersMetricsOutsideGo(t *testing.T, root, svc string) bool {
	t.Helper()

	pkg := filepath.Join(filepath.Dir(root), "kanz-py", "kanz_"+strings.ReplaceAll(svc, "-", "_"))
	if _, err := os.Stat(pkg); err != nil {
		t.Fatalf("%s is deployed by infra/deploy/ but has neither a Go package under services/ "+
			"nor a Python package at %s. This guard cannot tell whether it serves /metrics, and "+
			"a guess in either direction is a false result — teach registersMetricsOutsideGo "+
			"where this workload's source lives.", svc, pkg)
	}
	// prometheus_client is the only Prometheus surface this estate would use from
	// Python: make_wsgi_app/start_http_server are the two ways it exposes a
	// registry over HTTP, and a hand-rolled handler still has to name the route.
	return treeContainsAny(pkg, ".py", `"/metrics"`, `'/metrics'`,
		"make_wsgi_app", "start_http_server")
}

// treeContainsAny reports whether any non-test source file with the given
// extension under dir contains any of the given literals.
func treeContainsAny(dir, ext string, literals ...string) bool {
	found := false
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ext) {
			return nil
		}
		base := filepath.Base(p)
		if strings.HasSuffix(base, "_test"+ext) || strings.HasPrefix(base, "test_") {
			return nil // a test asserting on "/metrics" is not a service serving it
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
// IT IS EMPTY, AND THE ENTRY THAT WAS HERE CAME OUT THE WAY IT WAS MEANT TO. It
// held `inference` — the one deployed pod with no HTTP surface at all, whose
// manifest opened exactly one port (grpc 50051) probed by two tcpSocket checks,
// so no annotation could name a scrapable port. #241 built the surface
// (kanz-py/kanz_inference/observability/) and opened { containerPort: 8093, name:
// http }; the moment the manifest carried the annotation, the dead-entry check
// below failed the build naming this service, and the entry was removed in the
// same change. That is the intended lifecycle — an exemption is a debt with an
// issue number, not a carve-out.
//
// Leave the map rather than deleting it. An empty default-deny list is a working
// guard with nothing exempted; removing it would leave the next person needing a
// temporary exemption to reinvent the mechanism, and the likelier reach is for
// weakening the check instead. Same reasoning as metricSurfacesPendingRepair.
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

// httpSurfacePort returns the port that serves /metrics, and whether one was
// found.
//
// A PORT NAMED "metrics" WINS OVER "http", and that precedence is the whole
// point (#447). This guard used to assume /metrics rides the HTTP port, which is
// true for every service that serves both from one mux — and false for exactly
// the services that must not.
//
// #232 named the residual it could not close with a manifest edit: a
// tenant-scoped API sharing a port with /metrics is reachable from
// kanz-observability with a self-chosen principal, because
// allow-observability-scrape has to admit whatever port serves the scrape. The
// fix is a SECOND LISTENER, and a service that makes it declares two ports.
//
// Without this branch the guard actively FIGHTS that fix: it would report a
// correctly-split service as unscrapable and the obvious way to satisfy it would
// be to put the API back on the scraped port. A guard that pushes a capital
// surface back into a known exposure is worse than no guard.
//
// It went unnoticed because optimization — the first service to split — is in
// notDeployed, so its manifest was never checked. accounting is the first
// DEPLOYED service to split, and it found this immediately.
func httpSurfacePort(byName map[string]string) (string, bool) {
	if p, ok := byName[metricsPortName]; ok {
		return p, true
	}
	for _, n := range httpPortNames {
		if p, ok := byName[n]; ok {
			return p, true
		}
	}
	return "", false
}

// metricsPortName is the name a workload gives a listener that serves ONLY
// /metrics — the second listener a service grows when its API must leave the
// scraped port (#232, #447).
const metricsPortName = "metrics"
