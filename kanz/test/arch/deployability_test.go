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

// EVERY SERVICE IS DEPLOYABLE, OR IT SAYS WHY NOT — IN WRITING.
//
// We reported "15 images referenced by a manifest, 15 images built, no gaps" and
// called deployability complete. That was TRUE AND MEANINGLESS: it is a closed loop
// over the manifests that HAPPEN TO EXIST, and it is silent about the services that
// must RUN. webhook-ingest — the HMAC webhook that receives every TradingView signal,
// the FIRST HOP of the delivered trading loop — had no manifest and no image, and the
// parity check was green the whole time (EXEC-M10, found by deploying to a real
// cluster).
//
// A missing file cannot fail a check that only looks at the files present. So this
// test starts from the SERVICES, not the manifests: every service with a cmd/
// entrypoint must have
//
//	a Dockerfile · an entry in the CI build matrix · a deploy manifest
//
// or an explicit, reasoned exemption below. "We forgot" is not a reason anyone can
// write down, which is the point.
func TestEveryServiceIsDeployableOrExempt(t *testing.T) {
	root := moduleRoot(t)

	build, err := os.ReadFile(filepath.Join(filepath.Dir(root), ".github", "workflows", "build.yml"))
	if err != nil {
		t.Fatalf("read the image build matrix: %v", err)
	}
	// NORMALIZE THE LINE ENDINGS before matching. The check below is an exact match
	// ending in "\n", and git on Windows (core.autocrlf=true) hands this file over with
	// CRLF — so every service looked un-built, and this guard failed for ALL of them on
	// any Windows checkout. It could never have been run by a Windows developer; it
	// passed only because CI is Linux. A test that cannot run on a contributor's machine
	// is a test that contributor cannot trust.
	matrix := strings.ReplaceAll(string(build), "\r\n", "\n")

	var problems []string
	for _, svc := range servicesWithEntrypoints(t, root) {
		if reason, ok := notDeployed[svc]; ok {
			if strings.TrimSpace(reason) == "" {
				problems = append(problems, svc+": exempted with no reason")
			}
			continue
		}
		var missing []string
		if _, err := os.Stat(filepath.Join(root, "services", svc, "Dockerfile")); err != nil {
			missing = append(missing, "Dockerfile")
		}
		if !strings.Contains(matrix, "- service: "+svc+"\n") {
			missing = append(missing, "an entry in .github/workflows/build.yml")
		}
		if !manifestExists(root, svc) {
			missing = append(missing, "a deploy manifest (infra/deploy/"+svc+"-deploy.yaml)")
		}
		if len(missing) > 0 {
			problems = append(problems, svc+" is missing: "+strings.Join(missing, ", "))
		}
	}
	sort.Strings(problems)
	if len(problems) > 0 {
		t.Fatalf("these services cannot run in production:\n\n  %s\n\n"+
			"A service is DEPLOYABLE (Dockerfile + build-matrix entry + manifest) or it is EXEMPT with a written reason "+
			"in notDeployed. Nothing else. An image/manifest parity check cannot see a service that has neither.",
			strings.Join(problems, "\n  "))
	}
}

// notDeployed is the list of services that deliberately do not run in production,
// each with the reason. A service is here because somebody DECIDED it, not because
// somebody forgot it.
var notDeployed = map[string]string{
	"web-bff": "PARKED. The client surface is the CLI (CLI-01/02, delivered); the web workspace " +
		"vertical (PS-02b) sits behind the active execution direction. The BFF is built and tested but " +
		"deliberately not deployed — deploying an unused public edge is attack surface for nothing.",

	"optimization": "NOT YET, AND DELIBERATELY. It materializes an approved rebalance proposal into OMS " +
		"order COMMANDS — it is a capital path, not an analytic. It does not run until the execution loop " +
		"has run in production and there is a human approval surface in front of it. Deploying a service " +
		"that can emit orders, before anyone has watched the order path work, is the wrong order.",

	"autopilot": "NOT YET, AND DELIBERATELY. It is the closed-loop ops CONTROLLER — it consumes quality/ " +
		"drift/SLO signals and ACTS on the platform. An autonomous remediator must not be turned on before " +
		"the system it remediates has ever run in production and been watched by a human.",

	"lake-sink": "BLOCKED ON KAFKA. It streams the durable Kafka log (the system of record) into the " +
		"lakehouse. Kafka is not provisioned and its integration tests skip for the same reason " +
		"(TEST_KAFKA_BROKERS is unset in CI). Deploying a sink with no source would be a pod that reports " +
		"healthy and moves nothing.",

	"performance": "ANALYTICS PLANE, PARKED. Stateless return/attribution analytics with no consumer on " +
		"the active execution direction. It ships behind the analytics surface, not with the trading loop.",

	"lineage": "ANALYTICS/GOVERNANCE PLANE, PARKED. The OpenLineage harvester and provenance catalog. Same " +
		"reason as performance: no consumer on the active direction.",
}

// servicesWithEntrypoints lists every services/<name> that builds a binary.
func servicesWithEntrypoints(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "services"))
	if err != nil {
		t.Fatalf("read services/: %v", err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, "services", e.Name(), "cmd")); err != nil {
			continue // a library-only service directory builds no image
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

// manifestExists reports whether the service has a workload manifest. risk-engine
// ships an Argo Rollout rather than a Deployment; both are workloads.
func manifestExists(root, svc string) bool {
	for _, name := range []string{svc + "-deploy.yaml", svc + "-rollout.yaml"} {
		if _, err := os.Stat(filepath.Join(root, "infra", "deploy", name)); err == nil {
			return true
		}
	}
	return false
}

// A PROBE MUST POINT AT A PATH THE SERVICE ACTUALLY SERVES.
//
// market-ingest shipped a manifest that probed /healthz while the service served
// /livez. The probe 404'd, so the readiness signal — the thing that pulls a broken
// pod out of its Service — was decorative. Nobody caught it, because the manifest
// and the mux were only ever read by different people.
//
// The compiler cannot see a YAML file, so this test does: for every workload
// manifest, take the path each probe is aimed at and require that the service
// actually registers it.
//
// Scope, honestly: it looks in the service's own tree, and then in internal/ (the
// venue adapters register their probes in the shared internal/venueadapter/server).
// A path registered nowhere fails. This is a string check, not a behavioural one —
// the venue adapters additionally DRIVE their real probe handler with the manifest's
// path (internal/venueadapter/server/manifest_test.go), which is stronger, and is
// the shape to copy if a probe here ever gets subtle.
func TestEveryProbePointsAtARouteTheServiceServes(t *testing.T) {
	root := moduleRoot(t)

	probeRe := regexp.MustCompile(`(livenessProbe|readinessProbe|startupProbe):\s*\n\s*httpGet:\s*\{\s*path:\s*(\S+?),`)

	manifests, err := filepath.Glob(filepath.Join(root, "infra", "deploy", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var problems []string
	for _, m := range manifests {
		base := filepath.Base(m)
		svc := strings.TrimSuffix(strings.TrimSuffix(base, "-deploy.yaml"), "-rollout.yaml")
		if _, err := os.Stat(filepath.Join(root, "services", svc)); err != nil {
			continue // not a service workload (scaling policy, analysis template, …)
		}
		body, err := os.ReadFile(m)
		if err != nil {
			t.Fatal(err)
		}
		for _, hit := range probeRe.FindAllStringSubmatch(string(body), -1) {
			kind, path := hit[1], hit[2]
			if servesPath(t, root, svc, path) {
				continue
			}
			problems = append(problems, base+": the "+kind+" is aimed at "+path+
				", which "+svc+" does not register")
		}
	}
	sort.Strings(problems)
	if len(problems) > 0 {
		t.Fatalf("these probes point at paths that do not exist:\n\n  %s\n\n"+
			"A probe on a path the service does not serve 404s, so the signal it carries — readiness, which is what "+
			"pulls a broken pod out of its Service — is decorative. This is how market-ingest shipped a manifest "+
			"probing /healthz against a service serving /livez.", strings.Join(problems, "\n  "))
	}
}

// servesPath reports whether the service registers the HTTP path.
func servesPath(t *testing.T, root, svc, path string) bool {
	t.Helper()
	for _, dir := range []string{
		filepath.Join(root, "services", svc),
		filepath.Join(root, "internal", "venueadapter"), // the shared adapter probe surface
	} {
		found := false
		_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				return nil
			}
			if strings.Contains(string(b), `"`+path+`"`) || strings.Contains(string(b), `"GET `+path+`"`) {
				found = true
			}
			return nil
		})
		if found {
			return true
		}
	}
	return false
}
