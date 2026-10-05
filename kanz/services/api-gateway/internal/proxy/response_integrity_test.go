package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/api-gateway/internal/authz"
	"github.com/eighred/kanz/services/api-gateway/internal/middleware"
)

func TestGatewayPreservesPrincipalAndCachePolicy(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		p, ok := auth.PrincipalFromHeaders(r.Header)
		if !ok || p.Subject != "operator" || p.Tenant != "tenant" || !auth.PortfolioEntitled(p.Portfolios, "PF") ||
			r.URL.Path != "/v1/portfolios/PF/cash-forecast" || r.URL.Query().Get("currency") != "USD" {
			http.Error(w, "identity or query lost", 400)
			return
		}
		w.Header().Add("Cache-Control", "private")
		w.Header().Add("Cache-Control", "no-store")
		w.Header().Set("X-Internal-Only", "must-not-forward")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"currency":"USD"}`))
	}))
	defer upstream.Close()
	handler := New(NewMeshBackend(map[Service]string{ServiceAccounting: upstream.URL}, upstream.Client()), Roles{Fund: "treasury"})
	m := authz.NewMux(authz.Grants{"treasury": {authz.Fund}, "trader": {authz.Trade}, "analyst": {authz.Read}}, nil)
	handler.Routes(m)
	for _, role := range []string{"trader", "analyst", "treasury"} {
		r := httptest.NewRequest(http.MethodGet, "/v1/portfolios/PF/cash-forecast?currency=USD", nil)
		r = r.WithContext(middleware.WithPrincipal(r.Context(), &middleware.Principal{Subject: "operator", Tenant: "tenant", Roles: []string{role}, Portfolios: []string{"PF"}}))
		w := httptest.NewRecorder()
		m.ServeHTTP(w, r)
		if role != "treasury" {
			if w.Code != 403 || calls.Load() != 0 {
				t.Fatalf("role %s reached financial endpoint: status=%d calls=%d", role, w.Code, calls.Load())
			}
			continue
		}
		if w.Code != 200 || calls.Load() != 1 || w.Header().Get("Cache-Control") != "private, no-store" || w.Header().Get("X-Internal-Only") != "" || w.Body.String() != `{"currency":"USD"}` {
			t.Fatalf("upstream response changed: status=%d headers=%v body=%s", w.Code, w.Header(), w.Body.String())
		}
	}
}

func TestGatewayNeverReturnsAnOversizedUpstreamPrefixAsSuccess(t *testing.T) {
	for _, tc := range []struct {
		name    string
		size    int
		chunked bool
		status  int
	}{{"exact limit", maxRespBytes, false, 200}, {"oversized", maxRespBytes + 1, false, 502}, {"chunked oversized", maxRespBytes + 1, true, 502}} {
		t.Run(tc.name, func(t *testing.T) {
			payload := bytes.Repeat([]byte{'x'}, tc.size)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/octet-stream")
				if tc.chunked {
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
				} else {
					w.Header().Set("Content-Length", strconv.Itoa(tc.size))
				}
				_, _ = w.Write(payload)
			}))
			defer upstream.Close()
			h := New(NewMeshBackend(map[Service]string{ServiceAccounting: upstream.URL}, upstream.Client()), Roles{Fund: "treasury"})
			m := mux("treasury", authz.Fund)
			h.Routes(m)
			r := as(httptest.NewRequest(http.MethodGet, "/v1/portfolios/PF/cash-forecast?currency=USD", nil), "treasury")
			w := httptest.NewRecorder()
			m.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status=%d want=%d", w.Code, tc.status)
			}
			if tc.status == 200 && !bytes.Equal(w.Body.Bytes(), payload) {
				t.Fatal("exact-limit payload changed")
			}
			if tc.status == 502 && w.Body.Len() > 1024 {
				t.Fatal("overflow returned partial upstream evidence")
			}
		})
	}
}
