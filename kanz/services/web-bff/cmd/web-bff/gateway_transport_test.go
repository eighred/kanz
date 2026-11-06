package main

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/clientip"
	"github.com/eighred/kanz/internal/gatewaysig"
	"github.com/eighred/kanz/internal/platform/httpserver"
	"github.com/eighred/kanz/internal/testidentity"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/eighred/kanz/services/web-bff/internal/config"
	"github.com/eighred/kanz/services/web-bff/internal/identityclient"
	"github.com/eighred/kanz/services/web-bff/internal/server"
	"github.com/eighred/kanz/services/web-bff/internal/session"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
)

func serveGatewayFixture(t *testing.T, source transport.Source, peer spiffeid.ID, handler http.Handler) string {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := httpserver.New(l.Addr().String(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			t.Error("handler reached without workload authentication")
		}
		if handler != nil {
			handler.ServeHTTP(w, r)
		} else {
			w.WriteHeader(http.StatusNoContent)
		}
	}), httpserver.Standard())
	s.ErrorLog = log.New(io.Discard, "", 0)
	s.TLSConfig = transport.ServerTLSConfig(source, transport.AuthorizeServices(peer))
	done := make(chan error, 1)
	go func() { done <- s.ServeTLS(l, "", "") }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			_ = s.Close()
			t.Error("fixture shutdown failed")
		}
		if err := <-done; err != http.ErrServerClosed {
			t.Error("fixture serving failed")
		}
	})
	return "https://" + l.Addr().String()
}

func TestGatewayWorkloadTransport(t *testing.T) {
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
	issue := func(authority *testidentity.Authority, name string, expired bool) *testidentity.Source {
		t.Helper()
		id, err := transport.ServiceID("kanz-services", name)
		if err != nil {
			t.Fatal(err)
		}
		end := time.Now().Add(time.Hour / 2)
		if expired {
			end = time.Now().Add(-time.Minute)
		}
		source, err := authority.Issue(id, time.Now().Add(-30*time.Minute), end)
		if err != nil {
			t.Fatal(err)
		}
		return source
	}
	bffID, err := transport.ServiceID("kanz-services", "web-bff")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name           string
		server, client *testidentity.Source
		allowed        bool
	}{
		{"exact_peers", issue(ca, "api-gateway", false), issue(ca, "web-bff", false), true},
		{"wrong_server", issue(ca, "identity", false), issue(ca, "web-bff", false), false},
		{"wrong_client", issue(ca, "api-gateway", false), issue(ca, "copilot", false), false},
		{"untrusted_server", issue(otherCA, "api-gateway", false), issue(ca, "web-bff", false), false},
		{"expired_server", issue(ca, "api-gateway", true), issue(ca, "web-bff", false), false},
		{"expired_client", issue(ca, "api-gateway", false), issue(ca, "web-bff", true), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := serveGatewayFixture(t, tc.server, bffID, nil)
			tr, err := gatewayTransport(base, tc.client)
			if err != nil {
				t.Fatal(err)
			}
			defer tr.CloseIdleConnections()
			client := &http.Client{Transport: tr, Timeout: 3 * time.Second}
			resp, err := client.Get(base)
			if resp != nil {
				defer resp.Body.Close()
			}
			if tc.allowed && (err != nil || resp.StatusCode != http.StatusNoContent) {
				t.Fatal("authorized workload connection refused", err)
			}
			if !tc.allowed && err == nil {
				t.Fatal("unauthorized workload connection admitted")
			}
		})
	}
	t.Run("identity_rotation_and_unavailability", func(t *testing.T) {
		server, client := issue(ca, "api-gateway", false), issue(ca, "web-bff", false)
		base := serveGatewayFixture(t, server, bffID, nil)
		tr, err := gatewayTransport(base, client)
		if err != nil {
			t.Fatal(err)
		}
		defer tr.CloseIdleConnections()
		httpc := &http.Client{Transport: tr, Timeout: 3 * time.Second}
		check := func(allowed bool) {
			t.Helper()
			tr.CloseIdleConnections()
			resp, err := httpc.Get(base)
			if resp != nil {
				_ = resp.Body.Close()
			}
			if (err == nil) != allowed {
				t.Fatal("workload rotation outcome differs")
			}
		}
		check(true)
		server.ReplaceLeaf(issue(ca, "api-gateway", false))
		client.ReplaceLeaf(issue(ca, "web-bff", false))
		check(true)
		client.ReplaceLeaf(nil)
		check(false)
		client.ReplaceLeaf(issue(ca, "web-bff", false))
		check(true)
		server.ReplaceLeaf(issue(ca, "identity", false))
		check(false)
	})
	t.Run("disabled_is_a_nil_interface", func(t *testing.T) {
		tr, closeFn, err := openGatewayTransport(context.Background(), config.Config{})
		if err != nil || tr != nil || closeFn == nil {
			t.Fatal("legacy transport acquired a typed nil or identity dependency")
		}
		closeFn()
	})
	t.Run("no_plaintext_or_missing_identity", func(t *testing.T) {
		for _, u := range []string{"http://gateway.invalid", "https://user:synthetic@gateway.invalid", "https://gateway.invalid?override=yes"} {
			if _, err := gatewayTransport(u, issue(ca, "web-bff", false)); err == nil {
				t.Fatal("invalid workload endpoint admitted")
			}
		}
		if _, err := gatewayTransport("https://gateway.invalid", nil); err == nil {
			t.Fatal("missing identity admitted")
		}
	})
	t.Run("actual_bff_session_and_signing", func(t *testing.T) {
		const token, key = "synthetic-bff-gateway-token", "synthetic-bff-gateway-signing-key"
		var reached atomic.Int32
		gatewaySource := issue(ca, "api-gateway", false)
		base := serveGatewayFixture(t, gatewaySource, bffID, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(io.LimitReader(r.Body, 4096))
			if err != nil || r.URL.Path != "/v1/proof" || string(body) != "synthetic body" ||
				r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("Cookie") != "" ||
				!gatewaysig.Verify([]byte(key), r.Method, r.URL.Path, body, r.Header.Get("X-Signature")) {
				t.Error("actual BFF session/signing boundary changed")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			reached.Add(1)
			w.WriteHeader(http.StatusNoContent)
		}))
		tr, err := gatewayTransport(base, issue(ca, "web-bff", false))
		if err != nil {
			t.Fatal(err)
		}
		defer tr.CloseIdleConnections()
		idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"token":"`+token+`","expires_at":"2099-01-01T00:00:00Z","subject":"user:tls-fixture","tenant":"tls-fixture"}`)
		}))
		defer idp.Close()
		resolver, err := clientip.NewResolver("", nil)
		if err != nil {
			t.Fatal(err)
		}
		bff, err := server.New(&server.Readiness{}, server.Options{Identity: identityclient.New(idp.URL, "", time.Second),
			ClientIP: resolver, Sessions: session.NewManager(time.Hour), GatewayURL: base, GatewayTransport: tr,
			SigningSecret: key, SecureCookies: true})
		if err != nil {
			t.Fatal(err)
		}
		defer bff.Close()
		front := httptest.NewServer(bff)
		defer front.Close()
		client := &http.Client{Timeout: 3 * time.Second}
		login, err := http.NewRequest("POST", front.URL+"/auth/login", strings.NewReader(`{"subject":"user:tls-fixture","credential":"synthetic"}`))
		if err != nil {
			t.Fatal(err)
		}
		login.Header.Set("Origin", strings.Replace(front.URL, "http://", "https://", 1))
		login.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(login)
		if err != nil {
			t.Fatal(err)
		}
		cookies := resp.Cookies()
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || len(cookies) != 1 {
			t.Fatal("actual BFF credential login failed")
		}
		request, err := http.NewRequest("POST", front.URL+"/api/v1/proof", strings.NewReader("synthetic body"))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Origin", strings.Replace(front.URL, "http://", "https://", 1))
		request.AddCookie(cookies[0])
		resp, err = client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent || reached.Load() != 1 {
			t.Fatal("BFF did not use its authorized workload transport")
		}
		gatewaySource.ReplaceLeaf(issue(ca, "identity", false))
		tr.CloseIdleConnections()
		request, err = http.NewRequest("POST", front.URL+"/api/v1/proof", strings.NewReader("synthetic body"))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Origin", strings.Replace(front.URL, "http://", "https://", 1))
		request.AddCookie(cookies[0])
		resp, err = client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadGateway || reached.Load() != 1 {
			t.Fatal("BFF fell back after gateway workload identity changed")
		}
	})
}
