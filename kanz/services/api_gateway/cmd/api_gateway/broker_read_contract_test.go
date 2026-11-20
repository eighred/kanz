package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/eighred/kanz/services/api_gateway/internal/proxy"
)

// The producer's registered handlers must emit the same fixture (TestBrowserAccountReadContract).
// Replay it at the upstream seam to exercise the public router and discovery together.
type brokerFixture struct {
	data map[string]json.RawMessage
	last proxy.Request
}

func (b *brokerFixture) Forward(_ context.Context, r proxy.Request) (proxy.Response, error) {
	b.last = r
	return proxy.Response{Status: 200, ContentType: "application/json", Body: b.data[path.Base(r.Path)]}, nil
}

func TestBrokerReadsMatchPublicDiscovery(t *testing.T) {
	data, err := os.ReadFile("../../../tv_sync/internal/brokerapi/testdata/account_read.json")
	if err != nil {
		t.Fatal(err)
	}
	be := &brokerFixture{}
	if err := json.Unmarshal(data, &be.data); err != nil {
		t.Fatal(err)
	}
	router := routerWithBackend(t, be, treasuryRole, baselineRole)
	get := func(url string, authenticated bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, url, nil)
		if authenticated {
			req.Header.Set("Authorization", "Bearer a-token")
		}
		out := httptest.NewRecorder()
		router.ServeHTTP(out, req)
		return out
	}
	doc := get("/openapi.json", false)
	if doc.Code != 200 {
		t.Fatalf("GET /openapi.json = %d", doc.Code)
	}
	type schema struct {
		Type, Format, Pattern string
		Ref                   string `json:"$ref"`
		Required              []string
		Nullable              bool
		Items                 *schema
		Properties            map[string]schema
	}
	type operation struct {
		Capability string `json:"x-kanz-capability"`
		Parameters []struct {
			Name, In string
			Required bool
			Schema   schema
		}
		Responses map[string]struct {
			Content map[string]struct{ Schema schema }
		}
	}
	var spec struct {
		Servers    []struct{ URL string }
		Security   []map[string][]string
		Components struct{ Schemas map[string]schema }
		Paths      map[string]map[string]operation
	}
	if err := json.Unmarshal(doc.Body.Bytes(), &spec); err != nil {
		t.Fatal(err)
	}
	if len(spec.Servers) != 1 || spec.Servers[0].URL != "/v1" || len(spec.Security) != 1 || spec.Security[0]["bearerAuth"] == nil {
		t.Fatal("missing public server or bearer scheme")
	}
	const base = "/broker/accounts"
	brokerCount := 0
	for p := range spec.Paths {
		if strings.HasPrefix(p, "/broker/") {
			brokerCount++
		}
	}
	if brokerCount != 5 {
		t.Fatalf("published %d broker paths", brokerCount)
	}
	for _, tc := range []struct {
		name, response, row string
		window              bool
	}{
		{"accounts", "BrokerAccountsResponse", "BrokerAccount", false},
		{"state", "BrokerState", "", false},
		{"positions", "BrokerPositionsResponse", "BrokerPosition", false},
		{"orders", "BrokerOrdersResponse", "BrokerOrder", true},
		{"executions", "BrokerExecutionsResponse", "BrokerExecution", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			route := base
			var params []string
			if tc.name != "accounts" {
				route += "/{id}/" + tc.name
				params = []string{"id", "as_of"}
			}
			if tc.name == "executions" {
				params = append(params, "instrument")
			}
			ops := spec.Paths[route]
			op := ops["get"]
			if len(ops) != 1 || op.Capability != "read" {
				t.Fatal("expected only a capability-gated GET")
			}
			var gotParams []string
			for _, p := range op.Parameters {
				gotParams = append(gotParams, p.Name)
				if p.Required != (p.Name == "id") || (p.In == "path") != (p.Name == "id") || p.Schema.Type != "string" || (p.Schema.Format == "date-time") != (p.Name == "as_of") {
					t.Fatalf("parameter %+v", p)
				}
			}
			if !slices.Equal(gotParams, params) {
				t.Fatalf("parameters %v, want %v", gotParams, params)
			}
			for _, code := range []string{"400", "401", "403", "404", "413", "429", "502", "503"} {
				if _, ok := op.Responses[code]; !ok {
					t.Fatalf("missing response %s", code)
				}
			}
			if _, gone := op.Responses["410"]; gone != (tc.name != "accounts") {
				t.Fatal("incorrect history refusal")
			}
			if ref := op.Responses["200"].Content["application/json"].Schema.Ref; ref != "#/components/schemas/"+tc.response {
				t.Fatalf("response ref %q", ref)
			}
			body := spec.Components.Schemas[tc.response]
			if tc.row != "" {
				list := body.Properties[tc.name]
				if !slices.Equal(body.Required, []string{tc.name}) || list.Type != "array" || !list.Nullable || list.Items == nil || list.Items.Ref != "#/components/schemas/"+tc.row {
					t.Fatal("required nullable list or item type changed")
				}
			}
			horizon, ok := body.Properties["retainedFrom"]
			if ok != tc.window || ok && (horizon.Format != "date-time" || horizon.Nullable) {
				t.Fatalf("incorrect retention horizon: %+v", horizon)
			}
			url := strings.ReplaceAll(route, "{id}", "fund-a")
			be.last = proxy.Request{}
			if resp := get("/v1"+url, false); resp.Code != 401 || be.last.Service != "" {
				t.Fatalf("unauthenticated read: %d", resp.Code)
			}
			query := ""
			if tc.name != "accounts" {
				query = "?as_of=2026-01-02T03:04:05.123456789Z"
			}
			if tc.name == "executions" {
				query += "&instrument=X"
			}
			resp := get("/v1"+url+query, true)
			if resp.Code != 200 || be.last.Service != proxy.ServiceTVSync || be.last.Method != "GET" || be.last.Path != url || be.last.Principal == nil || be.last.Principal.Tenant != fundingTenant || !bytes.Equal(resp.Body.Bytes(), be.data[tc.name]) {
				t.Fatalf("registered route or response differs for %s: %d", url, resp.Code)
			}
			if tc.name != "accounts" && be.last.Query.Get("as_of") != "2026-01-02T03:04:05.123456789Z" || tc.name == "executions" && be.last.Query.Get("instrument") != "X" {
				t.Fatal("forwarded query changed")
			}
		})
	}
	for _, field := range []struct{ row, key string }{
		{"BrokerState", "balance"}, {"BrokerPosition", "qty"}, {"BrokerOrder", "limitPrice"}, {"BrokerExecution", "price"},
	} {
		value := spec.Components.Schemas[field.row].Properties[field.key]
		if value.Type != "string" || value.Pattern == "" {
			t.Fatalf("missing fixed-point string for %s.%s", field.row, field.key)
		}
	}
}
