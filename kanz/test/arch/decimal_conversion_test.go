package arch

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// dec.ToProto WRAPS when a value does not fit an int64 coefficient at the fixed
// scale — roughly $92bn in money terms. A wrapped coefficient is a fabricated
// number, and on a capital path the system then acts on it: a position valued
// at a wrapped figure, an average fill price that is not the price anything
// filled at.
//
// dec.ToProtoScaled preserves magnitude instead. This guard keeps the capital
// paths on it. ToProto is not banned outright — ~30 reporting and analytics
// callers use it and are unaffected by the ceiling — so the rule is scoped to
// the packages where a wrong number moves money.
var capitalPathPackages = []string{
	"services/oms/internal/position",
	"services/oms/internal/order",
}

// pendingErrorThreading are capital-path call sites that still use the wrapping
// conversion because their enclosing function has NO error return, so migrating
// them means threading an error through several signatures in code that
// currently cannot fail. That is a refactor with its own design question, not a
// find-and-replace.
//
// This map is the WORKLIST, not an excuse. A site here is a known gap with a
// written reason; a site NOT here and not migrated fails the build. Removing an
// entry is how the follow-up task reports progress.
var pendingErrorThreading = map[string]string{
	"services/oms/internal/position/book.go":     "Book.money and Book.stateOf return no error; threading one reaches Snapshot through three signatures",
	"services/oms/internal/position/postgres.go": "same shape as book.go — the Postgres-backed twin of the same read model",
}

func TestCapitalPathsDoNotUseWrappingToProto(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()
	var offenders []string
	scanned, seenPending := 0, map[string]bool{}

	for _, pkg := range capitalPathPackages {
		dir := filepath.Join(root, filepath.FromSlash(pkg))
		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			scanned++
			f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if perr != nil {
				return fmt.Errorf("parse %s: %w", path, perr)
			}
			rel := filepath.ToSlash(mustRel(root, path))
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "ToProto" {
					return true
				}
				id, ok := sel.X.(*ast.Ident)
				if !ok || id.Name != "dec" {
					return true
				}
				if _, pending := pendingErrorThreading[rel]; pending {
					seenPending[rel] = true
					return true
				}
				offenders = append(offenders, fmt.Sprintf("%s:%d", rel, fset.Position(call.Pos()).Line))
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}

	// NON-VACUITY: if the walk found no files, this guard would pass no matter
	// how many capital paths used the wrapping conversion.
	if scanned == 0 {
		t.Fatal("scanned zero Go files across the capital-path packages — the walk is broken")
	}

	// A declared exception that no longer has any dec.ToProto call is DEAD.
	// Removing it is how the follow-up task reports progress; leaving it lets a
	// future regression hide behind a stale entry.
	for file := range pendingErrorThreading {
		if !seenPending[file] {
			offenders = append(offenders, file+": declared in pendingErrorThreading but calls dec.ToProto nowhere — stale exemption, remove it")
		}
	}

	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("capital-path code calls the WRAPPING dec.ToProto:\n  %s\n\n"+
			"ToProto silently wraps above an int64 coefficient at the fixed scale (~$92bn in "+
			"money terms), and the system then acts on the fabricated number — a position "+
			"valued at a figure nothing is worth, an average price nothing filled at. Use "+
			"dec.ToProtoScaled, which preserves magnitude and reports when it cannot. If the "+
			"enclosing function has no error return, add the FILE to pendingErrorThreading "+
			"with the reason, rather than dropping the ok return.",
			strings.Join(offenders, "\n  "))
	}
}

func mustRel(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return rel
}
