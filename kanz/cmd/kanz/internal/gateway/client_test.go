package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func staticToken(tok string) func() string { return func() string { return tok } }

func TestAsk(t *testing.T) {
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"answer":    "VaR is within limits.",
			"citations": []string{"risk-log@42"},
			"grounded":  true,
		})
	}))
	defer srv.Close()

	c := New(srv.URL, staticToken("TOK123"))
	res, err := c.Ask(context.Background(), "how is my risk?")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if gotAuth != "Bearer TOK123" {
		t.Fatalf("auth header = %q", gotAuth)
	}
	if gotBody == "" || !json.Valid([]byte(gotBody)) {
		t.Fatalf("request body = %q", gotBody)
	}
	if res.Answer != "VaR is within limits." || len(res.Citations) != 1 || !res.Grounded {
		t.Fatalf("bad result: %+v", res)
	}
}

func TestMeasuresQueryParams(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"measures":[]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, staticToken("t"))
	raw, err := c.Measures(context.Background(), "PORT-1", []string{"VaR99", "Delta"}, "2026-07-10T00:00:00Z")
	if err != nil {
		t.Fatalf("Measures: %v", err)
	}
	if gotPath != "/v1/portfolios/PORT-1/measures" {
		t.Fatalf("path = %q", gotPath)
	}
	// Both measures are repeated and as_of is carried.
	if gotQuery == "" || !contains(gotQuery, "measure=VaR99") || !contains(gotQuery, "measure=Delta") || !contains(gotQuery, "as_of=") {
		t.Fatalf("query = %q", gotQuery)
	}
	if string(raw) != `{"measures":[]}` {
		t.Fatalf("raw = %s", raw)
	}
}

func TestExposureNoAsOfOmitsParam(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	if _, err := New(srv.URL, staticToken("t")).Exposure(context.Background(), "P", ""); err != nil {
		t.Fatalf("Exposure: %v", err)
	}
	if gotQuery != "" {
		t.Fatalf("expected no query params, got %q", gotQuery)
	}
}

func TestAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"tenant mismatch"}`))
	}))
	defer srv.Close()

	_, err := New(srv.URL, staticToken("t")).Exposure(context.Background(), "P", "")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.Status != http.StatusForbidden || apiErr.Message != "tenant mismatch" {
		t.Fatalf("apiErr = %+v", apiErr)
	}
}

func TestNoTokenOmitsAuthHeader(t *testing.T) {
	seen := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization") != ""
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	if _, err := New(srv.URL, staticToken("")).Exposure(context.Background(), "P", ""); err != nil {
		t.Fatalf("Exposure: %v", err)
	}
	if seen {
		t.Fatal("Authorization header should be absent when token is empty")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
