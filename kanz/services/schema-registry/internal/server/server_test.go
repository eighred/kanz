package server_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kanz-eng/kanz/services/schema-registry/internal/server"
	"github.com/kanz-eng/kanz/services/schema-registry/internal/storage"
)

func TestRegisterEndpoint(t *testing.T) {
	s := server.New(storage.NewMemory(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)

	post := func(schemaID, sourceTag string, body []byte) (*http.Response, map[string]any) {
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/schemas/"+schemaID, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/octet-stream")
		if sourceTag != "" {
			req.Header.Set("X-Source-Tag", sourceTag)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		var out map[string]any
		if resp.Header.Get("Content-Type") == "application/json" {
			_ = json.NewDecoder(resp.Body).Decode(&out)
		}
		return resp, out
	}

	// First register → 201, version 1
	resp, body := post("market.v1.MarketDataEvent", "v0.5.0", []byte("descA"))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first: status=%d want 201", resp.StatusCode)
	}
	if body["ref"] != "market.v1.MarketDataEvent:1" || body["created"] != true {
		t.Errorf("first body=%v", body)
	}

	// Same content → 200, same ref
	resp, body = post("market.v1.MarketDataEvent", "v0.5.1", []byte("descA"))
	if resp.StatusCode != http.StatusOK {
		t.Errorf("idempotent: status=%d want 200", resp.StatusCode)
	}
	if body["ref"] != "market.v1.MarketDataEvent:1" || body["created"] != false {
		t.Errorf("idempotent body=%v", body)
	}

	// New content → 201, version 2
	resp, body = post("market.v1.MarketDataEvent", "v0.5.2", []byte("descB"))
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("changed: status=%d want 201", resp.StatusCode)
	}
	if body["ref"] != "market.v1.MarketDataEvent:2" {
		t.Errorf("changed body=%v", body)
	}

	// Missing tag → 400
	resp, _ = post("market.v1.MarketDataEvent", "", []byte("descC"))
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("missing tag: status=%d want 400", resp.StatusCode)
	}

	// Empty body → 400
	resp, _ = post("market.v1.MarketDataEvent", "v0.5.3", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("empty body: status=%d want 400", resp.StatusCode)
	}

	// Schema id containing ':' → 400 (ambiguous with resolve URLs)
	resp, _ = post("market.v1.MarketDataEvent:5", "v0.5.4", []byte("x"))
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("schema_id with colon: status=%d want 400", resp.StatusCode)
	}
}

func TestResolveEndpoint(t *testing.T) {
	s := server.New(storage.NewMemory(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)

	register := func(schemaID, sourceTag string, body []byte) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/schemas/"+schemaID, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Source-Tag", sourceTag)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("register: status=%d want 201", resp.StatusCode)
		}
	}

	register("market.v1.MarketDataEvent", "v0.5.0", []byte("descA"))
	register("market.v1.MarketDataEvent", "v0.5.1", []byte("descB")) // version 2

	// Resolve v1 → 200, original bytes + metadata headers
	resp, err := http.Get(ts.URL + "/schemas/market.v1.MarketDataEvent:1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("resolve v1: status=%d want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "descA" {
		t.Errorf("resolve v1 body=%q want %q", body, "descA")
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type=%q want application/octet-stream", ct)
	}
	if resp.Header.Get("X-Schema-Source-Tag") != "v0.5.0" {
		t.Errorf("X-Schema-Source-Tag=%q", resp.Header.Get("X-Schema-Source-Tag"))
	}
	if resp.Header.Get("X-Schema-Fingerprint") == "" {
		t.Error("missing X-Schema-Fingerprint")
	}
	if resp.Header.Get("ETag") == "" {
		t.Error("missing ETag")
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("Cache-Control=%q expected to contain 'immutable'", cc)
	}

	// Resolve v2 → 200, the second-registered bytes
	resp2, err := http.Get(ts.URL + "/schemas/market.v1.MarketDataEvent:2")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp2.Body.Close() })
	body2, _ := io.ReadAll(resp2.Body)
	if string(body2) != "descB" {
		t.Errorf("resolve v2 body=%q want %q", body2, "descB")
	}

	// Unknown ref → 404
	resp3, _ := http.Get(ts.URL + "/schemas/no.such.Type:5")
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusNotFound {
		t.Errorf("missing ref: status=%d want 404", resp3.StatusCode)
	}

	// Missing version → 400
	resp4, _ := http.Get(ts.URL + "/schemas/missing-version")
	resp4.Body.Close()
	if resp4.StatusCode != http.StatusBadRequest {
		t.Errorf("missing version: status=%d want 400", resp4.StatusCode)
	}

	// Version 0 → 400
	resp5, _ := http.Get(ts.URL + "/schemas/x.y.Z:0")
	resp5.Body.Close()
	if resp5.StatusCode != http.StatusBadRequest {
		t.Errorf("version 0: status=%d want 400", resp5.StatusCode)
	}

	// Non-numeric version → 400
	resp6, _ := http.Get(ts.URL + "/schemas/x.y.Z:abc")
	resp6.Body.Close()
	if resp6.StatusCode != http.StatusBadRequest {
		t.Errorf("non-numeric version: status=%d want 400", resp6.StatusCode)
	}
}
