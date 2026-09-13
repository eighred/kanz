package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPriceObservationHasNoEvaluationCommand(t *testing.T) {
	backend := &fakeBackend{resp: Response{Status: http.StatusOK, Body: []byte(`{"observation_only":true}`)}}
	mux := testMux()
	New(backend, Roles{}).Routes(mux)
	read := httptest.NewRecorder()
	mux.ServeHTTP(read, authed(httptest.NewRequest(http.MethodGet, "/v1/prices/X", nil), "reader", "tenant"))
	if read.Code != http.StatusOK {
		t.Fatalf("Read capability cannot observe: %d", read.Code)
	}
	for _, path := range []string{"/v1/prices/X", "/v1/prices/X/evaluate"} {
		write := httptest.NewRecorder()
		mux.ServeHTTP(write, authed(httptest.NewRequest(http.MethodPost, path, nil), "reader", "tenant"))
		if write.Code != http.StatusMethodNotAllowed && write.Code != http.StatusNotFound {
			t.Fatalf("unexpected evaluation route: %s: %d", path, write.Code)
		}
	}
}
