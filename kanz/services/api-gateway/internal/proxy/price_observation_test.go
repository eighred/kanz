package proxy

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/eighred/kanz/pkg/auth"
)

func TestPriceObservationHasNoEvaluationCommand(t *testing.T) {
	backend := &fakeBackend{resp: Response{Status: http.StatusOK, Body: []byte(`{"observation_only":true}`)}}
	mux := testMux()
	New(backend, Roles{}).Routes(mux)
	read := httptest.NewRecorder()
	mux.ServeHTTP(read, authed(httptest.NewRequest(http.MethodGet, "/v1/price-observations/X", nil), "reader", "tenant"))
	if read.Code != http.StatusOK {
		t.Fatalf("Read capability cannot observe: %d", read.Code)
	}
	for _, path := range []string{"/v1/price-observations/X", "/v1/price-observations/X/evaluate"} {
		write := httptest.NewRecorder()
		mux.ServeHTTP(write, authed(httptest.NewRequest(http.MethodPost, path, nil), "reader", "tenant"))
		if write.Code != http.StatusMethodNotAllowed && write.Code != http.StatusNotFound {
			t.Fatalf("unexpected evaluation route: %s: %d", path, write.Code)
		}
	}
}

func TestPriceObservationPreservesIdentifierOverHTTP(t *testing.T) {
	for _, id := range []string{"BTC/USD", "X?as_of=other", "X#fragment", "X%2FY"} {
		t.Run(id, func(t *testing.T) {
			upstreamMux := http.NewServeMux()
			upstreamMux.HandleFunc("GET /v1/price-observations/{id}", func(w http.ResponseWriter, r *http.Request) {
				if r.PathValue("id") != id || r.URL.RawQuery != "probe=1" {
					t.Errorf("identifier changed: %q query=%q", r.PathValue("id"), r.URL.RawQuery)
				}
				if r.Header.Get(auth.HeaderPrincipalTenant) != "tenant" {
					t.Error("lost authenticated tenant")
				}
				w.WriteHeader(http.StatusNoContent)
			})
			upstream := httptest.NewServer(upstreamMux)
			defer upstream.Close()
			backend := NewMeshBackend(map[Service]string{ServiceDataMaster: upstream.URL}, upstream.Client())
			mux := testMux()
			New(backend, Roles{}).Routes(mux)
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, authed(httptest.NewRequest(http.MethodGet, "/v1/price-observations/"+url.PathEscape(id)+"?probe=1", nil), "reader", "tenant"))
			if response.Code != http.StatusNoContent {
				t.Fatalf("proxy status: %d", response.Code)
			}
		})
	}
}
