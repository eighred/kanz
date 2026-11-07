package gateway

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func servedRiskSchemas(t *testing.T) (map[string]any, map[string]any) {
	t.Helper()
	rr := httptest.NewRecorder()
	OpenAPIHandler()(rr, httptest.NewRequest(http.MethodGet, "/openapi.json", nil))
	var doc map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	return doc, doc["components"].(map[string]any)["schemas"].(map[string]any)
}

func TestRiskOpenAPIExactNumbersAndWellKnownTypes(t *testing.T) {
	_, schemas := servedRiskSchemas(t)
	decimal := schemas["common.v1.Decimal"].(map[string]any)["properties"].(map[string]any)
	if decimal["coefficient"].(map[string]any)["type"] != "string" || decimal["exponent"].(map[string]any)["type"] != "integer" {
		t.Fatal("Decimal must retain exact coefficient digits, with integer exponent")
	}
	position := schemas["common.v1.LogPosition"].(map[string]any)["properties"].(map[string]any)
	if position["offset"].(map[string]any)["type"] != "string" {
		t.Fatal("uint64 offset must not become a JavaScript number")
	}
	body, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(&commonpb.Decimal{Coefficient: math.MaxInt64, Exponent: -9})
	if err != nil || !strings.Contains(string(body), `"9223372036854775807"`) {
		t.Fatalf("actual Decimal encoding lost digits: %s %v", body, err)
	}
	if schemas["google.protobuf.Timestamp"].(map[string]any)["format"] != "date-time" || schemas["google.protobuf.Duration"].(map[string]any)["type"] != "string" {
		t.Fatal("well-known temporal types are JSON strings, not protobuf-shaped objects")
	}
	health, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(&querypb.HealthResponse{
		AsOf: timestamppb.New(time.Unix(1700000000, 123000000)), Staleness: durationpb.New(1500 * time.Millisecond),
	})
	if err != nil || !strings.Contains(string(health), `"1.500s"`) || !strings.Contains(string(health), `"2023-11-14T22:13:20.123Z"`) {
		t.Fatalf("actual temporal encoding differs: %s %v", health, err)
	}
}

func TestRiskOpenAPIEnumsOneofMapsAndCompleteReferences(t *testing.T) {
	doc, schemas := servedRiskSchemas(t)
	health := schemas["query.v1.HealthResponse"].(map[string]any)["properties"].(map[string]any)
	variants := health["mode"].(map[string]any)["oneOf"].([]any)
	if variants[1].(map[string]any)["type"] != "integer" {
		t.Fatal("unknown enum output must remain representable")
	}
	encoded, err := protojson.Marshal(&querypb.HealthResponse{Mode: querypb.Mode(12345)})
	if err != nil || !strings.Contains(string(encoded), "12345") {
		t.Fatalf("unknown enum encoding: %s %v", encoded, err)
	}
	shock := schemas["query.v1.ScenarioShock"].(map[string]any)
	if _, requiresOne := shock["required"]; requiresOne {
		t.Fatal("an absent protobuf oneof is legal")
	}
	constraints := shock["allOf"].([]any)
	if len(constraints) != 6 {
		t.Fatal("each pair of the four shock alternatives must be mutually exclusive")
	}
	seen := map[string]bool{}
	for _, value := range constraints {
		pair := value.(map[string]any)["not"].(map[string]any)["required"].([]any)
		seen[pair[0].(string)+"/"+pair[1].(string)] = true
	}
	for _, pair := range []string{"price/parallel_shift", "price/sector", "price/vol", "parallel_shift/sector", "parallel_shift/vol", "sector/vol"} {
		if !seen[pair] {
			t.Fatalf("oneof constraint missing: %s", pair)
		}
	}
	params := schemas["domain.v1.MeasureProvenance"].(map[string]any)["properties"].(map[string]any)["params"].(map[string]any)
	if params["type"] != "object" || params["additionalProperties"].(map[string]any)["type"] != "string" {
		t.Fatal("provenance map must retain typed string values")
	}
	var walk func(any)
	walk = func(value any) {
		switch value := value.(type) {
		case map[string]any:
			if ref, ok := value["$ref"].(string); ok {
				if _, exists := schemas[strings.TrimPrefix(ref, "#/components/schemas/")]; !exists {
					t.Fatalf("dangling schema reference %s", ref)
				}
			}
			if value["type"] == "object" && value["properties"] == nil && value["additionalProperties"] == nil {
				t.Fatal("opaque object in risk contract")
			}
			for _, child := range value {
				walk(child)
			}
		case []any:
			for _, child := range value {
				walk(child)
			}
		}
	}
	walk(doc)
	paths := doc["paths"].(map[string]any)
	if len(paths) != 5 || paths["/portfolios"] == nil {
		t.Fatal("all five existing risk read routes must be documented")
	}
	if !reflect.DeepEqual(riskOpenAPIDoc, buildRiskOpenAPI()) {
		t.Fatal("descriptor traversal must produce deterministic bytes")
	}
}
