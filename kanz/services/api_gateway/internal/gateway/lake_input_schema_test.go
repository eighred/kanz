package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

func TestLakeOpenAPIValidatesBusinessInputs(t *testing.T) {
	rec := httptest.NewRecorder()
	OpenAPIHandler()(rec, httptest.NewRequest(http.MethodGet, "/openapi.json", nil))
	var doc struct {
		Components struct{ Schemas json.RawMessage }
		Paths      map[string]map[string]struct {
			RequestBody struct {
				Content map[string]struct {
					Schema struct {
						Ref string `json:"$ref"`
					}
				}
			}
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	// OpenAPI stores local schemas under components. Give the same schema
	// objects to the JSON Schema validator under its standard dictionary.
	raw := bytes.ReplaceAll(doc.Components.Schemas, []byte("#/components/schemas/"), []byte("#/$defs/"))
	var defs map[string]*jsonschema.Schema
	if err := json.Unmarshal(raw, &defs); err != nil {
		t.Fatal(err)
	}
	resolve := func(path string) *jsonschema.Resolved {
		t.Helper()
		ref := doc.Paths[path]["post"].RequestBody.Content["application/json"].Schema.Ref
		component, found := strings.CutPrefix(ref, "#/components/schemas/")
		if !found {
			t.Fatalf("missing operation request schema for %s", path)
		}
		root := jsonschema.Schema{Defs: defs, Ref: "#/$defs/" + component}
		resolved, err := root.Resolve(nil)
		if err != nil {
			t.Fatal(err)
		}
		return resolved
	}
	create := resolve("/lake/snapshots")
	query := resolve("/lake/snapshots/{id}/query")
	const selection = `{"eventTypes":["fixture.event"],"eventTimeFrom":"2026-01-01T00:00:00Z","eventTimeUntil":"2026-01-02T00:00:00Z"}`
	const createBody = `{"requestId":"snapshot-fixture-01","selection":` + selection + `}`
	eventTypes := make([]string, 65)
	for i := range eventTypes {
		eventTypes[i] = "fixture.event." + strconv.Itoa(i)
	}
	tooManyTypes, err := json.Marshal(eventTypes)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		schema *jsonschema.Resolved
		body   string
		valid  bool
	}{
		{"CLI create", create, createBody, true},
		{"mixed field names", create, strings.Replace(createBody, "eventTimeUntil", "event_time_until", 1), true},
		{"no create input", create, "{}", false},
		{"missing selection", create, `{"requestId":"snapshot-fixture-01"}`, false},
		{"invalid request identifier", create, strings.Replace(createBody, "snapshot-fixture-01", "short", 1), false},
		{"missing interval end", create, strings.Replace(createBody, `,"eventTimeUntil":"2026-01-02T00:00:00Z"`, "", 1), false},
		{"duplicate event types", create, strings.Replace(createBody, `["fixture.event"]`, `["fixture.event","fixture.event"]`, 1), false},
		{"both request id spellings", create, strings.Replace(createBody, `"requestId"`, `"request_id":"snapshot-fixture-02","requestId"`, 1), false},
		{"65 event types", create, strings.Replace(createBody, `["fixture.event"]`, string(tooManyTypes), 1), false},
		{"untrimmed event type", create, strings.Replace(createBody, "fixture.event", " fixture.event", 1), false},
		{"empty event types", create, strings.Replace(createBody, `["fixture.event"]`, "[]", 1), false},
		{"canonical list cursor", query, `{"operation":"SNAPSHOT_QUERY_OPERATION_LIST_EVENTS","page_size":1000,"after_ordinal":"18446744073709551615"}`, true},
		{"CLI cursor zero", query, `{"operation":"SNAPSHOT_QUERY_OPERATION_DECODE_FAILURES","pageSize":1,"afterOrdinal":"0"}`, true},
		{"numeric count", query, `{"operation":2,"pageSize":1}`, true},
		{"quoted page size", query, `{"operation":1,"page_size":"1000"}`, true},
		{"empty query", query, "{}", false},
		{"missing size", query, `{"operation":1}`, false},
		{"zero size", query, `{"operation":1,"page_size":0}`, false},
		{"size 1001", query, `{"operation":1,"pageSize":1001}`, false},
		{"quoted size 1001", query, `{"operation":1,"page_size":"1001"}`, false},
		{"both size spellings", query, `{"operation":1,"pageSize":1,"page_size":1}`, false},
		{"count with zero cursor", query, `{"operation":"SNAPSHOT_QUERY_OPERATION_COUNT_BY_EVENT_TYPE","pageSize":1,"afterOrdinal":"0"}`, false},
		{"numeric count with cursor", query, `{"operation":2,"page_size":1,"after_ordinal":0}`, false},
		{"unsupported operation", query, `{"operation":99,"page_size":1}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var instance any
			if err := json.Unmarshal([]byte(tc.body), &instance); err != nil {
				t.Fatal(err)
			}
			if err := tc.schema.Validate(instance); (err == nil) != tc.valid {
				t.Fatalf("want valid=%t, got %v", tc.valid, err)
			}
		})
	}
}
