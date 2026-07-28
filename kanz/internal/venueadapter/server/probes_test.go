package server

// A venue adapter has two outputs. gRPC works as long as the process is
// listening; the fill FACTs it publishes are the half that can fail silently —
// and they are the half that carries the record of money moving.
//
// These tests hold the line that an adapter which cannot report its fills must
// stop accepting orders.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eighred/kanz/pkg/bus"
)

type failingPub struct{ err error }

func (f *failingPub) Publish(context.Context, bus.Event) error { return f.err }

func probe(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code, rec.Body.String()
}

func TestReadyBeforeAnyPublishAttempt(t *testing.T) {
	// A freshly started adapter has published nothing. That is not a failure — it
	// must be allowed to take its first order.
	r := &Readiness{}
	r.Set(true)
	r.TrackPublisher(bus.NewHealthPublisher(&failingPub{}, 3))

	if code, _ := probe(t, Probes(r, nil), "/readyz"); code != http.StatusOK {
		t.Fatalf("/readyz = %d before any publish, want 200 — the adapter would never accept an order", code)
	}
}

func TestAdapterGoesUnreadyWhenItCannotReportFills(t *testing.T) {
	// The dangerous state: gRPC is fine, the exchange is fine, and every fill FACT
	// is being dropped. The adapter would execute orders at a live venue and lose
	// the results — the fund's money moves and the OMS never hears.
	//
	// Unready ⇒ out of its Service ⇒ the OMS's dial fails ⇒ the router hard-errors
	// on this MIC. Refusing to trade beats trading blind.
	health := bus.NewHealthPublisher(&failingPub{err: errors.New("nats: no response from stream")}, 3)
	r := &Readiness{}
	r.Set(true)
	r.TrackPublisher(health)

	for i := 0; i < 3; i++ {
		_ = health.Publish(context.Background(), bus.Event{})
	}

	code, body := probe(t, Probes(r, nil), "/readyz")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz = %d while every fill publish fails, want 503 — this adapter would keep taking orders and losing their fills", code)
	}
	if !strings.Contains(body, "publish failures") || !strings.Contains(body, "LOST") {
		t.Fatalf("/readyz must state the consequence plainly; got %q", body)
	}

	// Liveness stays OK: a restart cannot fix a missing stream, and a crash-loop
	// would only bury the reason.
	if code, _ := probe(t, Probes(r, nil), "/healthz"); code != http.StatusOK {
		t.Fatalf("/healthz = %d, want 200", code)
	}
}

func TestAdapterRecoversWithoutARestart(t *testing.T) {
	inner := &failingPub{err: errors.New("nats: connection lost")}
	health := bus.NewHealthPublisher(inner, 2)
	r := &Readiness{}
	r.Set(true)
	r.TrackPublisher(health)

	for i := 0; i < 2; i++ {
		_ = health.Publish(context.Background(), bus.Event{})
	}
	if code, _ := probe(t, Probes(r, nil), "/readyz"); code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz = %d with the bus down, want 503", code)
	}

	inner.err = nil // the bus came back
	_ = health.Publish(context.Background(), bus.Event{})

	if code, _ := probe(t, Probes(r, nil), "/readyz"); code != http.StatusOK {
		t.Fatalf("/readyz = %d after publishing recovered, want 200 — the venue would stay offline until someone restarted it", code)
	}
}

func TestStartingUpIsUnreadyEvenWithHealthyPublishing(t *testing.T) {
	// Credentials, order view, connector — none of it up yet. Reporting ready here
	// would have the OMS route live orders into a process that cannot work them.
	r := &Readiness{} // Set(true) never called
	r.TrackPublisher(bus.NewHealthPublisher(&failingPub{}, 3))

	code, body := probe(t, Probes(r, nil), "/readyz")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz = %d during startup, want 503", code)
	}
	if !strings.Contains(body, "starting up") {
		t.Fatalf("body = %q, want it to say it is still starting", body)
	}
}
