package main

import (
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/eighred/kanz/internal/gatewaysig"
	"github.com/eighred/kanz/internal/identity"
	"github.com/eighred/kanz/internal/platform/halt"
	"github.com/eighred/kanz/services/api_gateway/internal/authz"
	"github.com/eighred/kanz/services/api_gateway/internal/config"
	"github.com/eighred/kanz/services/api_gateway/internal/gateway"
	"github.com/eighred/kanz/services/api_gateway/internal/orders"
	"github.com/eighred/kanz/services/api_gateway/internal/proxy"
)

func TestCustodyCustomerGatewayPermissions(t *testing.T) {
	issuer := newIdentityStub(t)
	issuer.feedUp.Store(true)
	authn, revocations := buildWired(t, issuer, &captureHandler{})
	if err := revocations.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		RequiredRole: baselineRole, FundRole: treasuryRole, CustodyReadRole: "custody-pilot-read",
		CustodyRerunRole: "custody-pilot-rerun", SigningSecret: rand.Text(),
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var ready atomic.Bool
	backend := &principalCapturingBackend{}
	router, err := buildRouter(cfg, gateway.New(nil, nil, nil, "", logger), orders.New(nil, "", halt.OpenGate(nil)),
		proxy.New(backend, proxyRoles(cfg)), nil, testObservability(t), &ready, logger, nil, authn, nil)
	if err != nil {
		t.Fatal(err)
	}

	const (
		permissions = "GET /v1/permissions"
		queue       = "GET /v1/custody/breaks"
		dated       = "GET /v1/portfolios/{id}/custody/comparison"
		broker      = "GET /v1/broker/accounts"
		funding     = "GET /v1/portfolios/{id}/cash-forecast"
		rerun       = "POST /v1/portfolios/{id}/custody/reruns"
	)
	checks := []struct {
		pattern string
		service proxy.Service
	}{
		{queue, proxy.ServiceAccounting}, {dated, proxy.ServiceAccounting}, {broker, proxy.ServiceTVSync},
		{funding, proxy.ServiceAccounting}, {rerun, proxy.ServiceAccounting},
	}
	for _, tc := range []struct {
		name       string
		roles      []string
		portfolios []string
		allowed    []string
		exclusive  bool
	}{
		{"customer", []string{baselineRole, cfg.CustodyReadRole}, []string{"PF1"}, []string{permissions, queue, dated}, true},
		{"customer-with-staff-roles", []string{baselineRole, treasuryRole, cfg.CustodyRerunRole, cfg.CustodyReadRole}, []string{"PF1"}, []string{permissions, queue, dated}, true},
		{"customer-without-portfolio", []string{baselineRole, cfg.CustodyReadRole}, nil, []string{permissions}, true},
		{"customer-with-staff-roles-without-portfolio", []string{baselineRole, treasuryRole, cfg.CustodyRerunRole, cfg.CustodyReadRole}, nil, []string{permissions}, true},
		{"internal-reader", []string{baselineRole}, []string{"PF1"}, []string{permissions, broker}, false},
		{"internal-funder", []string{baselineRole, treasuryRole}, []string{"PF1"}, []string{permissions, broker, queue, dated, funding}, false},
		{"internal-rerun-operator", []string{baselineRole, treasuryRole, cfg.CustodyRerunRole}, []string{"PF1"}, []string{permissions, broker, queue, dated, funding, rerun}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const tenant = "synthetic-custody-pilot"
			token, _, err := issuer.signer.Mint(&identity.User{
				Subject: tc.name, Tenant: tenant, Portfolios: tc.portfolios, Roles: tc.roles, Status: identity.StatusActive,
			})
			if err != nil {
				t.Fatal(err)
			}
			request := func(method, path string) *httptest.ResponseRecorder {
				req := httptest.NewRequest(method, path, nil)
				req.Header.Set("Authorization", "Bearer "+token)
				gatewaysig.SignRequest(req, []byte(cfg.SigningSecret), nil)
				response := httptest.NewRecorder()
				router.ServeHTTP(response, req)
				return response
			}
			response := request(http.MethodGet, "/v1/permissions")
			if response.Code != http.StatusOK {
				t.Fatalf("permission inventory returned %d", response.Code)
			}
			var inventory struct {
				Subject, Tenant string
				Portfolios      []string
				Routes          []authz.Route
				Capabilities    []authz.Capability
			}
			if err := json.Unmarshal(response.Body.Bytes(), &inventory); err != nil {
				t.Fatal(err)
			}
			if inventory.Subject != tc.name || inventory.Tenant != tenant || !slices.Equal(inventory.Portfolios, tc.portfolios) {
				t.Fatal("permissions do not describe the authenticated customer or operator")
			}
			routes := make(map[string]authz.Capability)
			for _, route := range inventory.Routes {
				routes[route.Pattern] = route.Capability
			}
			if routes[permissions] != "session.read" {
				t.Errorf("self-introspection advertised %q", routes[permissions])
			}
			if tc.exclusive {
				wantCaps := []authz.Capability{"session.read"}
				if len(tc.portfolios) != 0 {
					wantCaps = append(wantCaps, authz.CustodyRead)
				}
				slices.Sort(wantCaps)
				if len(inventory.Routes) != len(tc.allowed) || !slices.Equal(inventory.Capabilities, wantCaps) {
					t.Errorf("customer inventory has %d routes and capabilities %v; want %d and %v", len(inventory.Routes), inventory.Capabilities, len(tc.allowed), wantCaps)
				}
			}
			for _, check := range checks {
				want := slices.Contains(tc.allowed, check.pattern)
				if _, listed := routes[check.pattern]; listed != want {
					t.Errorf("route %s advertised=%t, want %t", check.pattern, listed, want)
				}
				method, pattern, _ := strings.Cut(check.pattern, " ")
				path := strings.ReplaceAll(pattern, "{id}", "PF1")
				if check.pattern == dated {
					path += "?custodian_id=fixture&business_date=2020-02-04"
				}
				*backend = principalCapturingBackend{}
				got := request(method, path)
				if want {
					if got.Code != http.StatusAccepted || backend.service != check.service || backend.subject != tc.name || backend.tenant != tenant {
						t.Errorf("%s: expected scoped upstream call, got HTTP %d and service %q", check.pattern, got.Code, backend.service)
					}
				} else if got.Code != http.StatusForbidden || backend.service != "" {
					t.Errorf("%s: expected gateway refusal, got HTTP %d and service %q", check.pattern, got.Code, backend.service)
				}
			}
		})
	}
}
