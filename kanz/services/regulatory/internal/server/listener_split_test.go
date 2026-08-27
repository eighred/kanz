package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/eighred/kanz/internal/audit/signer"
)

// THE FILING ROUTES AND /metrics MUST NOT SHARE A LISTENER (#765).
//
// allow-observability-scrape selects EVERY pod in kanz-services and admits a
// list of ports, 8083 among them. This service served both its /metrics and its
// five POST /v1/filings/* routes there — and a filing is not a read: it is
// signed and, on the default chain signer, appends a link to the AUDIT-01 hash
// chain through linkstore.AppendChained.
//
// So any pod in the kanz-observability namespace could write to the compliance
// record: a filing nobody filed, signed by the platform, occupying a real
// position in the chain. This service reads no principal header, so there was no
// authentication step for it to fail.
//
// audit made this same move from this same port in #627; accounting in #447;
// optimization in #409. This is the last of the four, and the only one whose
// routes append to the chain.

// stubMetrics is a recognisable handler: if it is ever reachable on the API mux
// the test can say so rather than infer it from a status code.
type stubMetrics struct{ hits int }

func (m *stubMetrics) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	m.hits++
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("kanz_stub_metric 1\n"))
}

func splitServer(t *testing.T) (*Server, *stubMetrics) {
	t.Helper()
	r := &Readiness{}
	r.Set(true)
	m := &stubMetrics{}
	return New(r, nil, signer.New(""), WithMetrics(m)), m
}

// /metrics IS NOT ON THE FILING MUX, even when a metrics handler was supplied.
func TestMetricsIsNotServedOnTheFilingListener(t *testing.T) {
	s, m := splitServer(t)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code == http.StatusOK {
		t.Fatalf("/metrics answered %d on the filing mux — that mux is reachable from the whole "+
			"kanz-observability namespace, and putting metrics back on it is what makes the "+
			"filing routes reachable there too", rec.Code)
	}
	if m.hits != 0 {
		t.Errorf("the metrics handler was invoked %d time(s) through the filing mux", m.hits)
	}
}

// THE HANDLER IS STILL AVAILABLE TO THE COMPOSITION ROOT. Dropping it instead of
// moving it would trade an exposure for an observability outage, and a service
// nobody can scrape is its own incident.
func TestTheMetricsHandlerIsHandedToTheCompositionRoot(t *testing.T) {
	s, m := splitServer(t)

	h := s.MetricsHandler()
	if h == nil {
		t.Fatal("MetricsHandler() is nil after WithMetrics — the composition root has nothing to " +
			"serve on the metrics listener, so /metrics is simply gone")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK || m.hits != 1 {
		t.Fatalf("the handed-back handler did not serve: code=%d hits=%d", rec.Code, m.hits)
	}
}

// WITHOUT WithMetrics THERE IS NOTHING TO SERVE, and the composition root must be
// able to tell — it guards on nil rather than mounting a nil handler.
func TestMetricsHandlerIsNilWhenNoneWasSupplied(t *testing.T) {
	r := &Readiness{}
	r.Set(true)
	if h := New(r, nil, signer.New("")).MetricsHandler(); h != nil {
		t.Fatalf("MetricsHandler() = %v with no WithMetrics option, want nil", h)
	}
}

// NON-VACUITY: the filing routes still work on this mux. Without this, the
// assertion above is satisfied by a server that serves nothing at all.
func TestTheFilingRoutesAreStillServedHere(t *testing.T) {
	s, _ := splitServer(t)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz = %d on the API mux, want 200", rec.Code)
	}
	// A filing route exists and is reached (the empty body is refused by the
	// handler, not by the mux — 404 would mean the route is gone).
	rec2 := httptest.NewRecorder()
	s.ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/v1/filings/sfdr", http.NoBody))
	if rec2.Code == http.StatusNotFound {
		t.Fatal("POST /v1/filings/sfdr is no longer routed — the split moved the API instead of " +
			"the metrics")
	}
}
