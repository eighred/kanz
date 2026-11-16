package gateway

import (
	"encoding/json"
	"fmt"
	"strings"

	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

var riskOpenAPIDoc = buildRiskOpenAPI()

// These describe the preferred snake_case ProtoJSON encoding used by write.
// They do not replace decodeJSON or financial/authorization admission checks.
// ProtoJSON also accepts aliases and alternate numeric input encodings:
// https://protobuf.dev/programming-guides/json/
func buildRiskOpenAPI() []byte {
	var doc map[string]any
	if err := json.Unmarshal([]byte(openAPIDoc), &doc); err != nil {
		panic(err)
	}
	schemas := doc["components"].(map[string]any)["schemas"].(map[string]any)
	doc["info"].(map[string]any)["description"] = "Read-only risk queries and transient scenarios. Schemas describe preferred snake_case ProtoJSON; parsers also accept protobuf JSON aliases and numeric input forms. Endpoint authorization and financial validation remain mandatory. This document covers risk reads, not the gateway's other service routes."
	for name, message := range map[string]proto.Message{
		"ExposureResponse":         &querypb.ExposureResponse{},
		"MeasuresResponse":         &querypb.MeasuresResponse{},
		"EvaluateScenarioRequest":  &querypb.EvaluateScenarioRequest{},
		"EvaluateScenarioResponse": &querypb.EvaluateScenarioResponse{},
		"HealthResponse":           &querypb.HealthResponse{},
		"ListPortfoliosResponse":   &querypb.ListPortfoliosResponse{},
	} {
		schemas[name] = protoSchema(message.ProtoReflect().Descriptor(), schemas)
	}
	doc["paths"].(map[string]any)["/portfolios"] = map[string]any{"get": map[string]any{
		"summary": "Portfolios visible to the caller, filtered by tenant and portfolio scope",
		"responses": map[string]any{"200": map[string]any{"description": "OK", "content": map[string]any{
			"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/ListPortfoliosResponse"}},
		}}},
	}}
	body, err := json.Marshal(doc)
	if err != nil {
		panic(err)
	}
	return body
}

func protoSchema(message protoreflect.MessageDescriptor, schemas map[string]any) map[string]any {
	name := string(message.FullName())
	ref := map[string]any{"$ref": "#/components/schemas/" + name}
	if _, exists := schemas[name]; exists {
		return ref
	}
	// Register before traversing: recursive protobufs must produce references.
	schema := map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}
	schemas[name] = schema
	switch message.FullName() {
	case "google.protobuf.Timestamp":
		schemas[name] = map[string]any{"type": "string", "format": "date-time"}
		return ref
	case "google.protobuf.Duration":
		schemas[name] = map[string]any{"type": "string", "pattern": `^-?[0-9]+(\.[0-9]{1,9})?s$`}
		return ref
	}
	if strings.HasPrefix(name, "google.protobuf.") {
		panic(fmt.Sprintf("risk OpenAPI needs an explicit ProtoJSON mapping for %s", name))
	}
	properties := schema["properties"].(map[string]any)
	for i := 0; i < message.Fields().Len(); i++ {
		field := message.Fields().Get(i)
		var value map[string]any
		if field.IsMap() {
			value = map[string]any{"type": "object", "additionalProperties": protoScalarSchema(field.MapValue(), schemas)}
		} else {
			value = protoScalarSchema(field, schemas)
			if field.IsList() {
				value = map[string]any{"type": "array", "items": value}
			}
		}
		properties[string(field.Name())] = value
	}
	// A oneof may be absent, but two alternatives cannot coexist. Requiring one
	// would incorrectly reject the protobuf default (and proto3 optional fields).
	var constraints []any
	for i := 0; i < message.Oneofs().Len(); i++ {
		fields := message.Oneofs().Get(i).Fields()
		for a := 0; a < fields.Len(); a++ {
			for b := a + 1; b < fields.Len(); b++ {
				constraints = append(constraints, map[string]any{"not": map[string]any{
					"required": []string{string(fields.Get(a).Name()), string(fields.Get(b).Name())},
				}})
			}
		}
	}
	if len(constraints) != 0 {
		schema["allOf"] = constraints
	}
	return ref
}

func protoScalarSchema(field protoreflect.FieldDescriptor, schemas map[string]any) map[string]any {
	switch field.Kind() {
	case protoreflect.MessageKind:
		return protoSchema(field.Message(), schemas)
	case protoreflect.BoolKind:
		return map[string]any{"type": "boolean"}
	case protoreflect.StringKind:
		return map[string]any{"type": "string"}
	case protoreflect.BytesKind:
		return map[string]any{"type": "string", "format": "byte"}
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return map[string]any{"type": "integer", "format": "int32", "minimum": -2147483648, "maximum": 2147483647}
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return map[string]any{"type": "integer", "format": "int64", "minimum": 0, "maximum": uint64(4294967295)}
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return map[string]any{"type": "string", "pattern": `^-?[0-9]+$`, "description": "Signed 64-bit integer encoded as a decimal string; retain exact digits."}
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return map[string]any{"type": "string", "pattern": `^[0-9]+$`, "description": "Unsigned 64-bit integer encoded as a decimal string; retain exact digits."}
	case protoreflect.EnumKind:
		var names []string
		for i := 0; i < field.Enum().Values().Len(); i++ {
			names = append(names, string(field.Enum().Values().Get(i).Name()))
		}
		// Unknown enum values are emitted as integers by protojson.
		return map[string]any{"oneOf": []any{map[string]any{"type": "string", "enum": names}, map[string]any{"type": "integer", "format": "int32"}}}
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		return map[string]any{"oneOf": []any{map[string]any{"type": "number"}, map[string]any{"type": "string", "enum": []string{"NaN", "Infinity", "-Infinity"}}}}
	default:
		panic(fmt.Sprintf("unsupported risk OpenAPI protobuf kind: %s", field.Kind()))
	}
}
