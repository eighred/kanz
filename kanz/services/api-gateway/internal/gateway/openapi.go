package gateway

import (
	"net/http"
)

// OpenAPIHandler serves the gateway's OpenAPI 3.0 description at /openapi.json
// (API-01c "OpenAPI emit"). The doc is a static, hand-maintained contract for
// the four read endpoints — emitted so clients/tooling get a machine-readable
// surface without the grpc-gateway codegen pipeline. Response schemas are
// described as opaque objects (the concrete shapes are the query.v1 protos,
// referenced by name) to keep the doc small and stable.
func OpenAPIHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(openAPIDoc))
	}
}

const openAPIDoc = `{
  "openapi": "3.0.3",
  "info": {
    "title": "Kanz Risk Query API",
    "version": "v1",
    "description": "REST surface over the risk-engine query.v1.RiskQueryService (API-01). Read-only: query, scenario, health — no mutation (state is event-driven)."
  },
  "servers": [{ "url": "/v1" }],
  "components": {
    "securitySchemes": {
      "bearerAuth": { "type": "http", "scheme": "bearer", "bearerFormat": "JWT" }
    },
    "schemas": {
      "ExposureResponse": { "type": "object", "description": "query.v1.ExposureResponse (protojson)" },
      "MeasuresResponse": { "type": "object", "description": "query.v1.MeasuresResponse (protojson)" },
      "EvaluateScenarioRequest": { "type": "object", "description": "query.v1.EvaluateScenarioRequest (protojson)" },
      "EvaluateScenarioResponse": { "type": "object", "description": "query.v1.EvaluateScenarioResponse (protojson)" },
      "HealthResponse": { "type": "object", "description": "query.v1.HealthResponse (protojson)" },
      "Error": { "type": "object", "properties": { "error": { "type": "string" } } }
    }
  },
  "security": [{ "bearerAuth": [] }],
  "paths": {
    "/portfolios/{id}/exposure": {
      "get": {
        "summary": "Latest exposure aggregate for a portfolio",
        "parameters": [
          { "name": "id", "in": "path", "required": true, "schema": { "type": "string" } },
          { "name": "as_of", "in": "query", "required": false, "schema": { "type": "string", "format": "date-time" } }
        ],
        "responses": {
          "200": { "description": "OK", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ExposureResponse" } } } },
          "401": { "description": "Unauthenticated", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/Error" } } } },
          "403": { "description": "Forbidden" },
          "404": { "description": "Portfolio not found" },
          "429": { "description": "Rate limit exceeded" }
        }
      }
    },
    "/portfolios/{id}/measures": {
      "get": {
        "summary": "Latest risk measures for a portfolio",
        "parameters": [
          { "name": "id", "in": "path", "required": true, "schema": { "type": "string" } },
          { "name": "as_of", "in": "query", "required": false, "schema": { "type": "string", "format": "date-time" } },
          { "name": "measure", "in": "query", "required": false, "schema": { "type": "array", "items": { "type": "string" } }, "description": "Repeatable; narrows the response to named measures." }
        ],
        "responses": {
          "200": { "description": "OK", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/MeasuresResponse" } } } },
          "404": { "description": "Portfolio not found" }
        }
      }
    },
    "/portfolios/{id}/scenario": {
      "post": {
        "summary": "Evaluate a what-if scenario against current state",
        "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }],
        "requestBody": { "required": true, "content": { "application/json": { "schema": { "$ref": "#/components/schemas/EvaluateScenarioRequest" } } } },
        "responses": {
          "200": { "description": "OK", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/EvaluateScenarioResponse" } } } },
          "400": { "description": "Malformed scenario" }
        }
      }
    },
    "/health": {
      "get": {
        "summary": "Engine operational mode + staleness",
        "responses": { "200": { "description": "OK", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/HealthResponse" } } } } }
      }
    }
  }
}`
