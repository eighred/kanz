package arch

import (
	"bytes"
	"crypto/sha256"
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

// #1413 keeps the optional stream out of the default consume/DR sets. This
// reviewed configuration binding is only a source guard: real same-tenant
// archiver/retention/recovery evidence is still required before enablement.
func checkOptionalLakeArchiveSource(source string) error {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "config.go", source, 0)
	if err != nil {
		return err
	}
	var functions bytes.Buffer
	found := map[string]bool{}
	subject := false
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && (fn.Name.Name == "Load" || fn.Name.Name == "snapshotSubjects") {
			found[fn.Name.Name] = true
			if err := format.Node(&functions, fset, fn); err != nil {
				return err
			}
		}
		gd, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if name.Name == "LakeSnapshotSubject" && i < len(vs.Values) {
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if ok && lit.Kind == token.STRING {
						value, _ := strconv.Unquote(lit.Value)
						subject = value == "lake.snapshot.>"
					}
				}
				if name.Name != "DefaultSubjects" || i >= len(vs.Values) {
					continue
				}
				values, ok := vs.Values[i].(*ast.CompositeLit)
				if !ok {
					return fmt.Errorf("default archive set unresolved")
				}
				for _, value := range values.Elts {
					switch v := value.(type) {
					case *ast.Ident:
						return fmt.Errorf("default archive subject unresolved: %s", v.Name)
					case *ast.BasicLit:
						text, err := strconv.Unquote(v.Value)
						if err != nil || text == "lake.snapshot.>" {
							return fmt.Errorf("optional stream entered default archive set")
						}
					default:
						return fmt.Errorf("default archive subject unresolved")
					}
				}
			}
		}
	}
	if !subject || len(found) != 2 {
		return fmt.Errorf("optional archive owner missing")
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(functions.Bytes()))
	if digest != "063efabf1c6795681dbfb20b50c58d75aba2754e77745f0146a89a9914038362" {
		return fmt.Errorf("optional archive binding unreviewed: %s", digest)
	}
	return nil
}

func TestOptionalLakeArchiveRouteIsClosed(t *testing.T) {
	root := moduleRoot(t)
	path := filepath.Join(root, "services", "archiver", "internal", "config", "config.go")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	source := string(raw)
	if err := checkOptionalLakeArchiveSource(source); err != nil {
		t.Fatal(err)
	}
	if provisionedNonDLQTopics(t, root)["lake.snapshot"] != "delete" {
		t.Fatal("snapshot FACT topic must be provisioned without compaction")
	}
	reason := notArchivedByDesign["lake.snapshot"]
	if !strings.Contains(reason, "ARCHIVER_LAKE_SNAPSHOTS") || !strings.Contains(reason, "#1413/#60") {
		t.Fatal("optional default/DR scope must remain explicit")
	}
	for _, mutation := range []struct{ name, old, next string }{
		{"lost-binding", `snapshotSubjects(os.Getenv("ARCHIVER_LAKE_SNAPSHOTS"), cfg.Subjects)`, `snapshotSubjects("false", cfg.Subjects)`},
		{"implicit-enable", `case "", "false":`, `case "true":`},
		{"lost-enabled-route", `return append(slices.Clone(subjects), LakeSnapshotSubject), nil`, `return subjects, nil`},
		{"different-subject", `const LakeSnapshotSubject = "lake.snapshot.>"`, `const LakeSnapshotSubject = "lake.other.>"`},
		{"default-subscription", `"order.>",`, `"order.>", "lake.snapshot.>",`},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			changed := strings.Replace(source, mutation.old, mutation.next, 1)
			if changed == source {
				t.Fatal("mutation did not reach source")
			}
			if checkOptionalLakeArchiveSource(changed) == nil {
				t.Fatal("weakened optional archive binding passed")
			}
		})
	}
}
