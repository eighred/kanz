package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// A MANDATE PUBLISH IS A READ-MODIFY-WRITE, AND A SECOND PUBLISHER WOULD NOT
// KNOW THAT (#916).
//
// # What the stream cannot do for us
//
// The MANDATE stream keeps ONE message per (tenant, portfolio) subject and never
// ages it out (infra/nats/bootstrap-job.yaml). That message carries the mandate
// IN FORCE plus every mandate an operator has SCHEDULED — a set, because
// `effective_at` may be in the future and compaction keeps the NEWEST message
// rather than the one in effect. Publishing a future-dated mandate on its own
// therefore DELETED the version governing the portfolio, and the next replica to
// boot resolved it as UNGOVERNED: admitted with no constraints at all under
// OMS_REQUIRE_MANDATE=false, reported only by a counter that reads exactly like a
// portfolio nobody has written a mandate for yet.
//
// The repair is that internal/compliance.Publisher READS the subject it is about
// to write and merges into it. That is a property of one function, and the stream
// cannot enforce it: a second publisher — an admin route, a migration, a fixture
// that "just needs a mandate on the bus" — that formats the subject itself and
// publishes a single mandate would land a perfectly valid message that silently
// deletes the rest.
//
// # What this guard checks, and what it does not
//
// It is DEFAULT-DENY over the two symbols any such publisher must reach for:
// SubjectMandateFor (the per-portfolio subject) and EventTypeMandateChanged (the
// event type). Every non-test file naming either must be in the allow-list below,
// with why. Adding a publisher is then a deliberate edit to this list rather than
// something nobody notices.
//
// It works over the AST, NOT over the text, so it cannot be satisfied — or
// tripped — by a comment mentioning either name. Three guards in this directory
// have passed with the checked thing deleted because they grepped raw source and
// matched their own prose.
//
// It does NOT check that a publisher merges correctly. That is behaviour, and it
// is pinned where behaviour belongs: internal/compliance's
// TestAScheduledMandateDoesNotEvictTheOneInForce and
// TestAScheduledMandateSurvivesARestart, both against a real compacted stream.
// This guard's job is narrower and complementary — to make sure there is only one
// place those tests have to cover.
var mandateSubjectPublisherAllowed = map[string]string{
	"internal/compliance/mandate_codec.go": "defines both symbols.",
	"internal/compliance/publisher.go": "THE publisher. It reads the subject via MandateState, merges " +
		"the new mandate into the set already there, prunes with retainSelectable and writes the " +
		"result back conditioned on the sequence it read.",
	"cmd/kanz-mandate/main.go": "prints the subject an approval will publish to, so the operator can " +
		"see it before confirming. It does NOT construct the event — it calls compliance.Publisher, " +
		"which is the point.",
}

// mandatePublishSymbols are the two names a mandate publisher cannot avoid.
var mandatePublishSymbols = map[string]bool{
	"SubjectMandateFor":       true,
	"EventTypeMandateChanged": true,
}

func TestTheMandateSubjectHasExactlyOnePublisher(t *testing.T) {
	root := moduleRoot(t)
	found := map[string][]string{}

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".claude", ".gotmp", "gen", "node_modules", "vendor", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// Parsed WITHOUT comments on purpose: a doc comment naming SubjectMandateFor
		// is documentation, not a publish, and a guard that cannot tell them apart
		// fires on its own explanation.
		file, pErr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if pErr != nil {
			return nil
		}
		rel, rErr := filepath.Rel(root, path)
		if rErr != nil {
			return rErr
		}
		rel = filepath.ToSlash(rel)
		ast.Inspect(file, func(n ast.Node) bool {
			var name string
			switch v := n.(type) {
			case *ast.SelectorExpr:
				name = v.Sel.Name
			case *ast.Ident:
				name = v.Name
			default:
				return true
			}
			if mandatePublishSymbols[name] {
				found[rel] = append(found[rel], name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	// NON-VACUITY. A scan that finds nothing passes having checked nothing — the
	// failure mode of a guard whose walk root or symbol set quietly drifted.
	for _, must := range []string{"internal/compliance/publisher.go", "internal/compliance/mandate_codec.go"} {
		if len(found[must]) == 0 {
			t.Fatalf("the scan found no reference to the mandate publish symbols in %s. Those "+
				"symbols are defined and used there, so this guard is not looking where it "+
				"thinks it is — fix the scan, do not delete the check", must)
		}
	}

	var offenders []string
	for file := range found {
		if _, ok := mandateSubjectPublisherAllowed[file]; !ok {
			offenders = append(offenders, file+" ("+strings.Join(uniqueSorted(found[file]), ", ")+")")
		}
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("a SECOND place reaches for the mandate subject:\n  %s\n\n"+
			"The MANDATE stream retains ONE message per portfolio, forever, and that message carries "+
			"the mandate in force PLUS every mandate scheduled after it. A publisher that writes a "+
			"single mandate DELETES the rest — which is #916: scheduling a change left the portfolio "+
			"UNGOVERNED at the next restart, admitted unconstrained, with nothing reporting a loss.\n\n"+
			"Publish through internal/compliance.Publisher, which reads the subject and merges. If "+
			"this really is a new publisher, add it to mandateSubjectPublisherAllowed with why, and "+
			"make it merge.", strings.Join(offenders, "\n  "))
	}

	// DEAD ENTRIES. An allow-list outliving what it excused is how an exemption
	// becomes a permission nobody chose.
	var dead []string
	for file := range mandateSubjectPublisherAllowed {
		if len(found[file]) == 0 {
			dead = append(dead, file)
		}
	}
	if len(dead) > 0 {
		sort.Strings(dead)
		t.Fatalf("mandateSubjectPublisherAllowed names %v, and none of them reaches the mandate "+
			"subject any more. Remove the entries — an allow-list that outlives its subject "+
			"silently permits whatever moves into that path next", dead)
	}
}

// TestTheMandatePublishIsConditionedOnWhatItRead pins the second half of the
// read-modify-write, in the one function that performs it.
//
// Reading the subject and merging is worthless if the write is unconditional: two
// approvers merging concurrently each write a set built from what they read, the
// later write wins, and the loser's mandate disappears off a stream that never
// ages out. Exactly the #916 disappearance, from a different cause.
//
// It also pins that the value published is the SET encoding. Publisher.Publish
// calling MarshalMandateValue — the SINGLE-mandate encoder, which exists only for
// the approval digest — would be the original defect restored in one line, and it
// would still compile, still publish, and still look right.
func TestTheMandatePublishIsConditionedOnWhatItRead(t *testing.T) {
	path := filepath.Join(moduleRoot(t), "internal", "compliance", "publisher.go")
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	var publish *ast.FuncDecl
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == "Publish" && fn.Recv != nil {
			publish = fn
			break
		}
	}
	if publish == nil {
		t.Fatal("no method named Publish in internal/compliance/publisher.go — this guard reads " +
			"that function and has just stopped reading anything")
	}

	conditioned, setEncoded, singleEncoded := false, false, false
	ast.Inspect(publish, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.KeyValueExpr:
			if k, ok := v.Key.(*ast.Ident); ok && k.Name == "ExpectedLastSubjectSeq" {
				conditioned = true
			}
		case *ast.CallExpr:
			name := ""
			switch f := v.Fun.(type) {
			case *ast.Ident:
				name = f.Name
			case *ast.SelectorExpr:
				name = f.Sel.Name
			}
			switch name {
			case "MarshalMandateSetValue":
				setEncoded = true
			case "MarshalMandateValue":
				singleEncoded = true
			}
		}
		return true
	})

	if !conditioned {
		t.Error("Publisher.Publish emits a bus.Event with no ExpectedLastSubjectSeq. The set it " +
			"writes was merged into the value it READ, so an unconditional write silently drops " +
			"whatever another approver put on the subject in between — a mandate that vanishes off " +
			"a stream with no max-age (#916)")
	}
	if !setEncoded {
		t.Error("Publisher.Publish does not call MarshalMandateSetValue. The MANDATE stream keeps " +
			"one message per portfolio, so that message has to carry the whole selectable set")
	}
	if singleEncoded {
		t.Error("Publisher.Publish calls MarshalMandateValue — the SINGLE-mandate encoding, which " +
			"exists only so MandateDigest can hash the version two people actually approved. " +
			"Publishing it puts one mandate on a subject that retains one message: scheduling a " +
			"change would again delete the mandate in force (#916)")
	}
}

func uniqueSorted(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
