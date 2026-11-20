// Command broker_contract derives browser decoders and gateway discovery from the registered broker reads.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"reflect"
	"strings"

	"github.com/eighred/kanz/services/tv_sync/internal/brokerapi"
)

type schema = map[string]any

type operation struct {
	Path   string   `json:"path"`
	Query  []string `json:"query,omitempty"`
	Schema string   `json:"schema"`
}
type contract struct {
	Operations []operation       `json:"operations"`
	Schemas    map[string]schema `json:"schemas"`
}

func schemaName(t reflect.Type) string { return "Broker" + strings.TrimSuffix(t.Name(), "DTO") }

func scalar(field reflect.StructField) (string, schema, error) {
	kind := field.Tag.Get("broker")
	switch {
	case field.Type.Kind() == reflect.String && kind == "":
		return "text", schema{"type": "string"}, nil
	case field.Type.Kind() == reflect.String && kind == "decimal":
		return kind, schema{"type": "string", "pattern": `^-?[0-9]+(\.[0-9]+)?$`}, nil
	case field.Type.Kind() == reflect.String && (kind == "timestamp" || kind == "horizon"):
		return "timestamp", schema{
			"type": "string", "format": "date-time",
			"pattern": `^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,9})?(Z|[+-][0-9]{2}:[0-9]{2})$`,
		}, nil
	case field.Type.Kind() == reflect.Int && kind == "count" && !strings.Contains(field.Tag.Get("json"), ","):
		return kind, schema{"type": "integer", "minimum": 0}, nil
	default:
		return "", nil, fmt.Errorf("unsupported broker field: %s %s (%q)", field.Name, field.Type, kind)
	}
}

type wireObject struct {
	schema              schema
	row                 reflect.Type
	listKey, horizonKey string
	tsFields            string
}

func describe(t reflect.Type) (wireObject, error) {
	var obj wireObject
	var fields bytes.Buffer
	properties := schema{}
	var required []string
	for _, field := range reflect.VisibleFields(t) {
		key, option, _ := strings.Cut(field.Tag.Get("json"), ",")
		if !field.IsExported() || key == "" || key == "-" || option != "" && option != "omitempty" {
			return obj, fmt.Errorf("unsupported JSON field %s.%s", t, field.Name)
		}
		if option == "" {
			required = append(required, key)
		}
		if field.Type.Kind() == reflect.Slice {
			if obj.row != nil || field.Type.Elem().Kind() != reflect.Struct || option != "" {
				return obj, fmt.Errorf("unsupported broker collection %s.%s", t, field.Name)
			}
			obj.row = field.Type.Elem()
			obj.listKey = key
			properties[key] = schema{"type": "array", "nullable": true, "items": schema{"$ref": "#/components/schemas/" + schemaName(obj.row)}}
			continue
		}
		kind, s, err := scalar(field)
		if err != nil {
			return obj, err
		}
		if field.Tag.Get("broker") == "horizon" {
			if obj.horizonKey != "" || option != "omitempty" {
				return obj, fmt.Errorf("invalid retention horizon %s.%s", t, field.Name)
			}
			obj.horizonKey = key
			s["description"] = "Inclusive start of the resident history, omitted until retention has dropped history."
		}
		properties[key] = s
		if option == "omitempty" {
			kind += "?"
		}
		fmt.Fprintf(&fields, "  %q: '%s',\n", key, kind)
	}
	if obj.horizonKey != "" && obj.row == nil {
		return obj, fmt.Errorf("retention horizon on non-list response %s", t)
	}
	if obj.row != nil && (len(properties) > 2 || (len(properties) == 2 && obj.horizonKey == "")) {
		return obj, fmt.Errorf("unsupported broker list envelope %s", t)
	}
	obj.schema = schema{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
	obj.tsFields = fields.String()
	return obj, nil
}

func render() ([]byte, []byte, error) {
	var rows, routes bytes.Buffer
	rows.WriteString("// Code generated from tv_sync account reads by go generate. DO NOT EDIT.\n")
	routes.WriteString("\nexport const brokerReads = {\n")
	doc := contract{Schemas: map[string]schema{}}
	for _, op := range brokerapi.AccountReads() {
		t := reflect.TypeOf(op.Response)
		obj, err := describe(t)
		if err != nil {
			return nil, nil, err
		}
		name := schemaName(t)
		doc.Schemas[name] = obj.schema
		doc.Operations = append(doc.Operations, operation{op.Path, op.Query, name})
		rowType, row := t, obj
		if obj.row != nil {
			rowType = obj.row
			row, err = describe(rowType)
			if err != nil {
				return nil, nil, err
			}
			doc.Schemas[schemaName(rowType)] = row.schema
		}
		varName := "broker" + strings.TrimPrefix(schemaName(rowType), "Broker")
		fmt.Fprintf(&rows, "\nexport const %s = {\n%s} as const\n", varName, row.tsFields)
		fmt.Fprintf(&routes, "  %s: { path: %q, fields: %s", op.Name, op.Path, varName)
		if len(op.Query) > 0 {
			query, err := json.Marshal(op.Query)
			if err != nil {
				return nil, nil, err
			}
			fmt.Fprintf(&routes, ", query: %s", query)
		}
		if obj.listKey != "" {
			fmt.Fprintf(&routes, ", list: %q", obj.listKey)
		}
		if obj.horizonKey != "" {
			fmt.Fprintf(&routes, ", horizon: %q", obj.horizonKey)
		}
		routes.WriteString(" },\n")
	}
	routes.WriteString("} as const\n")
	rows.Write(routes.Bytes())
	gateway, err := json.MarshalIndent(doc, "", "  ")
	return rows.Bytes(), append(gateway, '\n'), err
}

func main() {
	ts := flag.String("out", "", "generated TypeScript decoder contract")
	gateway := flag.String("gateway", "", "generated gateway discovery input")
	flag.Parse()
	if *ts == "" || *gateway == "" {
		log.Fatal("-out and -gateway are required")
	}
	rows, doc, err := render()
	if err != nil {
		log.Fatal(err)
	}
	if err = os.WriteFile(*ts, rows, 0644); err != nil {
		log.Fatal(err)
	}
	if err = os.WriteFile(*gateway, doc, 0644); err != nil {
		log.Fatal(err)
	}
}
