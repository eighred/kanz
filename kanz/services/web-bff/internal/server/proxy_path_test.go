package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestProxyPreservesEscapedInstrumentIdentifier(t *testing.T) {
	for _, id := range []string{"BTC/USD", "X?as_of=other", "X#fragment", "X%2FY"} {
		t.Run(id, func(t *testing.T) {
			gateway := http.NewServeMux()
			gateway.HandleFunc("GET /v1/price-observations/{id}", func(w http.ResponseWriter, r *http.Request) {
				if r.PathValue("id") != id || r.URL.RawQuery != "probe=1" {
					t.Errorf("identifier changed: %q query=%q", r.PathValue("id"), r.URL.RawQuery)
				}
				w.WriteHeader(http.StatusNoContent)
			})
			upstream := httptest.NewServer(gateway)
			defer upstream.Close()
			srv := bffWithSigning(t, upstream.URL, "")
			response := proxyAs(t, srv, http.MethodGet, "/api/v1/price-observations/"+url.PathEscape(id)+"?probe=1", "")
			if response.Code != http.StatusNoContent {
				t.Fatalf("proxy status: %d", response.Code)
			}
		})
	}
}
