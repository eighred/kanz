package arch

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// ONE SANITIZER FOR SUBJECT SYNTAX, AND IT MUST BE INJECTIVE (#999).
//
// `subject.Token` mapped `.`, `*`, `>` and ` ` all onto `_`. That is a many-to-one map,
// and the estate holds ids on both sides of it — `VOD.L` (a RIC) and `VOD_L` (the shape
// MBS_A / OPT_A / LIQUID_FAST use) became one token. The POSITION and MANDATE streams keep
// ONE message per subject forever, so two instruments on one subject means one holding's
// current position overwrites the other's, and DeliverLastPerSubject — the single read a
// booting consumer arms its whole book from — returns one and silence about the other.
// The compliance monitor's book is filled that way: a fund holding an instrument its
// mandate FORBIDS looks compliant, because the holding is not there. Nothing errors.
//
// The repair is an injective encoding in internal/platform/subject, proven by a round trip
// through Detoken and a fuzz target. THIS GUARD PROTECTS THE "ONE IMPLEMENTATION" HALF —
// the half a unit test cannot hold. A copied four-way replacer is how the fix stops
// spreading, and this codebase has paid that bill before: 17 services each had their own
// `secret()` and 15 were wrong.
//
// It is deliberately narrow. It does not try to recognise every conceivable hand-rolled
// sanitizer; it recognises the SHAPE THAT WAS ACTUALLY WRITTEN TWICE — a strings.Replace
// family call carrying a subject delimiter as a literal — because that is what a
// copy-paste of the removed code looks like, and a guard that tried to be exhaustive here
// would fire on unrelated string munging and be exempted into uselessness.
//
// SCOPE: production files only. walkGoFiles skips _test.go, so a sanitizer written inside a
// test is not caught — deliberately, because a test's own fixture is not a second
// implementation the estate publishes through. The thing this protects is the code that
// builds a subject a real consumer binds.

// subjectDelimiters are the bytes that are not ours to spend inside a subject token:
// `.` delimits tokens, `*` and `>` are wildcards, and a space is refused by the broker.
// A Replace call naming one of these is a call deciding subject syntax.
var subjectDelimiters = map[string]bool{".": true, "*": true, ">": true, " ": true}

// sanitizerExemption is one call site that keeps its own copy of the lossy map, and what
// PAYS FOR keeping it.
//
// The `guard` field is the half that is easy to leave out, and it is the load-bearing one. An exemption
// naming only an issue is a promise, and a promise is not a control: the issue can be
// closed by DECIDING to keep the copy — which is what happened here (#1011) — and the
// entry then reads as an outstanding repair forever while nothing checks that the
// compensating behaviour still exists. So an entry must name the test that holds the
// losses harmless, and TestSubjectSanitizerExemptionsNameALiveGuard fails if that test is
// not in the exempted file's own package.
type sanitizerExemption struct {
	// issue is the decision record: the issue that either retires this copy or explains
	// why it stays.
	issue string
	// guard is the test function, in the exempted file's package, that proves this copy's
	// many-to-one losses cannot cause harm. Deleting it fails the build.
	guard string
}

// oneSanitizerExemptions are the call sites that still hold their own copy. An entry is
// never a permanent carve-out on its own — TestSubjectSanitizerExemptionsAreAllLive fails
// when one stops being needed, and TestSubjectSanitizerExemptionsNameALiveGuard fails when
// the control that pays for it is gone.
var oneSanitizerExemptions = map[string]sanitizerExemption{
	// durableName maps a SUBJECT onto a JetStream consumer name, which may not contain
	// `.`, `*`, `>` or whitespace. Same lossy map, different blast radius: two subjects
	// that differ only in `.` vs `_` — or `>` vs `*` — become ONE durable, and
	// CreateOrUpdateConsumer then rewrites the first subscription's FilterSubject instead
	// of failing, so one feed goes dark while its consumer stays Ready.
	//
	// #1011 DECIDED THIS COPY STAYS, and the decision is in durableName's own doc comment:
	// subject.Token is legal in a consumer name, but adopting it renames every durable in
	// the estate, and a renamed durable is a new consumer starting at DeliverAll — a 24h
	// replay of order.> and a 168h replay of the ledger streams, against a 2-minute
	// in-process dedup window. The loss is refused at the point of use instead.
	"pkg/bus/nats.go": {issue: "#1011", guard: "TestDurableNameCollisionIsRefused"},
}

// The trees the estate's own code lives in. `.gotmp` (build artifacts) and `.claude` are
// outside all of them by construction.
var sanitizerRoots = []string{"internal", "services", "pkg", "cmd", "test/load", "tools"}

type sanitizerHit struct {
	file string
	line int
	fn   string
	call string
}

func TestOneSubjectSanitizer(t *testing.T) {
	hits := findSubjectSanitizers(t)

	var unexempt []sanitizerHit
	for _, h := range hits {
		if _, ok := oneSanitizerExemptions[h.file]; !ok {
			unexempt = append(unexempt, h)
		}
	}
	if len(unexempt) > 0 {
		var b strings.Builder
		for _, h := range unexempt {
			fmt.Fprintf(&b, "\n  %s:%d  %s  %s", h.file, h.line, h.fn, h.call)
		}
		t.Fatalf("%d call site(s) decide subject syntax outside internal/platform/subject:%s\n\n"+
			"A four-way replace of `.`, `*`, `>` and ` ` onto one byte is MANY-TO-ONE. On a "+
			"compacted stream that means two entities share one subject and one silently "+
			"overwrites the other's current state; on a durable name it means two subscriptions "+
			"share one consumer and one feed goes dark while staying Ready. Neither errors.\n\n"+
			"Use subject.Token, which is injective and proven so by a round trip through "+
			"subject.Detoken (#999). If this call site genuinely cannot — a JetStream consumer "+
			"name has its own charset — open an issue for it and add it to "+
			"oneSanitizerExemptions with that number.", len(unexempt), b.String())
	}
}

// THE EXEMPTION LIST MUST NOT OUTLIVE ITS REPAIR.
//
// Without this arm an exemption is permanent the moment the code it covers is fixed: the
// guard keeps passing, the entry keeps naming an issue nobody reopens, and the next
// reader takes the list as the set of known-bad sites rather than a stale one.
func TestSubjectSanitizerExemptionsAreAllLive(t *testing.T) {
	hits := findSubjectSanitizers(t)
	live := map[string]bool{}
	for _, h := range hits {
		live[h.file] = true
	}

	var dead []string
	for file, ex := range oneSanitizerExemptions {
		if !live[file] {
			dead = append(dead, fmt.Sprintf("%s (%s)", file, ex.issue))
		}
	}
	sort.Strings(dead)
	if len(dead) > 0 {
		t.Fatalf("oneSanitizerExemptions names %d call site(s) that no longer sanitize a subject: %s\n\n"+
			"The copy was removed and the carve-out was not. Delete the entry and close its issue.",
			len(dead), strings.Join(dead, ", "))
	}

	// NON-VACUITY. If the detector stops finding anything at all, every assertion above
	// holds trivially and the guard is decorative. It must keep seeing the one call site
	// the estate is known to have — and when that is repaired, the arm above is what
	// fails first and tells the next reader to delete this check with the entry.
	if len(hits) == 0 {
		t.Fatal("the detector found NO subject sanitizer anywhere, including the exempted one. " +
			"Either strings.NewReplacer moved out of pkg/bus/nats.go (delete its exemption and " +
			"this check) or the AST walk stopped reaching the estate — which would make " +
			"TestOneSubjectSanitizer pass over an unread tree.")
	}
}

// AN EXEMPTION MUST BE PAID FOR, NOT MERELY EXPLAINED.
//
// TestSubjectSanitizerExemptionsAreAllLive above catches an exemption that outlived its
// REPAIR. This catches the other direction, which is the one #1011 created: an exemption
// that outlived its COMPENSATING CONTROL. #1011 decided pkg/bus keeps the lossy map and
// refuses the collision instead, so from here on the entry is permanent — and a permanent
// entry with nothing checking the refusal still exists is how the hazard comes back with
// the carve-out still saying it is handled.
//
// The check is deliberately structural rather than textual: the named test must be a real
// `func Name(t *testing.T)` declaration in the exempted file's OWN package directory. A
// guard that matched prose would be satisfied by the comment above it, which this
// repository has been bitten by before.
func TestSubjectSanitizerExemptionsNameALiveGuard(t *testing.T) {
	root := moduleRoot(t)
	for file, ex := range oneSanitizerExemptions {
		if ex.guard == "" {
			t.Errorf("exemption for %s (%s) names no compensating guard: an exemption is a carve-out "+
				"with nothing checking it until one is named", file, ex.issue)
			continue
		}
		dir := filepath.Join(root, filepath.FromSlash(path.Dir(file)))
		if !testFuncExistsIn(t, dir, ex.guard) {
			t.Errorf("exemption for %s (%s) names %s as the control that makes its lossy sanitizer safe, "+
				"and no such test function exists in %s.\n\n"+
				"Either the control was deleted — in which case the copy is a live hazard again and the "+
				"exemption is now a lie — or it was renamed, in which case name it here. Do not delete "+
				"this check to make the build green.",
				file, ex.issue, ex.guard, path.Dir(file))
		}
	}
}

// testFuncExistsIn reports whether dir holds a `func name(t *testing.T)` declaration in any
// of its _test.go files. It parses rather than greps for the reason findSubjectSanitizers
// does.
func testFuncExistsIn(t *testing.T, dir, name string) bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, parser.SkipObjectResolution)
		if perr != nil {
			t.Fatalf("parse %s: %v", e.Name(), perr)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Name.Name != name {
				continue
			}
			if len(fn.Type.Params.List) != 1 {
				continue
			}
			star, ok := fn.Type.Params.List[0].Type.(*ast.StarExpr)
			if !ok {
				continue
			}
			sel, ok := star.X.(*ast.SelectorExpr)
			if !ok {
				continue
			}
			pkg, ok := sel.X.(*ast.Ident)
			if ok && pkg.Name == "testing" && sel.Sel.Name == "T" {
				return true
			}
		}
	}
	return false
}

// findSubjectSanitizers walks the estate's AST for strings.Replace-family calls that name
// a subject delimiter as a literal argument.
//
// AST, NOT GREP, and that is load-bearing twice over. A regex over raw source matches this
// guard's own prose and the doc comments of the very function it is checking — a guard
// that matches prose checks nothing. Parsing also means a delimiter mentioned in a comment
// beside an unrelated Replace call cannot produce a hit.
func findSubjectSanitizers(t *testing.T) []sanitizerHit {
	t.Helper()
	root := moduleRoot(t)
	fset := token.NewFileSet()
	var hits []sanitizerHit

	for _, sub := range sanitizerRoots {
		walkGoFiles(t, root, sub, fset, func(rel string, f *ast.File) {
			// The one implementation is allowed to be the one implementation.
			if strings.HasPrefix(rel, "internal/platform/subject/") {
				return
			}
			var fnStack []string
			ast.Inspect(f, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.FuncDecl:
					fnStack = append(fnStack, node.Name.Name)
				case *ast.CallExpr:
					sel, ok := node.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					pkg, ok := sel.X.(*ast.Ident)
					if !ok || pkg.Name != "strings" {
						return true
					}
					switch sel.Sel.Name {
					case "NewReplacer", "Replace", "ReplaceAll":
					default:
						return true
					}
					if !anyArgIsADelimiter(node.Args) {
						return true
					}
					fn := "(file scope)"
					if len(fnStack) > 0 {
						fn = fnStack[len(fnStack)-1]
					}
					hits = append(hits, sanitizerHit{
						file: rel,
						line: fset.Position(node.Pos()).Line,
						fn:   fn,
						call: "strings." + sel.Sel.Name,
					})
				}
				return true
			})
		})
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].file != hits[j].file {
			return hits[i].file < hits[j].file
		}
		return hits[i].line < hits[j].line
	})
	return hits
}

func anyArgIsADelimiter(args []ast.Expr) bool {
	for _, a := range args {
		lit, ok := a.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			continue
		}
		v, err := strconv.Unquote(lit.Value)
		if err != nil {
			continue
		}
		if subjectDelimiters[v] {
			return true
		}
	}
	return false
}
