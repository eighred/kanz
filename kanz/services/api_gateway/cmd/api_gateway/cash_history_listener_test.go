package main

import (
	"context"
	"crypto/tls"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/gatewaysig"
	"github.com/eighred/kanz/internal/identity"
	"github.com/eighred/kanz/internal/testidentity"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/eighred/kanz/services/api_gateway/internal/config"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
)

func TestPrivateHistoryListenerForwardsOnlyTheAccountingRead(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		forward      bool
	}{
		{"GET", "/v1/portfolios/PF/cash-commits?currency=USD&after=1&through=3&limit=2", true},
		{"GET", "/v1/portfolios/PF%2Fsub/cash-commits", true},
		{"GET", "/v1/portfolios/PF/collateral/cash-commits", false},
		{"GET", "/v1/portfolios/PF/cash-forecast", false},
		{"HEAD", "/v1/portfolios/PF/cash-commits", false},
		{"POST", "/v1/portfolios/PF/cash-commits", false},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			calls := 0
			handler := onlyCashHistory(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(http.StatusNoContent) }))
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
			if tc.forward {
				if calls != 1 || w.Code != http.StatusNoContent {
					t.Fatal("expected one authorized-router call", calls, w.Code)
				}
			} else if calls != 0 || w.Code != http.StatusNotFound {
				t.Fatal("unexpected private route", calls, w.Code)
			}
		})
	}
}

func TestOnlyOMSWorkloadCanReachHistoryListener(t *testing.T) {
	td, err := spiffeid.TrustDomainFromString(transport.TrustDomain)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := testidentity.NewAuthority(td)
	if err != nil {
		t.Fatal(err)
	}
	issue := func(name string) *testidentity.Source {
		id, err := transport.ServiceID("kanz-services", name)
		if err != nil {
			t.Fatal(err)
		}
		s, err := ca.Issue(id, time.Now().Add(-time.Minute), time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	gatewayID, err := transport.ServiceID("kanz-services", "api-gateway")
	if err != nil {
		t.Fatal(err)
	}
	const (
		historyKey  = "synthetic-history-key"
		ordinaryKey = "synthetic-public-key"
	)
	cfg := config.Config{RequiredRole: baselineRole, TradeRole: traderRole, CashHistoryRole: "oms-history",
		SigningSecret: ordinaryKey, CashHistorySigningSecret: historyKey, MFARequired: true}
	issuer := newIdentityStub(t)
	issuer.feedUp.Store(true)
	authn, revocations := buildWired(t, issuer, &captureHandler{})
	if err := revocations.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	mint := func(tenant, role string, portfolios ...string) string {
		t.Helper()
		token, _, err := issuer.signer.Mint(&identity.User{
			Subject: "workload:oms", Tenant: tenant, Roles: []string{role}, Portfolios: portfolios, Status: identity.StatusActive,
		})
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	historyToken := mint("fixture", "oms-history", "PF")
	handler := preAuthRouter(t, cfg, authn)
	srv, err := newOMSHistoryServer("127.0.0.1:0", handler, issue("api-gateway"))
	if err != nil {
		t.Fatal(err)
	}
	srv.ErrorLog = log.New(io.Discard, "", 0)
	ln, err := net.Listen("tcp4", srv.Addr)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(tls.NewListener(ln, srv.TLSConfig)) }()
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Error(err)
		}
		if err := <-done; err != http.ErrServerClosed {
			t.Error(err)
		}
	})
	const path = "/v1/portfolios/PF/cash-commits?currency=USD&after=1&through=3&limit=2"
	for _, tc := range []struct {
		name, peer, key, bearer string
		want                    int // zero means there must be no HTTP response
	}{
		{"authorized", "oms", historyKey, historyToken, http.StatusAccepted},
		{"missing signature", "oms", "", historyToken, http.StatusUnauthorized},
		{"public signature", "oms", ordinaryKey, historyToken, http.StatusUnauthorized},
		{"missing bearer", "oms", historyKey, "", http.StatusUnauthorized},
		{"unverified bearer", "oms", historyKey, "invalid", http.StatusUnauthorized},
		{"ordinary bearer", "oms", historyKey, mint("fixture", baselineRole, "PF"), http.StatusForbidden},
		{"other portfolio", "oms", historyKey, mint("fixture", "oms-history", "other"), http.StatusForbidden},
		{"no portfolio grant", "oms", historyKey, mint("fixture", "oms-history"), http.StatusForbidden},
		{"no tenant", "oms", historyKey, mint("", "oms-history", "PF"), http.StatusForbidden},
		{"BFF peer", "web-bff", historyKey, historyToken, 0},
		{"accounting peer", "accounting", historyKey, historyToken, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := &http.Transport{TLSClientConfig: transport.ClientTLSConfig(issue(tc.peer), transport.AuthorizeServices(gatewayID))}
			defer tr.CloseIdleConnections()
			client := http.Client{Transport: tr, Timeout: 3 * time.Second}
			req, err := http.NewRequest(http.MethodGet, "https://"+ln.Addr().String()+path, nil)
			if err != nil {
				t.Fatal(err)
			}
			if tc.bearer != "" {
				req.Header.Set("Authorization", "Bearer "+tc.bearer)
			}
			gatewaysig.SignRequest(req, []byte(tc.key), nil)
			resp, err := client.Do(req)
			if resp != nil {
				defer resp.Body.Close()
			}
			if tc.want == 0 {
				if err == nil || resp != nil {
					t.Fatal("unexpected HTTP response for unauthorized workload")
				}
			} else if err != nil || resp.StatusCode != tc.want {
				if err != nil {
					t.Fatal(err)
				}
				t.Fatalf("history status %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
	for _, tc := range []struct {
		key, path string
		want      int
	}{
		{historyKey, path, http.StatusUnauthorized}, {ordinaryKey, path, http.StatusForbidden},
		{ordinaryKey, "/v1/permissions", http.StatusForbidden},
	} {
		r := httptest.NewRequest(http.MethodGet, tc.path, nil)
		r.Header.Set("Authorization", "Bearer "+historyToken)
		gatewaysig.SignRequest(r, []byte(tc.key), nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("ordinary listener status %d, want %d", w.Code, tc.want)
		}
	}
}
