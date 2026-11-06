package main

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/gatewaysig"
	"github.com/eighred/kanz/internal/platform/httpserver"
	"github.com/eighred/kanz/internal/testidentity"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/eighred/kanz/services/api-gateway/internal/config"
	"github.com/eighred/kanz/services/api-gateway/internal/middleware"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
)

func TestBFFWorkloadListener(t *testing.T) {
	td, err := spiffeid.TrustDomainFromString(transport.TrustDomain)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := testidentity.NewAuthority(td)
	if err != nil {
		t.Fatal(err)
	}
	otherCA, err := testidentity.NewAuthority(td)
	if err != nil {
		t.Fatal(err)
	}
	issue := func(authority *testidentity.Authority, name string) *testidentity.Source {
		t.Helper()
		id, err := transport.ServiceID("kanz-services", name)
		if err != nil {
			t.Fatal(err)
		}
		s, err := authority.Issue(id, time.Now().Add(-time.Minute), time.Now().Add(30*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	serverSource := issue(ca, "api-gateway")
	apiID, err := transport.ServiceID("kanz-services", "api-gateway")
	if err != nil {
		t.Fatal(err)
	}
	const key = "synthetic-tls-gateway-signing"
	start := func(t *testing.T, roles []string) string {
		t.Helper()
		router := preAuthRouter(t, config.Config{RequiredRole: baselineRole, TradeRole: traderRole, SigningSecret: key},
			stubAuthenticator{principal: &middleware.Principal{Subject: "user:tls-proof", Tenant: "tls-proof", Roles: roles}})
		s, err := newBFFTLSServer("127.0.0.1:0", router, httpserver.Standard(), serverSource)
		if err != nil {
			t.Fatal(err)
		}
		s.ErrorLog = log.New(io.Discard, "", 0)
		l, err := net.Listen("tcp4", s.Addr)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- s.ServeTLS(l, "", "") }()
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := s.Shutdown(ctx); err != nil {
				_ = s.Close()
				t.Error("gateway fixture shutdown failed")
			}
			if err := <-done; err != http.ErrServerClosed {
				t.Error("gateway fixture serving failed")
			}
		})
		return "https://" + l.Addr().String()
	}
	t.Run("workload_does_not_bypass_user_controls", func(t *testing.T) {
		base := start(t, []string{baselineRole})
		tr := &http.Transport{TLSClientConfig: transport.ClientTLSConfig(issue(ca, "web-bff"), transport.AuthorizeServices(apiID)), TLSHandshakeTimeout: time.Second}
		defer tr.CloseIdleConnections()
		client := &http.Client{Transport: tr, Timeout: 3 * time.Second}
		for _, tc := range []struct {
			name          string
			signed, token bool
			status        int
		}{
			{"unsigned", false, true, http.StatusUnauthorized},
			{"no_bearer", true, false, http.StatusUnauthorized},
			{"signed_authenticated", true, true, http.StatusOK},
		} {
			t.Run(tc.name, func(t *testing.T) {
				r, err := http.NewRequest("GET", base+"/v1/permissions", nil)
				if err != nil {
					t.Fatal(err)
				}
				if tc.token {
					r.Header.Set("Authorization", "Bearer synthetic")
				}
				if tc.signed {
					gatewaysig.SignRequest(r, []byte(key), nil)
				}
				resp, err := client.Do(r)
				if err != nil {
					t.Fatal(err)
				}
				_ = resp.Body.Close()
				if resp.StatusCode != tc.status {
					t.Fatalf("user control verdict %d, want %d", resp.StatusCode, tc.status)
				}
			})
		}
	})
	t.Run("wrong_workload_and_untrusted_certificate", func(t *testing.T) {
		base := start(t, []string{baselineRole})
		for _, source := range []*testidentity.Source{issue(ca, "copilot"), issue(otherCA, "web-bff")} {
			tr := &http.Transport{TLSClientConfig: transport.ClientTLSConfig(source, transport.AuthorizeServices(apiID)), TLSHandshakeTimeout: time.Second}
			resp, err := (&http.Client{Transport: tr, Timeout: 3 * time.Second}).Get(base + "/healthz")
			if resp != nil {
				_ = resp.Body.Close()
			}
			tr.CloseIdleConnections()
			if err == nil {
				t.Fatal("unauthorized workload reached even the health route")
			}
		}
	})
	t.Run("missing_client_certificate", func(t *testing.T) {
		base := start(t, []string{baselineRole})
		tr := &http.Transport{TLSClientConfig: tlsconfig.TLSClientConfig(serverSource, transport.AuthorizeServices(apiID)), TLSHandshakeTimeout: time.Second}
		defer tr.CloseIdleConnections()
		resp, err := (&http.Client{Transport: tr, Timeout: 3 * time.Second}).Get(base + "/healthz")
		if resp != nil {
			_ = resp.Body.Close()
		}
		if err == nil {
			t.Fatal("missing workload certificate admitted")
		}
	})
	t.Run("workload_does_not_grant_role", func(t *testing.T) {
		base := start(t, []string{"unrelated-role"})
		tr := &http.Transport{TLSClientConfig: transport.ClientTLSConfig(issue(ca, "web-bff"), transport.AuthorizeServices(apiID)), TLSHandshakeTimeout: time.Second}
		defer tr.CloseIdleConnections()
		r, err := http.NewRequest("GET", base+"/v1/permissions", nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer synthetic")
		gatewaysig.SignRequest(r, []byte(key), nil)
		resp, err := (&http.Client{Transport: tr, Timeout: 3 * time.Second}).Do(r)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatal("workload certificate granted a user role")
		}
	})
	t.Run("missing_router_or_source", func(t *testing.T) {
		if _, err := newBFFTLSServer(":9443", nil, httpserver.Standard(), serverSource); err == nil {
			t.Fatal("nil router admitted")
		}
		if _, err := newBFFTLSServer(":9443", http.NotFoundHandler(), httpserver.Standard(), nil); err == nil {
			t.Fatal("nil source admitted")
		}
	})
}
