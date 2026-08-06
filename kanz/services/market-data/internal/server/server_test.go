package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/market-data/internal/server"
)

// deadPublisher fails every publish, standing in for the broker refusing the
// feed's normalized events.
type deadPublisher struct{ err error }

// Publish returns d.err, which the recovery test clears in place to stand for a
// broker coming back.
func (d *deadPublisher) Publish(context.Context, bus.Event) error { return d.err }

func probe(t *testing.T, r *server.Readiness, path string) (int, string) {
	t.Helper()
	srv := server.New(r, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code, rec.Body.String()
}

func failNTimes(t *testing.T, h *bus.HealthPublisher, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		_ = h.Publish(context.Background(), bus.Event{})
	}
}

// Before startup finishes, /readyz must refuse — unchanged by #299, asserted
// because the publisher work below must not have quietly turned a not-ready
// service into a ready one.
func TestReadyzRefusesBeforeStartupCompletes(t *testing.T) {
	code, body := probe(t, &server.Readiness{}, "/readyz")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz = %d before startup, want 503", code)
	}
	if !strings.Contains(body, "starting up") {
		t.Errorf("the refusal must say WHY; got %s", body)
	}
}

// The feed attaches its tracker only after it has dialed a broker and built a
// producer. Until then the absence of a tracker must read as READY, or the
// service would report a publish failure that has not happened.
func TestReadyzIsReadyBeforeTheFeedAttachesItsTracker(t *testing.T) {
	r := &server.Readiness{}
	r.Set(true)

	code, _ := probe(t, r, "/readyz")
	if code != http.StatusOK {
		t.Fatalf("/readyz = %d with no tracker attached yet, want 200 — a feed that has not reached its "+
			"producer yet has not failed to publish", code)
	}
}

// THE POINT OF #299. A feed publisher that cannot reach the spine takes this
// service out of Service, the way venue-binance and venue-okx already behave for
// the same class of data.
func TestReadyzRefusesAfterConsecutiveFeedPublishFailures(t *testing.T) {
	r := &server.Readiness{}
	r.Set(true)
	h := bus.NewHealthPublisher(&deadPublisher{err: errors.New("no response from stream")},
		bus.DefaultPublishFailureThreshold)
	r.TrackPublisher(h)

	// One below the threshold is NOT yet a failure: a single blip must not pull a
	// service out of rotation.
	failNTimes(t, h, bus.DefaultPublishFailureThreshold-1)
	if code, _ := probe(t, r, "/readyz"); code != http.StatusOK {
		t.Fatalf("/readyz = %d after %d failures (threshold %d), want 200 — one blip below the "+
			"threshold must not un-ready the service",
			code, bus.DefaultPublishFailureThreshold-1, bus.DefaultPublishFailureThreshold)
	}

	failNTimes(t, h, 1)
	code, body := probe(t, r, "/readyz")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz = %d after %d consecutive feed publish failures, want 503. The publisher is "+
			"dead and the pod is still advertising itself as healthy — which is exactly the silence "+
			"#299 exists to end", code, bus.DefaultPublishFailureThreshold)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, body)
	}
	if !strings.Contains(got["reason"], "feed publish failures") {
		t.Errorf("the refusal must name the feed publisher as the cause, or an operator reads it as a "+
			"store or subscription problem; got %q", got["reason"])
	}
	if !strings.Contains(got["reason"], "no response from stream") {
		t.Errorf("the refusal must carry the last error; got %q", got["reason"])
	}
}

// Recovery is not manual. HealthPublisher resets its consecutive count on a
// success, so a broker that comes back brings the service back with it.
func TestReadyzRecoversWhenPublishingRecovers(t *testing.T) {
	r := &server.Readiness{}
	r.Set(true)
	dead := &deadPublisher{err: errors.New("broker down")}
	h := bus.NewHealthPublisher(dead, bus.DefaultPublishFailureThreshold)
	r.TrackPublisher(h)

	failNTimes(t, h, bus.DefaultPublishFailureThreshold)
	if code, _ := probe(t, r, "/readyz"); code != http.StatusServiceUnavailable {
		t.Fatalf("setup: expected 503 before recovery, got %d", code)
	}

	dead.err = nil // the broker comes back
	if err := h.Publish(context.Background(), bus.Event{}); err != nil {
		t.Fatalf("publish after recovery: %v", err)
	}
	if code, _ := probe(t, r, "/readyz"); code != http.StatusOK {
		t.Fatalf("/readyz = %d after publishing recovered, want 200 — a service that stays out of "+
			"rotation after the cause cleared needs a human for nothing", code)
	}
}

// THE DOCUMENTED LIMIT, PINNED. /healthz is a SEPARATE endpoint from /readyz in
// market-data-deploy.yaml, so a dead feed publisher must NOT restart the pod:
// ingestion keeps running and the store keeps being written. If this ever starts
// failing, un-readying became a crash-loop, which is a different and much larger
// decision than the one #299 took.
func TestHealthzStaysOKWhileTheFeedPublisherIsDead(t *testing.T) {
	r := &server.Readiness{}
	r.Set(true)
	h := bus.NewHealthPublisher(&deadPublisher{err: errors.New("broker down")},
		bus.DefaultPublishFailureThreshold)
	r.TrackPublisher(h)
	failNTimes(t, h, bus.DefaultPublishFailureThreshold*2)

	if code, _ := probe(t, r, "/readyz"); code != http.StatusServiceUnavailable {
		t.Fatalf("setup: expected /readyz 503, got %d", code)
	}
	if code, _ := probe(t, r, "/healthz"); code != http.StatusOK {
		t.Fatalf("/healthz = %d with a dead feed publisher, want 200. Liveness is a separate probe: "+
			"un-readying signals the failure, it must not restart a pod whose ingest half is still "+
			"consuming and writing", code)
	}
}
