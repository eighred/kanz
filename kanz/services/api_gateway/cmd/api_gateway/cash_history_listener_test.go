package main

import (
	"crypto/tls"
	"fmt"
	"github.com/eighred/kanz/internal/testidentity"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"io"
	"log"
	"net"
	"time"

	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPrivateHistoryListenerForwardsOnlyTheAccountingRead(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		forward      bool
	}{
		{"GET", "/v1/portfolios/PF/cash-commits?currency=USD", true},
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
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
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
	for _, name := range []string{"oms", "web-bff", "accounting"} {
		tr := &http.Transport{TLSClientConfig: transport.ClientTLSConfig(issue(name), transport.AuthorizeServices(gatewayID))}
		client := http.Client{Transport: tr, Timeout: 3 * time.Second}
		r, err := client.Get(fmt.Sprintf("https://%s/v1/portfolios/PF/cash-commits", ln.Addr()))
		if r != nil {
			_ = r.Body.Close()
		}
		tr.CloseIdleConnections()
		if name == "oms" {
			if err != nil || r.StatusCode != http.StatusUnauthorized {
				t.Fatal("OMS connection did not reach bearer authorization", err)
			}
		} else if err == nil {
			t.Fatalf("%s reached cash history router", name)
		}
	}
}
