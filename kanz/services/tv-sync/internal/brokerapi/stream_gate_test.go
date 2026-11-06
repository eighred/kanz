package brokerapi

import (
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/tv-sync/internal/projection"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// These component transport tests explicitly opt into an always-ready fixture;
// server tests separately qualify the real revocable serving generation.
type readyStreamFixture struct{}

func (readyStreamFixture) AcquireStream() (StreamLease, bool) { return readyStreamFixture{}, true }
func (readyStreamFixture) Done() <-chan struct{}              { return nil }
func (readyStreamFixture) Write(fn func() error) error        { return fn() }
func readyHandler(p *projection.Projection) *Handler {
	h := New(p)
	h.SetStreamGate(readyStreamFixture{})
	return h
}
func TestStreamRefusesUnwiredServingGate(t *testing.T) {
	h := New(projection.New(time.Now, nil))
	mux := http.NewServeMux()
	h.Routes(mux)
	r := httptest.NewRequest("GET", "/broker/accounts/fund/stream", nil)
	auth.SetPrincipalHeaders(r.Header, "fixture", "tenant", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired stream status%d", w.Code)
	}
}
