package arch

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The bus already propagates TLS to writer, reader and metadata transports.
// The missing boundary was at its production callers: all three constructed
// plaintext Kafka clients despite the deployed listener requiring an SVID.
// Keep new service callers in scope and require a local mesh lifecycle.
// This is a syntactic wiring guard, not control-flow analysis or proof of a
// broker handshake, principal mapping, tenant ACLs, rotation or readiness.
func TestServiceKafkaClientsCarryWorkloadIdentity(t *testing.T) {
	root := moduleRoot(t)
	seen := map[string]int{}
	err := filepath.WalkDir(filepath.Join(root, "services"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		count, err := checkKafkaMeshWiring(file)
		if err != nil {
			t.Errorf("%s: %v", path, err)
		}
		if count > 0 {
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			seen[filepath.ToSlash(rel)] = count
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"services/archiver/cmd/archiver/main.go",
		"services/archiver/cmd/archiver-drain/main.go",
		"services/lake-sink/cmd/lake-sink/main.go",
	} {
		if seen[path] != 1 {
			t.Errorf("expected one protected Kafka dial in %s, found %d; review a moved or removed caller", path, seen[path])
		}
	}
}

func checkKafkaMeshWiring(file *ast.File) (int, error) {
	imports := map[string]string{}
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			return 0, err
		}
		name := filepath.Base(path)
		if imp.Name != nil {
			name = imp.Name.Name
		}
		if name == "." && (path == "github.com/eighred/kanz/pkg/bus" || path == "github.com/eighred/kanz/pkg/transport") {
			return 0, fmt.Errorf("dot-import hides identity wiring")
		}
		imports[name] = path
	}
	packageCall := func(expr ast.Expr, pkg, method string) bool {
		sel, ok := expr.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != method {
			return false
		}
		id, ok := sel.X.(*ast.Ident)
		return ok && imports[id.Name] == "github.com/eighred/kanz/pkg/"+pkg
	}
	count := 0
	var failures []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		meshes := map[string]token.Pos{}
		closed := map[string]bool{}
		hasKafka := false
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			if call, ok := node.(*ast.CallExpr); ok && packageCall(call.Fun, "bus", "DialKafka") {
				hasKafka = true
			}
			if assign, ok := node.(*ast.AssignStmt); ok && len(assign.Rhs) == 1 && len(assign.Lhs) == 2 {
				call, ok := assign.Rhs[0].(*ast.CallExpr)
				if ok && packageCall(call.Fun, "transport", "NewMesh") && len(call.Args) == 2 {
					// A literal empty socket cannot accidentally bless a production caller.
					if _, literal := call.Args[1].(*ast.BasicLit); !literal {
						if id, ok := assign.Lhs[0].(*ast.Ident); ok {
							meshes[id.Name] = assign.Pos()
						}
					}
				}
			}
			if deferStmt, ok := node.(*ast.DeferStmt); ok {
				ast.Inspect(deferStmt, func(node ast.Node) bool {
					call, ok := node.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if ok && sel.Sel.Name == "Close" {
						if id, ok := sel.X.(*ast.Ident); ok {
							closed[id.Name] = true
						}
					}
					return true
				})
			}
			return true
		})
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			configType, arg := "KafkaConfig", 0
			switch {
			case packageCall(call.Fun, "bus", "DialKafka"):
				count++
			case hasKafka && packageCall(call.Fun, "bus", "DialNATS"):
				// The drain's single-writer gate must authenticate to NATS too.
				configType, arg = "NATSConfig", 1
			default:
				return true
			}
			wired := false
			if len(call.Args) == arg+1 {
				if cfg, ok := call.Args[arg].(*ast.CompositeLit); ok && packageCall(cfg.Type, "bus", configType) {
					for _, element := range cfg.Elts {
						field, ok := element.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						key, ok := field.Key.(*ast.Ident)
						if !ok || key.Name != "TLSConfig" {
							continue
						}
						sel, ok := field.Value.(*ast.SelectorExpr)
						if ok && sel.Sel.Name == "Client" {
							if id, ok := sel.X.(*ast.Ident); ok {
								pos, found := meshes[id.Name]
								wired = found && pos < call.Pos() && closed[id.Name]
							}
						}
					}
				}
			}
			if !wired {
				failures = append(failures, fn.Name.Name+": "+configType+".TLSConfig must use Client from a preceding NewMesh with deferred Close")
			}
			return true
		})
	}
	if len(failures) > 0 {
		return count, fmt.Errorf("%s", strings.Join(failures, "; "))
	}
	return count, nil
}

func TestKafkaMeshWiringRejectsRegressions(t *testing.T) {
	const source = `package main
import b "github.com/eighred/kanz/pkg/bus"
import tr "github.com/eighred/kanz/pkg/transport"
func run() {
 mesh, err := tr.NewMesh(ctx, cfg.SPIFFESocket)
 if err != nil { return }
 defer mesh.Close()
 client, err := b.DialKafka(b.KafkaConfig{Brokers: brokers, TLSConfig: mesh.Client})
 gate, err := b.DialNATS(ctx, b.NATSConfig{URL: url, TLSConfig: mesh.Client})
 _ = gate
 _ = client; _ = err
}`
	cases := []struct{ name, old, replacement string }{
		{"omitted TLS", ", TLSConfig: mesh.Client", ""},
		{"nil TLS", "TLSConfig: mesh.Client", "TLSConfig: nil"},
		{"unrelated TLS", "TLSConfig: mesh.Client", "TLSConfig: other.Client"},
		{"no source", "mesh, err := tr.NewMesh(ctx, cfg.SPIFFESocket)", "mesh, err := disabledMesh()"},
		{"disabled source", "cfg.SPIFFESocket", `""`},
		{"no close", "defer mesh.Close()", ""},
		{"different close", "defer mesh.Close()", "defer other.Close()"},
		{"gate omits TLS", "URL: url, TLSConfig: mesh.Client", "URL: url"},
	}
	check := func(t *testing.T, source string) error {
		t.Helper()
		file, err := parser.ParseFile(token.NewFileSet(), "fixture.go", source, 0)
		if err != nil {
			t.Fatal(err)
		}
		count, err := checkKafkaMeshWiring(file)
		if count != 1 {
			t.Fatalf("found %d Kafka calls, want 1", count)
		}
		return err
	}
	if err := check(t, source); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := check(t, strings.Replace(source, tc.old, tc.replacement, 1)); err == nil {
				t.Fatal("mutation passed")
			}
		})
	}
}
