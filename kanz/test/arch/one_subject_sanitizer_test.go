package arch

import (
	"fmt"
	"go/ast"
	"go/token"
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

// oneSanitizerExemptions are the call sites that still hold their own copy, each with the
// issue that retires it. An entry is a promise with an owner, not a permanent carve-out —
// TestSubjectSanitizerExemptionsAreAllLive fails when one stops being needed.
var oneSanitizerExemptions = map[string]string{
	// durableName maps a SUBJECT onto a JetStream consumer name, which may not contain
	// `.`, `*`, `>` or whitespace. Same lossy map, different blast radius: two subjects
	// that differ only in `.` vs `_` become ONE durable, and CreateOrUpdateConsumer then
	// rewrites the first subscription's FilterSubject instead of failing — one feed goes
	// dark while its consumer stays Ready. Measured at the time of #999: zero collisions
	// among the estate's 290 subject literals and every subscribe subject is a constant,
	// so it is latent rather than live — and repairing it renames every durable in the
	// estate, which is a redelivery migration and not a refactor.
	"pkg/bus/nats.go": "#1011",
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
	for file, issue := range oneSanitizerExemptions {
		if !live[file] {
			dead = append(dead, fmt.Sprintf("%s (%s)", file, issue))
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
