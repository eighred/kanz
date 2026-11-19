// Command broker_contract generates the browser's read contract from the TV projection.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"log"
	"os"
	"reflect"
	"strings"

	"github.com/eighred/kanz/services/tv_sync/internal/projection"
)

func render() ([]byte, error) {
	var out bytes.Buffer
	out.WriteString("// Code generated from tv_sync projection DTOs by go generate. DO NOT EDIT.\n")
	for _, dto := range []any{projection.AccountDTO{}, projection.StateDTO{}, projection.PositionDTO{}, projection.OrderDTO{}, projection.ExecutionDTO{}} {
		t := reflect.TypeOf(dto)
		fmt.Fprintf(&out, "\nexport const broker%s = {\n", strings.TrimSuffix(t.Name(), "DTO"))
		for _, field := range reflect.VisibleFields(t) {
			name, option, _ := strings.Cut(field.Tag.Get("json"), ",")
			if !field.IsExported() || name == "" || name == "-" || option != "" && option != "omitempty" {
				return nil, fmt.Errorf("unsupported JSON field %s.%s", t, field.Name)
			}
			kind := field.Tag.Get("broker")
			switch {
			case field.Type.Kind() == reflect.String && kind == "":
				kind = "text"
			case field.Type.Kind() == reflect.String && (kind == "decimal" || kind == "timestamp"):
			case field.Type.Kind() == reflect.Int && kind == "count" && option == "":
			default:
				return nil, fmt.Errorf("unsupported wire type for %s.%s", t, field.Name)
			}
			if option == "omitempty" {
				kind += "?"
			}
			fmt.Fprintf(&out, "  %q: '%s',\n", name, kind)
		}
		out.WriteString("} as const\n")
	}
	return out.Bytes(), nil
}

func main() {
	path := flag.String("out", "", "path to the generated TypeScript contract")
	flag.Parse()
	if *path == "" {
		log.Fatal("-out is required")
	}
	data, err := render()
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(*path, data, 0644); err != nil {
		log.Fatal(err)
	}
}
