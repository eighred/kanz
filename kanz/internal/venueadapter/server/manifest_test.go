package server_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/kanz-eng/kanz/internal/venueadapter/server"
	"github.com/kanz-eng/kanz/pkg/bus"
)

// The publish-health control only works if the kubelet actually HITS the endpoint
// that reports it. That is a property of the deploy manifest, not of the code, and
// the code cannot enforce it — so this test does.
//
// It is not hypothetical. market-ingest shipped a manifest whose probe pointed at
// /healthz while the service served /livez: the probe 404'd, and the whole
// readiness signal was decorative. Nothing caught it, because the manifest and the
// mux were only ever read by different people.
//
// This test does NOT compare strings. It reads the path the manifest tells the
// kubelet to poll, then drives the REAL Probes() handler with THAT path while
// publishing is broken, and requires it to answer 503. A manifest pointing at
// /healthz fails here, because /healthz stays 200 when the bus is gone — which is
// exactly what liveness is for, and exactly why it must not be the readiness probe.
func TestDeployManifestsProbeThePathThatActuallyReportsPublishHealth(t *testing.T) {
	for _, manifest := range []string{
		"../../../infra/deploy/venue-binance-deploy.yaml",
		"../../../infra/deploy/venue-okx-deploy.yaml",
	} {
		t.Run(strings.TrimSuffix(manifest[strings.LastIndex(manifest, "/")+1:], ".yaml"), func(t *testing.T) {
			path, port := readinessProbe(t, manifest)

			// The probe must poll the port THIS manifest tells THIS adapter to serve
			// its probes on (VENUE_*_LISTEN). A probe aimed at a port nothing listens
			// on — the gRPC port, say — is a probe that can never fail, and a probe
			// that can never fail is a pod that is never evicted.
			if listen := probeListenPort(t, manifest); port != listen {
				t.Errorf("the kubelet polls port %s, but this adapter serves its probes on %s", port, listen)
			}

			// A live adapter whose publishes are ALL failing: it can still execute
			// orders at the exchange, and it can no longer report what it did.
			readiness := &server.Readiness{}
			readiness.Set(true)
			health := bus.NewHealthPublisher(brokenBus{}, 1)
			readiness.TrackPublisher(health)
			_ = health.Publish(t.Context(), fillEvent())

			rec := httptest.NewRecorder()
			server.Probes(readiness, nil).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("the kubelet is told to poll %q, and that path answers %d while every fill this adapter produces is being lost.\n"+
					"The pod stays in its Service, the OMS keeps routing orders into it, and the money moves unrecorded. "+
					"The readiness probe must point at the path that reports publish health.", path, rec.Code)
			}
		})
	}
}

// brokenBus is a bus that is simply not there.
type brokenBus struct{}

func (brokenBus) Publish(context.Context, bus.Event) error { return errNoBus }

var errNoBus = errors.New("no responders for stream")

// readinessProbe extracts the path and port the manifest tells the kubelet to poll.
// A manifest this cannot parse FAILS the test: an unreadable probe is an unproven
// probe, and silently skipping it is how the market-ingest manifest survived.
func readinessProbe(t *testing.T, path string) (probePath, probePort string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	// readinessProbe:
	//   httpGet: { path: /readyz, port: 8091 }
	re := regexp.MustCompile(`readinessProbe:\s*\n\s*httpGet:\s*\{\s*path:\s*(\S+?),\s*port:\s*(\d+)\s*\}`)
	m := re.FindStringSubmatch(string(b))
	if m == nil {
		t.Fatalf("%s: could not find a readinessProbe httpGet — if the format changed, fix this test rather than deleting it: "+
			"it is the only thing standing between a mis-pointed probe and an adapter that trades while nobody is listening", path)
	}
	return m[1], m[2]
}

// probeListenPort reads the port this manifest configures the adapter's probe
// server to listen on (VENUE_<VENUE>_LISTEN: ":8091").
func probeListenPort(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	// [A-Z]+ excludes the underscore, so this cannot match VENUE_*_GRPC_LISTEN —
	// which matters: matching the gRPC port here would "prove" that the kubelet
	// polls the port orders are submitted on.
	re := regexp.MustCompile(`name:\s*VENUE_[A-Z]+_LISTEN,\s*value:\s*"?:(\d+)"?`)
	m := re.FindStringSubmatch(string(b))
	if m == nil {
		t.Fatalf("%s: no VENUE_*_LISTEN — cannot tell which port this adapter serves its probes on", path)
	}
	return m[1]
}
