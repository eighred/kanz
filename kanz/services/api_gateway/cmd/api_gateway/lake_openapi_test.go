package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/eighred/kanz/internal/gatewaysig"
	"github.com/eighred/kanz/internal/platform/halt"
	lakepb "github.com/eighred/kanz/kanz-schemas-go/lake/v1"
	"github.com/eighred/kanz/services/api_gateway/internal/config"
	"github.com/eighred/kanz/services/api_gateway/internal/gateway"
	"github.com/eighred/kanz/services/api_gateway/internal/middleware"
	"github.com/eighred/kanz/services/api_gateway/internal/orders"
	"github.com/eighred/kanz/services/api_gateway/internal/proxy"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type lakeContractClient struct {
	t *testing.T
}

func (c lakeContractClient) CreateSnapshot(_ context.Context, req *lakepb.CreateSnapshotRequest, _ ...grpc.CallOption) (*lakepb.CreateSnapshotResponse, error) {
	if req.GetInput().GetRequestId() != "snapshot-fixture-01" || len(req.GetInput().GetSelection().GetEventTypes()) != 1 || req.GetCaller().GetTenant() != "fixture-tenant" {
		c.t.Fatal("snapshot create did not receive the business input and authenticated caller")
	}
	return &lakepb.CreateSnapshotResponse{Snapshot: &lakepb.Snapshot{RowCount: 2}}, nil
}

func (c lakeContractClient) GetSnapshot(_ context.Context, req *lakepb.GetSnapshotRequest, _ ...grpc.CallOption) (*lakepb.GetSnapshotResponse, error) {
	if req.GetSnapshotId() != "fixture" {
		c.t.Fatal("snapshot path did not reach the reader")
	}
	return &lakepb.GetSnapshotResponse{Snapshot: &lakepb.Snapshot{
		ManifestSha256: make([]byte, 32), CreatedAt: &timestamppb.Timestamp{Seconds: 0, Nanos: 123456789},
	}}, nil
}

func (c lakeContractClient) QuerySnapshot(_ context.Context, req *lakepb.QuerySnapshotRequest, _ ...grpc.CallOption) (*lakepb.QuerySnapshotResponse, error) {
	query := req.GetQuery()
	if req.GetSnapshotId() != "fixture" || query == nil || query.PageSize != 1 || query.Operation != lakepb.SnapshotQueryOperation_SNAPSHOT_QUERY_OPERATION_LIST_EVENTS {
		c.t.Fatal("snapshot query did not retain the exact business cursor")
	}
	if query.AfterOrdinal != nil {
		if *query.AfterOrdinal != math.MaxUint64 {
			c.t.Fatal("gateway rounded the exact cursor")
		}
		return &lakepb.QuerySnapshotResponse{Result: &lakepb.SnapshotQueryResult{Exhausted: true}}, nil
	}
	return &lakepb.QuerySnapshotResponse{Result: &lakepb.SnapshotQueryResult{
		Events: []*lakepb.SnapshotEvent{{Ordinal: 0, EventId: "first", EventType: "fixture.event"}}, NextOrdinal: proto.Uint64(0),
	}}, nil
}

type lakeSchemaRef struct {
	Ref string `json:"$ref"`
}

type lakeMedia struct {
	Content map[string]struct{ Schema lakeSchemaRef }
}

type lakeDocument struct {
	Paths map[string]map[string]struct {
		Capability string `json:"x-kanz-capability"`
		Parameters []struct {
			Name, In, Description string
			Required              bool
		}
		RequestBody *lakeMedia
		Responses   map[string]lakeMedia
	}
	Components struct {
		Schemas map[string]struct {
			Type, Format         string
			Required             []string
			AdditionalProperties *bool
			Properties           map[string]struct {
				Type, Format, Pattern string
				Ref                   string `json:"$ref"`
			}
		}
	}
}

func TestLakeSnapshotOpenAPIMatchesConfiguredGateway(t *testing.T) {
	var publicDocument string
	for _, profile := range []struct {
		name          string
		lake, signing bool
	}{
		{"disabled", false, false}, {"disabled-signed", false, true}, {"enabled", true, false}, {"enabled-signed", true, true},
	} {
		t.Run(profile.name, func(t *testing.T) {
			enabled := profile.lake
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			h := gateway.New(nil, nil, nil, "", logger)
			cfg := config.Config{RequiredRole: "base", OperatorRole: "operator", TradeRole: "trader"}
			if profile.signing {
				cfg.SigningSecret = t.Name()
			}
			if enabled {
				cfg.LakeSnapshot = &config.LakeSnapshot{Tenant: "fixture-tenant", CreateRole: "creator", ReadRole: "reader"}
				if err := h.WithLakeSnapshots(lakeContractClient{t: t}, cfg.LakeSnapshot.Tenant); err != nil {
					t.Fatal(err)
				}
			}
			caller := stubAuthenticator{principal: &middleware.Principal{Subject: "fixture-subject", Tenant: "fixture-tenant", Roles: []string{"base", "creator", "reader"}}}
			router, err := buildRouter(cfg, h, orders.New(nil, "", halt.OpenGate(nil)), proxy.New(&recordingBackend{}, proxyRoles(cfg)), nil, testObservability(t), new(atomic.Bool), logger, nil, caller, nil)
			if err != nil {
				t.Fatal(err)
			}
			docResponse := httptest.NewRecorder()
			router.ServeHTTP(docResponse, httptest.NewRequest("GET", "/openapi.json", nil))
			var doc lakeDocument
			if docResponse.Code != 200 || json.Unmarshal(docResponse.Body.Bytes(), &doc) != nil {
				t.Fatal("gateway did not return the public OpenAPI document")
			}
			if publicDocument == "" {
				publicDocument = docResponse.Body.String()
			} else if docResponse.Body.String() != publicDocument {
				t.Fatal("public contract revealed snapshot configuration")
			}
			if strings.Contains(docResponse.Body.String(), "fixture-tenant") || strings.Contains(docResponse.Body.String(), "lake.v1.GatewayCaller") {
				t.Fatal("gateway document included deployment or delegation data")
			}
			for _, tc := range []struct {
				method, path, capability, input, output, body, field string
				value                                                any
			}{
				{"POST", "/lake/snapshots", "lake.snapshot.create", "CreateSnapshotInput", "CreateSnapshotResponse", `{"requestId":"snapshot-fixture-01","selection":{"eventTypes":["fixture.event"],"eventTimeFrom":"2026-01-01T00:00:00Z","eventTimeUntil":"2026-01-02T00:00:00Z"}}`, "row_count", "2"},
				{"GET", "/lake/snapshots/{id}", "lake.snapshot.read", "", "GetSnapshotResponse", "", "created_at", "1970-01-01T00:00:00.123456789Z"},
				{"POST", "/lake/snapshots/{id}/query", "lake.snapshot.read", "SnapshotQuery", "QuerySnapshotResponse", `{"after_ordinal":"18446744073709551615","page_size":1,"operation":"SNAPSHOT_QUERY_OPERATION_LIST_EVENTS"}`, "exhausted", true},
				{"POST", "/lake/snapshots/{id}/query", "lake.snapshot.read", "SnapshotQuery", "QuerySnapshotResponse", `{"operation":"SNAPSHOT_QUERY_OPERATION_LIST_EVENTS","page_size":1}`, "next_ordinal", "0"},
			} {
				op, documented := doc.Paths[tc.path][strings.ToLower(tc.method)]
				req := httptest.NewRequest(tc.method, "/v1"+strings.ReplaceAll(tc.path, "{id}", "fixture"), strings.NewReader(tc.body))
				req.Header.Set("Authorization", "Bearer fixture")
				headerFound := false
				for _, param := range op.Parameters {
					if param.In == "header" && param.Name == gatewaysig.Header && !param.Required && strings.Contains(param.Description, "when gateway request signing is enabled") {
						headerFound = true
					}
				}
				if !headerFound {
					t.Fatalf("%s: missing conditional signing header", tc.path)
				}
				if profile.signing {
					unsigned := httptest.NewRequest(tc.method, req.URL.Path, strings.NewReader(tc.body))
					unsigned.Header.Set("Authorization", "Bearer fixture")
					refused := httptest.NewRecorder()
					router.ServeHTTP(refused, unsigned)
					if refused.Code != http.StatusUnauthorized || !strings.Contains(refused.Body.String(), "missing request signature") {
						t.Fatalf("%s: missing signature did not produce the documented 401", tc.path)
					}
					gatewaysig.SignRequest(req, []byte(cfg.SigningSecret), []byte(tc.body))
				}
				result := httptest.NewRecorder()
				router.ServeHTTP(result, req)
				if !documented {
					t.Fatalf("%s %s: missing optional operation", tc.method, tc.path)
				}
				const prefix = "#/components/schemas/lake.v1."
				if op.Capability != tc.capability || op.Responses["200"].Content["application/json"].Schema.Ref != prefix+tc.output {
					t.Fatalf("%s: incorrect capability or response reference", tc.path)
				}
				if (op.RequestBody != nil) != (tc.input != "") || (op.RequestBody != nil && op.RequestBody.Content["application/json"].Schema.Ref != prefix+tc.input) {
					t.Fatalf("%s: incorrect external business input", tc.path)
				}
				if op.Responses["403"].Content["application/json"].Schema.Ref != "#/components/schemas/LakeError" {
					t.Fatalf("%s: missing capability refusal schema", tc.path)
				}
				if !enabled {
					if _, exists := op.Responses["404"].Content["text/plain"]; !exists || result.Code != http.StatusNotFound || !strings.HasPrefix(result.Header().Get("Content-Type"), "text/plain") {
						t.Fatalf("disabled %s returned %d", tc.path, result.Code)
					}
					continue
				}
				var payload map[string]map[string]any
				if result.Code != 200 || json.Unmarshal(result.Body.Bytes(), &payload) != nil {
					t.Fatalf("%s: expected successful JSON, received %d", tc.path, result.Code)
				}
				wrapper := "snapshot"
				if tc.output == "QuerySnapshotResponse" {
					wrapper = "result"
				}
				if doc.Components.Schemas["lake.v1."+tc.output].Properties[wrapper].Ref == "" {
					t.Fatalf("%s: missing typed %s response property", tc.path, wrapper)
				}
				if payload[wrapper][tc.field] != tc.value {
					t.Fatalf("%s: expected exact %s in served ProtoJSON", tc.path, tc.field)
				}
				if tc.method == "GET" && payload[wrapper]["manifest_sha256"] != "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=" {
					t.Fatal("snapshot digest was not base64")
				}
			}
			if enabled {
				props := doc.Components.Schemas["lake.v1.Snapshot"].Properties
				if props["row_count"].Type != "string" || props["row_count"].Pattern != "^[0-9]+$" || props["manifest_sha256"].Format != "byte" {
					t.Fatal("served schema disagrees with the exact snapshot count or digest")
				}
				timestamp := doc.Components.Schemas["google.protobuf.Timestamp"]
				if timestamp.Type != "string" || timestamp.Format != "date-time" {
					t.Fatal("snapshot timestamps require a date-time string")
				}
				for _, name := range []string{"lake.v1.CreateSnapshotInput", "lake.v1.SnapshotQuery"} {
					input, found := doc.Components.Schemas[name]
					if !found || input.AdditionalProperties == nil || *input.AdditionalProperties {
						t.Fatalf("%s must be a closed business input", name)
					}
					if _, found := input.Properties["caller"]; found {
						t.Fatalf("%s has a caller field", name)
					}
				}
				errSchema := doc.Components.Schemas["LakeError"]
				if len(errSchema.Required) != 1 || errSchema.Required[0] != "error" || errSchema.Properties["error"].Type != "string" {
					t.Fatal("refusals require a JSON object with an error string")
				}
				caller.principal.Roles = []string{"base"}
				req := httptest.NewRequest(http.MethodGet, "/v1/lake/snapshots/fixture", nil)
				req.Header.Set("Authorization", "Bearer fixture")
				refused := httptest.NewRecorder()
				gatewaysig.SignRequest(req, []byte(cfg.SigningSecret), nil)
				router.ServeHTTP(refused, req)
				var body struct{ Error string }
				if refused.Code != http.StatusForbidden || json.Unmarshal(refused.Body.Bytes(), &body) != nil || body.Error == "" {
					t.Fatal("documented read capability refusal did not return 403 JSON")
				}
			}
		})
	}
}
