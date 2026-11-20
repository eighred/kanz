package gateway

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/eighred/kanz/services/api_gateway/internal/authz"
)

//go:embed broker_reads.json
var brokerReadContract []byte

var gatewayOpenAPIDoc = withBrokerReads(riskOpenAPIDoc)

// withBrokerReads publishes the TV producer's registered reads under the gateway's
// existing /v1 server and bearer scheme. The gateway owns admission and forwarding.
func withBrokerReads(base []byte) []byte {
	var doc map[string]any
	var contract struct {
		Operations []struct {
			Path, Schema string
			Query        []string
		}
		Schemas map[string]json.RawMessage
	}
	if err := json.Unmarshal(base, &doc); err != nil {
		panic(err)
	}
	if err := json.Unmarshal(brokerReadContract, &contract); err != nil {
		panic(err)
	}
	info := doc["info"].(map[string]any)
	info["title"] = "Kanz Gateway API"
	info["description"] = strings.TrimSuffix(info["description"].(string), " This document covers risk reads, not the gateway's other service routes.") + " Broker account reads use tv-sync JSON with fixed-point decimal strings. Account access is scoped to the authenticated tenant."
	schemas := doc["components"].(map[string]any)["schemas"].(map[string]any)
	for name, s := range contract.Schemas {
		if _, exists := schemas[name]; exists {
			panic("duplicate broker schema: " + name)
		}
		schemas[name] = s
	}
	schemas["BrokerError"] = map[string]any{"type": "object", "required": []string{"error"}, "properties": map[string]any{"error": map[string]string{"type": "string"}}}
	paths := doc["paths"].(map[string]any)
	for _, op := range contract.Operations {
		if _, exists := paths[op.Path]; exists {
			panic("duplicate broker path: " + op.Path)
		}
		if _, exists := schemas[op.Schema]; !exists {
			panic("missing broker response schema: " + op.Schema)
		}
		responses := map[string]any{
			"200": brokerJSON("Tenant-scoped account projection", map[string]string{"$ref": "#/components/schemas/" + op.Schema}),
			"400": brokerError("Invalid request"),
			"401": brokerError("Authentication required"),
			"403": brokerError("Read permission required"),
			"413": brokerError("Request body exceeds the gateway limit"),
			"429": brokerError("Rate limit exceeded"),
			"502": brokerError("Broker upstream forwarding failed"),
		}
		// An optional upstream route may be absent. The default HTTP mux uses
		// text/plain; tv-sync's account-not-found refusal uses JSON.
		notFound := brokerError("Account or broker route unavailable")
		notFound["content"].(map[string]any)["text/plain"] = map[string]any{"schema": map[string]string{"type": "string"}}
		responses["404"] = notFound
		// The gateway uses an error object. The TV readiness gate uses {"status":"not ready"}.
		unavailable := map[string]any{"oneOf": []any{
			map[string]string{"$ref": "#/components/schemas/BrokerError"},
			map[string]any{"type": "object", "required": []string{"status"}, "properties": map[string]any{"status": map[string]any{"type": "string", "enum": []string{"not ready"}}}},
		}}
		responses["503"] = brokerJSON("Gateway or broker temporarily unavailable", unavailable)
		var params []map[string]any
		if strings.Contains(op.Path, "{id}") {
			params = append(params, map[string]any{"name": "id", "in": "path", "required": true, "schema": map[string]string{"type": "string"}})
			responses["500"] = brokerError("Account projection read failed")
		}
		for _, name := range op.Query {
			s := map[string]string{"type": "string"}
			var description string
			switch name {
			case "as_of":
				s["format"] = "date-time"
				description = "Knowledge time (RFC 3339). Omit for the current projection. Supply exactly one nonempty value. Repeated values, malformed encoding and the reserved instant 0001-01-01T00:00:00Z return 400."
				responses["410"] = brokerError("Knowledge time precedes the available retention horizon")
			case "instrument":
				description = "Filter executions by instrument."
			default:
				panic(fmt.Sprintf("undocumented query parameter %q for %s", name, op.Path))
			}
			params = append(params, map[string]any{"name": name, "in": "query", "description": description, "schema": s})
		}
		get := map[string]any{"summary": "Read " + op.Path[strings.LastIndex(op.Path, "/")+1:] + " for the authenticated tenant", "responses": responses, "x-kanz-capability": authz.Read}
		if len(params) > 0 {
			get["parameters"] = params
		}
		paths[op.Path] = map[string]any{"get": get}
	}
	b, err := json.Marshal(doc)
	if err != nil {
		panic(err)
	}
	return b
}

func brokerJSON(description string, schema any) map[string]any {
	return map[string]any{"description": description, "content": map[string]any{"application/json": map[string]any{"schema": schema}}}
}
func brokerError(description string) map[string]any {
	return brokerJSON(description, map[string]string{"$ref": "#/components/schemas/BrokerError"})
}
