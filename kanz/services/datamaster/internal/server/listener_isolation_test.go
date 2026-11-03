package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/eighred/kanz/pkg/auth"
)

func TestListenerIsolationWithForgedPrincipal(t *testing.T) {
	s, _ := newServer(t)
	s.metrics = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	ops := httptest.NewServer(s.OperationsHandler())
	defer ops.Close()
	classifier := httptest.NewServer(s.ClassifierHandler())
	defer classifier.Close()
	api := httptest.NewServer(s)
	defer api.Close()
	for _, tc := range []struct {
		base, method, path string
		want               int
	}{
		{ops.URL, "GET", "/metrics", 200}, {ops.URL, "GET", "/readyz", 200},
		{classifier.URL, "GET", "/v1/securities/INST1", 200}, {api.URL, "GET", "/v1/securities/INST1", 200},
		{ops.URL, "GET", "/v1/securities/INST1", 404}, {ops.URL, "GET", "/v1/exceptions", 404},
		{ops.URL, "POST", "/v1/exceptions/fixture/override", 404}, {ops.URL, "POST", "/v1/exceptions/fixture/override/approve", 404},
		{classifier.URL, "POST", "/v1/exceptions/fixture/override", 404}, {classifier.URL, "POST", "/v1/exceptions/fixture/override/approve", 404},
		{classifier.URL, "GET", "/v1/prices/INST1", 404}, {api.URL, "GET", "/metrics", 404},
	} {
		req, err := http.NewRequest(tc.method, tc.base+tc.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set(auth.HeaderPrincipalTenant, testTenant)
		req.Header.Set("X-Kanz-Principal-Subject", "fixture")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if res.StatusCode != tc.want {
			t.Errorf("%s %s: got %d want %d", tc.method, req.URL, res.StatusCode, tc.want)
		}
	}
}
