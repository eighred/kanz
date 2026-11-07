package arch

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The legacy mixed-tenant landing handler is not a fold into the new optional
// tenant pool. Recognize its actual file-only composition, not a service-wide
// exemption. This is a closed syntax/dependency check, not arbitrary dataflow.
func lakeLandingSources(t *testing.T, service string) map[string]string {
	t.Helper()
	files := map[string]string{}
	for _, dir := range []string{"internal/cdc", "internal/sink", "internal/decode", "cmd/lake-sink"} {
		err := filepath.WalkDir(filepath.Join(service, dir), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			wire, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(service, path)
			if err != nil {
				return err
			}
			files[filepath.ToSlash(rel)] = string(wire)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return files
}
func lakeNodeText(n ast.Node) string {
	var b bytes.Buffer
	if format.Node(&b, token.NewFileSet(), n) != nil {
		return ""
	}
	return b.String()
}
func verifyMixedLakeFileLanding(files map[string]string) error {
	allowed := map[string]bool{}
	for _, name := range strings.Fields(`bufio context crypto/rand crypto/sha256 encoding/hex encoding/json errors fmt io log/slog net/http os path/filepath sort strings sync time
 github.com/eighred/kanz/kanz-schemas-go/envelope/v1 github.com/eighred/kanz/pkg/bus
 github.com/eighred/kanz/services/lake-sink/internal/decode github.com/eighred/kanz/services/lake-sink/internal/sink
 github.com/prometheus/client_golang/prometheus golang.org/x/sys/unix
 google.golang.org/protobuf/encoding/protojson google.golang.org/protobuf/proto google.golang.org/protobuf/reflect/protodesc google.golang.org/protobuf/reflect/protoreflect google.golang.org/protobuf/types/descriptorpb google.golang.org/protobuf/types/dynamicpb`) {
		allowed[name] = true
	}
	calls := map[string]int{}
	tenantRows := 0
	rowLiterals := 0
	invalidBinding := false
	receiver := false
	constructor := false
	for path, source := range files {
		file, err := parser.ParseFile(token.NewFileSet(), path, source, 0)
		if err != nil {
			return err
		}
		if !strings.HasPrefix(path, "cmd/") {
			for _, imp := range file.Imports {
				name, err := strconv.Unquote(imp.Path.Value)
				if err != nil || !allowed[name] {
					return fmt.Errorf("mixed landing dependency requires review: %s", path)
				}
			}
		}
		for _, declaration := range file.Decls {
			fn, ok := declaration.(*ast.FuncDecl)
			if !ok || fn.Name.Name != "NewEventSink" {
				continue
			}
			uses, bindings, literals, returns := 0, 0, 0, 0
			ast.Inspect(fn, func(n ast.Node) bool {
				if _, ok := n.(*ast.ReturnStmt); ok {
					returns++
				}
				if id, ok := n.(*ast.Ident); ok && id.Name == "s" {
					uses++
				}
				if literal, ok := n.(*ast.CompositeLit); ok && lakeNodeText(literal.Type) == "EventSink" {
					literals++
					for _, field := range literal.Elts {
						if kv, ok := field.(*ast.KeyValueExpr); ok && lakeNodeText(kv.Key) == "sink" && lakeNodeText(kv.Value) == "s" {
							bindings++
						}
					}
				}
				return true
			})
			// Exactly the parameter declaration and its initializer use. Writes,
			// shadowing, address escape or other substitution refuse this form.
			returnsBinding := false
			if fn.Body != nil && len(fn.Body.List) > 0 {
				if ret, ok := fn.Body.List[len(fn.Body.List)-1].(*ast.ReturnStmt); ok && len(ret.Results) == 1 {
					if address, ok := ret.Results[0].(*ast.UnaryExpr); ok && address.Op == token.AND {
						if literal, ok := address.X.(*ast.CompositeLit); ok && lakeNodeText(literal.Type) == "EventSink" {
							returnsBinding = true
						}
					}
				}
			}
			constructor = fn.Recv == nil && uses == 2 && bindings == 1 && literals == 1 && returns == 1 && returnsBinding
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if assignment, ok := n.(*ast.AssignStmt); ok {
				for _, lhs := range assignment.Lhs {
					if lakeNodeText(lhs) == "fileSink" && (len(assignment.Rhs) != 1 || lakeNodeText(assignment.Rhs[0]) != `sink.NewFileSink(cfg.OutputDir, "", fileOptions...)`) {
						invalidBinding = true
					}
					if path == "internal/cdc/sink.go" && lakeNodeText(lhs) == "row" {
						if len(assignment.Rhs) != 1 {
							invalidBinding = true
						} else if literal, ok := assignment.Rhs[0].(*ast.CompositeLit); !ok || lakeNodeText(literal.Type) != "sink.Row" {
							invalidBinding = true
						}
					}
					if lakeNodeText(lhs) == "e.sink" || lakeNodeText(lhs) == "row.TenantID" || lakeNodeText(lhs) == "env.TenantId" {
						invalidBinding = true
					}
				}
			}

			if call, ok := n.(*ast.CallExpr); ok {
				calls[lakeNodeText(call)]++
			}
			if row, ok := n.(*ast.CompositeLit); ok && lakeNodeText(row.Type) == "sink.Row" {
				rowLiterals++
				for _, field := range row.Elts {
					if kv, ok := field.(*ast.KeyValueExpr); ok && lakeNodeText(kv.Key) == "TenantID" && lakeNodeText(kv.Value) == "env.GetTenantId()" {
						tenantRows++
					}
				}
			}
			if spec, ok := n.(*ast.TypeSpec); ok && spec.Name.Name == "EventSink" {
				st, ok := spec.Type.(*ast.StructType)
				if !ok {
					return true
				}
				want := map[string]string{"decoder": "*decode.Decoder", "sink": "sink.Sink", "now": "func() time.Time", "logger": "*slog.Logger", "metrics": "*Metrics"}
				got := map[string]string{}
				for _, field := range st.Fields.List {
					for _, name := range field.Names {
						got[name.Name] = lakeNodeText(field.Type)
					}
				}
				receiver = len(got) == len(want)
				for name, typ := range want {
					receiver = receiver && got[name] == typ
				}
			}
			return true
		})
	}
	if !receiver || !constructor || tenantRows != 1 || rowLiterals != 1 || invalidBinding {
		return fmt.Errorf("mixed landing attribution/receiver changed")
	}
	for _, call := range []string{`sink.NewFileSink(cfg.OutputDir, "", fileOptions...)`, `runSink(ctx, cfg, fileSink, readiness, logger, obs)`, `cdc.NewEventSink(decode.NewDecoder(resolver), fileSink, time.Now, logger, metrics)`, `e.sink.Write(ctx, row)`, `e.sink.Flush()`} {
		if calls[call] != 1 {
			return fmt.Errorf("mixed landing file binding changed: %s", call)
		}
	}
	return nil
}
func TestMixedLakeLandingPreservesFileCustodyBoundary(t *testing.T) {
	files := lakeLandingSources(t, filepath.Join(moduleRoot(t), "services/lake-sink"))
	if err := verifyMixedLakeFileLanding(files); err != nil {
		t.Fatal(err)
	}
	for _, m := range []struct{ path, old, next string }{
		{"internal/cdc/sink.go", `"context"`, `"github.com/jackc/pgx/v5"`},
		{"internal/cdc/sink.go", "env.GetTenantId()", "configuredTenant"},
		{"internal/cdc/sink.go", "return &EventSink{", "s = nil; return &EventSink{"},
		{"internal/cdc/sink.go", "sink: s", "sink: other"},
		{"internal/cdc/sink.go", "if err := e.sink.Write", `row.TenantID = "foreign"; if err := e.sink.Write`},
		{"internal/cdc/sink.go", "if err := e.sink.Write", `row = other; if err := e.sink.Write`},
		{"cmd/lake-sink/main.go", `sink.NewFileSink(cfg.OutputDir, "", fileOptions...)`, `other.NewStore(cfg.OutputDir)`},
		{"cmd/lake-sink/main.go", `cdc.NewEventSink(decode.NewDecoder(resolver), fileSink, time.Now, logger, metrics)`, `cdc.NewEventSink(decode.NewDecoder(resolver), tenantStore, time.Now, logger, metrics)`},
		{"internal/cdc/sink.go", "e.sink.Write(ctx, row)", "tenantStore.Write(ctx, row)"},
		{"cmd/lake-sink/main.go", "eventSink := cdc.NewEventSink", "fileSink = tenantStore; eventSink := cdc.NewEventSink"},
	} {
		cloned := map[string]string{}
		for k, v := range files {
			cloned[k] = v
		}
		cloned[m.path] = strings.Replace(cloned[m.path], m.old, m.next, 1)
		if cloned[m.path] == files[m.path] || verifyMixedLakeFileLanding(cloned) == nil {
			t.Fatalf("mixed landing mutation escaped: %s", m.old)
		}
	}
}
