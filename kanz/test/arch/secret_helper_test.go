package arch

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// secretPkg is the ONE place a secret may be resolved from. Everything else
// must call into it.
const secretPkg = "pkg/secret"

// ONE SECRET RESOLVER, AND NO LOCAL COPIES.
//
// Every service composition root plus cmd/kanz-migrate carried its own
// `func secret(k string) string`, and fifteen of the seventeen discarded
// os.ReadFile's error:
//
//	if p := os.Getenv(k + "_FILE"); p != "" {
//	    if b, err := os.ReadFile(p); err == nil {
//	        return strings.TrimSpace(string(b))
//	    }
//	}
//	return os.Getenv(k)          // ← reached when the mount is UNREADABLE
//
// Setting <k>_FILE is the deployment stating that a durable secret mount was
// intended. If that file is missing or unreadable, that is a DEPLOYMENT FAULT —
// but the fall-through answers it with the plaintext env and then with "", so a
// failed Vault CSI mount is indistinguishable from a secret nobody configured.
// Several services then treat "" as a legal value selecting an in-memory store,
// so the pod comes up, reports healthy, and loses everything on restart.
//
// THE POINT OF THIS GUARD IS THE COPIES, NOT THE BUG. venue-binance and
// venue-okx had the CORRECT implementation for weeks while fifteen others stayed
// wrong, because nothing connected them. A fix that lives in a copy does not
// spread; it just makes the next reader think the problem is solved. Deleting
// the copies is what makes the fix estate-wide, and this test is what stops
// them coming back.
//
// # WHY THIS READS THE AST, AFTER TWO YEARS OF GREPPING FOR IT (#661)
//
// This guard used to match raw source for two halves — the literal `"_FILE"`
// AND `os.ReadFile` in the same file — and that technique failed in BOTH
// directions at once.
//
// IT MISSED A REAL EIGHTEENTH COPY. services/operator's config carried
// `readTokenOr(fileEnv, valueEnv string)`, which took the key NAME AS A
// PARAMETER, so the literal `"_FILE"` appeared nowhere and the pattern could not
// match. It was resolving VAULT_TOKEN — the credential that unlocks every other
// secret — with exactly the error-discarding fall-through described above. A
// mis-mounted VAULT_TOKEN_FILE left the operator starting cleanly on whatever
// the plaintext var held, or on "". Nothing about that helper was unusual; the
// guard simply required a spelling it did not use.
//
// AND IT FIRED ON PROSE. The paragraph you are reading names both halves. Under
// the old technique that made this file, and its sibling scanner, fail — so they
// were skipped BY NAME through a hard-coded map. A guard that must be told to
// ignore its own explanation is one that cannot tell code from commentary, and
// three guards in this directory have now been caught passing while the thing
// they checked was deleted, for that reason. The skip list is gone: the parse
// below discards comments, so there is nothing in prose for it to match.
//
// WHAT IT MATCHES NOW IS THE SHAPE, NOT THE SPELLING: a value that comes from
// the environment and then names a file this process opens. That is what
// resolving a mount IS, and it is invariant under renaming the helper, taking
// the key as a parameter, or building it by any means at all.

// secretSinks are the calls that turn a path into bytes. A tainted value
// reaching any of them is this process reading a file the environment named.
var secretSinks = map[string]map[string]bool{
	"os":     {"ReadFile": true, "Open": true, "OpenFile": true},
	"ioutil": {"ReadFile": true},
}

func TestOnlyOnePlaceResolvesSecrets(t *testing.T) {
	root := moduleRoot(t)

	files := goFilesUnder(t, root)
	if len(files) == 0 {
		t.Fatalf("found zero Go files under %s — the scanner is broken, not the estate", root)
	}

	var openCoded, localHelpers, inTheOnePlace []string
	for _, f := range files {
		fset := token.NewFileSet()
		// NO parser.ParseComments. The doc above quotes the whole pattern; a
		// guard that reads its own explanation as evidence is the defect #661
		// filed, not a detector of it.
		parsed, err := parser.ParseFile(fset, f.rel, f.body, 0)
		if err != nil {
			continue // generated or build-tagged file this walk cannot parse
		}
		own := strings.HasPrefix(f.rel, secretPkg+"/") || f.rel == secretPkg

		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			// A local `func secret(...)` is the obvious reintroduction — someone
			// adds a service by copying a composition root. Resolved as a
			// declaration, so the word in a comment is not a match.
			if !own && fn.Name.Name == "secret" && fn.Recv == nil {
				localHelpers = append(localHelpers,
					fmt.Sprintf("%s:%d", f.rel, fset.Position(fn.Pos()).Line))
			}
			for _, line := range resolvesAMountedPath(fset, fn) {
				where := fmt.Sprintf("%s:%d %s", f.rel, line, fn.Name.Name)
				if own {
					inTheOnePlace = append(inTheOnePlace, where)
					continue
				}
				openCoded = append(openCoded, where)
			}
		}
	}

	// NON-VACUITY, AND A STRONGER ONE THAN COUNTING FILES. pkg/secret.Read is a
	// KNOWN instance of the shape being hunted — it is the legitimate one. If the
	// analysis cannot find it, the analysis is broken, and every empty result
	// below would be a false all-clear. This is the arm that would have caught
	// the old technique's blind spot, had it been asked the question.
	if len(inTheOnePlace) == 0 {
		t.Fatalf("the environment-to-file-read analysis found nothing in %s, which is where the "+
			"one legitimate resolver lives. It cannot detect the shape it exists to detect, so "+
			"the clean result for the rest of the estate proves nothing", secretPkg)
	}

	sort.Strings(localHelpers)
	for _, f := range localHelpers {
		t.Errorf("%s declares its own func secret()\n\n"+
			"There is one resolver, %s.Read, and it returns an error when a declared "+
			"<K>_FILE mount is unreadable. A local copy is how fifteen roots kept the "+
			"error-discarding version while two had the fix — a fix in a copy does not "+
			"spread, it just stops the next reader looking. Call secret.Read instead.",
			f, secretPkg)
	}

	sort.Strings(openCoded)
	for _, f := range openCoded {
		t.Errorf("%s reads a file whose path came from the environment — it is resolving a "+
			"secret mount by hand\n\n"+
			"That is the same defect as a local secret() under a different name, and it does "+
			"not matter whether the key is a literal, a parameter or assembled: this matched "+
			"the SHAPE. services/operator's readTokenOr took the key as a parameter and went "+
			"unseen for months while resolving VAULT_TOKEN (#661).\n\n"+
			"Setting a <K>_FILE env var is fine anywhere (tests do it to drive Load); READING "+
			"the path it names is what belongs in one place. If a genuine second resolution "+
			"strategy is needed, it goes in %s beside Read, where its error handling is "+
			"reviewed once.", f, secretPkg)
	}
}

// resolvesAMountedPath reports the lines in fn where a value obtained from the
// environment reaches a file read.
//
// Taint, not pattern. The seed is os.Getenv — however its argument is spelled —
// and it propagates through any assignment that mentions an already-tainted
// name, so filepath.Join(dir, p), strings.TrimSpace(p) and a plain copy all
// carry it. That propagation is what makes the check survive the renaming and
// re-wrapping that defeated the literal match.
//
// Deliberately intra-procedural. A helper that returns an env value and a caller
// that opens it would slip through, and closing that needs type information this
// package does not load. Stated rather than left to be discovered: the shape
// this catches is the one that has actually shipped eighteen times.
func resolvesAMountedPath(fset *token.FileSet, fn *ast.FuncDecl) []int {
	tainted := map[string]bool{}

	// Seed and propagate to a fixpoint. Assignments can precede or follow one
	// another in any order inside nested blocks, so a single pass would miss
	// chains that read backwards in source order.
	for changed := true; changed; {
		changed = false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			var lhs []ast.Expr
			var rhs []ast.Expr
			switch s := n.(type) {
			case *ast.AssignStmt:
				lhs, rhs = s.Lhs, s.Rhs
			case *ast.ValueSpec:
				for _, name := range s.Names {
					lhs = append(lhs, name)
				}
				rhs = s.Values
			default:
				return true
			}
			if len(rhs) == 0 {
				return true
			}
			// A multi-value RHS (one call, several results) taints every name it
			// binds: os.ReadFile's own (b, err) is the shape, and which half
			// carries the path is not decidable here.
			single := len(lhs) == len(rhs)
			for i, name := range lhs {
				id, ok := name.(*ast.Ident)
				if !ok || id.Name == "_" || tainted[id.Name] {
					continue
				}
				src := rhs
				if single {
					src = rhs[i : i+1]
				}
				for _, e := range src {
					if fromEnvironment(e, tainted) {
						tainted[id.Name] = true
						changed = true
						break
					}
				}
			}
			return true
		})
	}

	var lines []int
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !isSink(call.Fun) {
			return true
		}
		for _, arg := range call.Args {
			if fromEnvironment(arg, tainted) {
				lines = append(lines, fset.Position(call.Pos()).Line)
				break
			}
		}
		return true
	})
	sort.Ints(lines)
	return lines
}

// fromEnvironment reports whether e is os.Getenv(...), a name already tainted,
// or any expression containing one. os.ReadFile(os.Getenv(k)) has no
// intermediate variable to taint, so the nested call has to count directly.
func fromEnvironment(e ast.Expr, tainted map[string]bool) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.Ident:
			if tainted[v.Name] {
				found = true
			}
		case *ast.CallExpr:
			if pkg, fn, ok := packageCall(v.Fun); ok && pkg == "os" &&
				(fn == "Getenv" || fn == "LookupEnv") {
				found = true
			}
		}
		return !found
	})
	return found
}

func isSink(fun ast.Expr) bool {
	pkg, name, ok := packageCall(fun)
	if !ok {
		return false
	}
	return secretSinks[pkg][name]
}

// packageCall resolves a pkg.Fn callee. A method on a local value is not one,
// which is what keeps f.ReadFile() from reading as os.ReadFile().
func packageCall(fun ast.Expr) (string, string, bool) {
	s, ok := fun.(*ast.SelectorExpr)
	if !ok {
		return "", "", false
	}
	id, ok := s.X.(*ast.Ident)
	if !ok {
		return "", "", false
	}
	return id.Name, s.Sel.Name, true
}

// goFile is one Go source file: where it is, and what is in it.
type goFile struct {
	rel  string // module-relative, forward slashes
	body string
}

func goFilesUnder(t *testing.T, root string) []goFile {
	t.Helper()

	var out []goFile
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".claude", "vendor", "node_modules", ".gotmp":
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		out = append(out, goFile{rel: filepath.ToSlash(rel), body: string(b)})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}
