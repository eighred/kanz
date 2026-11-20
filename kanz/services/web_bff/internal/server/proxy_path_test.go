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

func TestProxyReachesVersionedAuditSearchWithFilters(t *testing.T) {
	gateway := http.NewServeMux()
	gateway.HandleFunc("GET /v1/audit/events", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("event_type") != "order.order.healed" || r.URL.Query().Get("since") != "2026-09-01T00:00:00Z" || r.URL.Query().Get("limit") != "100" {
			t.Error("audit search filters changed")
		}
		w.WriteHeader(http.StatusNoContent)
	})
	upstream := httptest.NewServer(gateway)
	defer upstream.Close()
	srv := bffWithSigning(t, upstream.URL, "")
	response := proxyAs(t, srv, http.MethodGet, "/api/v1/audit/events?limit=100&event_type=order.order.healed&since=2026-09-01T00%3A00%3A00Z", "")
	if response.Code != http.StatusNoContent {
		t.Fatalf("audit proxy status: %d", response.Code)
	}
}

func TestCustodyQueueBFFPreservesContinuationAndStatus(t *testing.T) {
	for _, tc := range []struct {
		query  string
		status int
		body   string
	}{
		{"limit=1&after=UEZ8Q3xxdWFudGl0eXzpm6o", 200, `{"count":1,"next_cursor":"cTE6YWJj"}`},
		{"limit=1&after=YQ&after=Yg", 400, `{"error":"Invalid custody queue cursor."}`},
		{"", 422, `{"code":"custody_queue_pagination_required","limit":4096}`},
	} {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/custody/breaks" || r.URL.RawQuery != tc.query {
				t.Errorf("changed URL %s", r.URL)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		s := bffWithSigning(t, upstream.URL, "")
		w := proxyAs(t, s, http.MethodGet, "/api/v1/custody/breaks?"+tc.query, "")
		upstream.Close()
		if w.Code != tc.status || w.Body.String() != tc.body {
			t.Fatalf("%s status %d body %s", tc.query, w.Code, w.Body.String())
		}
	}
}
