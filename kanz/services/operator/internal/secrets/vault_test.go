package secrets

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestVaultStoreSetVenueKeysPuts(t *testing.T) {
	var gotPath, gotToken string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotToken = r.Header.Get("X-Vault-Token")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	s, err := NewVaultStore(srv.URL, "tok")
	if err != nil {
		t.Fatalf("NewVaultStore: %v", err)
	}
	if err := s.SetVenueKeys(context.Background(), "okx", VenueKeys{APIKey: "k", APISecret: "s", Passphrase: "p"}); err != nil {
		t.Fatalf("SetVenueKeys: %v", err)
	}
	if gotPath != "/v1/kv/data/kanz/venue-okx" {
		t.Errorf("path = %q, want /v1/kv/data/kanz/venue-okx", gotPath)
	}
	if gotToken != "tok" {
		t.Errorf("token header = %q, want tok", gotToken)
	}
	data, _ := gotBody["data"].(map[string]any)
	if data["api_key"] != "k" || data["api_secret"] != "s" || data["api_passphrase"] != "p" {
		t.Errorf("body data = %v, want the three fields", data)
	}
}

func TestVaultStoreSetVenueKeysErrorOnNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"errors":["permission denied"]}`))
	}))
	defer srv.Close()
	s, _ := NewVaultStore(srv.URL, "tok")
	if err := s.SetVenueKeys(context.Background(), "okx", VenueKeys{APIKey: "k", APISecret: "s", Passphrase: "p"}); err == nil {
		t.Fatal("want error on 403")
	}
}

func TestVaultStoreListVenues(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// KV-v2 LIST of kv/metadata/kanz — returns key names under kanz/, never values.
		_, _ = w.Write([]byte(`{"data":{"keys":["venue-okx","risk-engine","oms"]}}`))
	}))
	defer srv.Close()
	s, _ := NewVaultStore(srv.URL, "tok")
	got, err := s.ListVenues(context.Background())
	if err != nil {
		t.Fatalf("ListVenues: %v", err)
	}
	byVenue := map[string]bool{}
	for _, v := range got {
		byVenue[v.Venue] = v.Configured
	}
	if !byVenue["okx"] || byVenue["binance"] {
		t.Errorf("presence = %v, want okx configured, binance not", byVenue)
	}
}
