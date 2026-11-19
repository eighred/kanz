package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/services/web_bff/internal/oidc"
	"github.com/eighred/kanz/services/web_bff/internal/session"
)

// Read the operator-facing table so that an extra registration, a stale entry or a
// switch to another handler fails the same test, including in optional profiles.
type routeContract struct {
	profile         string
	handler         string
	sessionRequired bool
}

func documentedRoutes(t *testing.T) map[string]routeContract {
	t.Helper()
	data, err := os.ReadFile("../../routes.md")
	if err != nil {
		t.Fatal(err)
	}
	routes := make(map[string]routeContract)
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "| ") || strings.HasPrefix(line, "| ServeMux pattern |") || strings.HasPrefix(line, "| ---") {
			continue
		}
		columns := strings.Split(strings.Trim(strings.TrimSpace(line), "|"), "|")
		if len(columns) != 8 {
			t.Fatalf("expected eight contract columns in %q", line)
		}
		for i := range columns {
			columns[i] = strings.TrimSpace(columns[i])
			if columns[i] == "" {
				t.Fatalf("empty contract column in %q", line)
			}
		}
		pattern, profile, handler := columns[0], columns[1], columns[2]
		switch profile {
		case "always", "metrics", "oidc", "static":
		default:
			t.Fatalf("route %q has unknown registration profile %q", pattern, profile)
		}
		if _, exists := routes[pattern]; exists {
			t.Fatalf("duplicate documented pattern %q", pattern)
		}
		if columns[3] != "required" && columns[3] != "optional" {
			t.Fatalf("route %q has unknown session contract %q", pattern, columns[3])
		}
		routes[pattern] = routeContract{profile: profile, handler: handler, sessionRequired: columns[3] == "required"}
	}
	return routes
}

type recordedBFFRoutes struct {
	mux      *http.ServeMux
	handlers map[string]string
	metrics  http.Handler
	static   *staticHandler
}

func (r *recordedBFFRoutes) Handle(pattern string, handler http.Handler) {
	r.mux.Handle(pattern, handler)
	switch handler {
	case r.metrics:
		r.handlers[pattern] = "Metrics"
	case r.static:
		r.handlers[pattern] = "staticHandler"
	default:
		r.handlers[pattern] = fmt.Sprintf("%T", handler)
	}
}

func (r *recordedBFFRoutes) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	r.mux.HandleFunc(pattern, handler)
	name := runtime.FuncForPC(reflect.ValueOf(handler).Pointer()).Name()
	r.handlers[pattern] = strings.TrimSuffix(name[strings.LastIndex(name, ".")+1:], "-fm")
}

func TestBrowserRouteContracts(t *testing.T) {
	contracts := documentedRoutes(t)
	for _, withOIDC := range []bool{false, true} {
		for _, withMetrics := range []bool{false, true} {
			for _, withStatic := range []bool{false, true} {
				name := fmt.Sprintf("oidc=%t/metrics=%t/static=%t", withOIDC, withMetrics, withStatic)
				t.Run(name, func(t *testing.T) {
					s := new(Server)
					if withOIDC {
						s.oidc = new(oidc.Client)
					}
					if withMetrics {
						s.metrics = http.NewServeMux()
					}
					if withStatic {
						s.static = new(staticHandler)
					}
					actual := &recordedBFFRoutes{
						mux: http.NewServeMux(), handlers: make(map[string]string),
						metrics: s.metrics, static: s.static,
					}
					s.routes(actual)
					enabled := map[string]bool{"always": true, "oidc": withOIDC, "metrics": withMetrics, "static": withStatic}
					for pattern, contract := range contracts {
						if !enabled[contract.profile] {
							continue
						}
						if got := actual.handlers[pattern]; got != contract.handler {
							t.Errorf("%s: expected %s, registered %q", pattern, contract.handler, got)
						}
						delete(actual.handlers, pattern)
					}
					for pattern, handler := range actual.handlers {
						t.Errorf("unexpected registration %s -> %s", pattern, handler)
					}
				})
			}
		}
	}
}

func TestDocumentedSessionRoutesRefuseMissingSession(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("a request without a BFF session reached an upstream")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(upstream.Close)
	s, err := New(&Readiness{}, Options{
		Identity: testIdentity(t, upstream.URL), ClientIP: testClientIP(t),
		Sessions: session.NewManager(time.Hour), GatewayURL: upstream.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})

	for pattern, contract := range documentedRoutes(t) {
		if !contract.sessionRequired {
			continue
		}
		t.Run(pattern, func(t *testing.T) {
			method, path, found := strings.Cut(pattern, " ")
			if !found {
				method, path = http.MethodGet, pattern+"v1/permissions"
			}
			path = strings.NewReplacer("{id}", "fixture-id", "{subject}", "fixture-subject").Replace(path)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(method, path, nil)
			s.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized || rec.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("expected 401 JSON, received %d %q", rec.Code, rec.Header().Get("Content-Type"))
			}
			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body) != 1 || body["error"] == "" {
				t.Error("expected a JSON object with a single nonempty error")
			}
		})
	}
}
