package gateway

import (
	"encoding/json"
	"strings"

	lakepb "github.com/eighred/kanz/kanz-schemas-go/lake/v1"
	"github.com/eighred/kanz/services/api_gateway/internal/authz"
	"google.golang.org/protobuf/proto"
)

var lakeOpenAPIDoc = buildLakeOpenAPI()

func buildLakeOpenAPI() []byte {
	var doc map[string]any
	if err := json.Unmarshal(riskOpenAPIDoc, &doc); err != nil {
		panic(err)
	}
	info := doc["info"].(map[string]any)
	info["title"] = "Kanz Risk Query and Lake Snapshot API"
	info["description"] = "Risk queries, transient scenarios and optional finite lake snapshots. Lake routes return 404 if the snapshot service is unconfigured. Schemas describe the preferred snake_case ProtoJSON encoding. Request parsers also accept protobuf JSON aliases and alternate numeric input forms. Each lake operation requires its gateway capability and a tenant-wide principal in the configured snapshot tenant."
	paths := doc["paths"].(map[string]any)
	schemas := doc["components"].(map[string]any)["schemas"].(map[string]any)
	schemas["LakeError"] = map[string]any{
		"type": "object", "required": []string{"error"},
		"properties": map[string]any{
			"error": map[string]any{"type": "string"},
			"code":  map[string]any{"type": "string", "description": "Present when recent MFA is required.", "enum": []string{"mfa_required"}},
		},
	}
	for _, route := range []struct {
		path, method, summary string
		capability            authz.Capability
		input, output         proto.Message
	}{
		{"/lake/snapshots", "post", "Create a finite snapshot for a request ID, unique event types and a half-open event-time interval", authz.LakeSnapshotCreate, &lakepb.CreateSnapshotInput{}, &lakepb.CreateSnapshotResponse{}},
		{"/lake/snapshots/{id}", "get", "Get a finite snapshot by its tenant-scoped ID", authz.LakeSnapshotRead, nil, &lakepb.GetSnapshotResponse{}},
		{"/lake/snapshots/{id}/query", "post", "Query a ready snapshot using a bounded page of events, decode failures or counts by event type", authz.LakeSnapshotRead, &lakepb.SnapshotQuery{}, &lakepb.QuerySnapshotResponse{}},
	} {
		result := protoSchema(route.output.ProtoReflect().Descriptor(), schemas)
		refusal := map[string]any{"$ref": "#/components/schemas/LakeError"}
		notFound := lakeJSON(refusal)
		// An enabled handler conceals inaccessible resources with a JSON 404.
		// An unmounted path receives ServeMux's ordinary text/plain 404.
		notFound["text/plain"] = map[string]any{"schema": map[string]any{"type": "string"}}
		op := map[string]any{
			"summary": route.summary, "x-kanz-capability": route.capability,
			"responses": map[string]any{
				"200":     map[string]any{"description": "OK", "content": lakeJSON(result)},
				"400":     map[string]any{"description": "Invalid input or snapshot precondition", "content": lakeJSON(refusal)},
				"401":     map[string]any{"description": "Authentication required", "content": lakeJSON(refusal)},
				"403":     map[string]any{"description": "Insufficient authority or recent MFA required", "content": lakeJSON(refusal)},
				"404":     map[string]any{"description": "Snapshot service unconfigured or snapshot unavailable to this caller", "content": notFound},
				"502":     map[string]any{"description": "Snapshot response unavailable", "content": lakeJSON(refusal)},
				"default": map[string]any{"description": "Gateway or snapshot owner refusal", "content": lakeJSON(refusal)},
			},
		}
		if route.input != nil {
			input := protoSchema(route.input.ProtoReflect().Descriptor(), schemas)
			op["requestBody"] = map[string]any{"required": true, "content": lakeJSON(input)}
		}
		if strings.Contains(route.path, "{id}") {
			op["parameters"] = []any{map[string]any{
				"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"},
			}}
		}
		paths[route.path] = map[string]any{route.method: op}
	}
	body, err := json.Marshal(doc)
	if err != nil {
		panic(err)
	}
	return body
}

func lakeJSON(schema map[string]any) map[string]any {
	return map[string]any{"application/json": map[string]any{"schema": schema}}
}
