package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
	"unicode"
)

// EVERY VERDICT THE COPILOT COMPUTES REACHES THE CALLER (#981).
//
// # What went wrong without it
//
// `agent.Answer` carried six fields and `/v1/ask` returned four. The two it
// dropped were the two that say an answer should not be trusted:
//
//   - `Ungrounded` — the numeric tokens the output review could not match to any
//     cited tool value. Without it `grounded: false` says a number in this answer
//     came from nowhere without saying WHICH, so a caller can only discard the
//     whole answer or ignore the flag. Ignoring it is the cheaper habit.
//   - `BudgetExhausted` — set when the tool-use loop hit maxTurns without the
//     model reaching a final answer. This one was worse: an exhausted loop returns
//     `Grounded: true` (vacuously, having asserted no numbers) and
//     `Refused: false`, so a caller reading the four fields that DID ship saw an
//     ordinary, trustworthy answer whose prose happened to say it failed.
//
// #971 separated "the model declined" from "the model never converged" on the
// grounds that they are different facts with different owners. Dropping the field
// at the HTTP boundary re-merged them one hop later, which is how a distinction
// bought deliberately is lost by omission rather than by decision.
//
// # Why the field list is DERIVED
//
// The defect is "somebody enumerated a set by hand and missed a member", so a
// guard that also enumerated it by hand would be a further copy of the thing that
// broke — it would have been written against the four fields that shipped. This
// reads the fields off `agent.Answer` itself and requires each to appear in the
// response projection, so a SEVENTH verdict added later cannot be silently
// dropped: it fails here on the day it is added.
//
// The only hand-written part is the rename table below, and it is deliberately
// the small half — a new field with no entry gets its snake_case name, which is
// the convention every other key already follows.

// askKeyRenames names the two response keys that deliberately differ from their
// Go field. Everything else must appear as its snake_case form.
//
// These are not verdicts, which is why they are allowed to be renamed: `Text` is
// the answer and `Citations` are its sources. A verdict arriving here would be a
// reason to look twice.
var askKeyRenames = map[string]string{
	"Text":      "answer",
	"Citations": "citations",
}

// copilotFile parses one non-test file under services/copilot.
func copilotFile(t *testing.T, rel string) (*token.FileSet, *ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(moduleRoot(t), filepath.FromSlash(rel)), nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v\n\nThis guard reads the copilot's answer surface directly. If the file "+
			"moved, move the guard with it rather than deleting it (#981).", rel, err)
	}
	return fset, f
}

// snakeKey converts a Go field name to the response key convention: BudgetExhausted
// becomes budget_exhausted.
func snakeKey(field string) string {
	var b strings.Builder
	for i, r := range field {
		if unicode.IsUpper(r) {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(unicode.ToLower(r))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// structFieldsOf returns the exported field names of the named struct type.
func structFieldsOf(f *ast.File, name string) []string {
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != name {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok || st.Fields == nil {
			return false
		}
		for _, fld := range st.Fields.List {
			for _, id := range fld.Names {
				if id.IsExported() {
					out = append(out, id.Name)
				}
			}
		}
		return false
	})
	return out
}

// askResponseKeys returns the string keys of the map literal passed to the
// writeJSON call that answers 200 in handleAsk.
func askResponseKeys(f *ast.File) map[string]bool {
	keys := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		fd, ok := n.(*ast.FuncDecl)
		if !ok || fd.Name.Name != "handleAsk" || fd.Body == nil {
			return true
		}
		ast.Inspect(fd.Body, func(m ast.Node) bool {
			lit, ok := m.(*ast.CompositeLit)
			if !ok {
				return true
			}
			// map[string]any{...}
			mt, ok := lit.Type.(*ast.MapType)
			if !ok {
				return true
			}
			if id, ok := mt.Key.(*ast.Ident); !ok || id.Name != "string" {
				return true
			}
			for _, el := range lit.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if k, ok := kv.Key.(*ast.BasicLit); ok && k.Kind == token.STRING {
					keys[strings.Trim(k.Value, `"`)] = true
				}
			}
			return true
		})
		return false
	})
	return keys
}

func TestACopilotAnswerCarriesEveryVerdictItComputes(t *testing.T) {
	_, agentFile := copilotFile(t, "services/copilot/internal/agent/agent.go")
	fields := structFieldsOf(agentFile, "Answer")

	// Non-vacuity, both halves. A rename or a refactor that made either read
	// empty would otherwise turn this guard into a no-op that still passes.
	const floor = 5
	if len(fields) < floor {
		t.Fatalf("agent.Answer has %d exported field(s), want at least %d — this guard derives its "+
			"expectations from that struct, and reading a near-empty one proves nothing (#981)",
			len(fields), floor)
	}

	fset, serverFile := copilotFile(t, "services/copilot/internal/server/server.go")
	keys := askResponseKeys(serverFile)
	if len(keys) < floor {
		t.Fatalf("handleAsk's 200 response has %d key(s), want at least %d — either the response is "+
			"no longer built as a map[string]any literal in that function, in which case this guard "+
			"now walks past it silently, or the surface has genuinely collapsed (#981)",
			len(keys), floor)
	}

	// THE RENAME TABLE IS THIS GUARD'S ESCAPE HATCH, so it is bounded twice.
	//
	// A dead entry means the table is describing a field that no longer exists,
	// which is how a rename table drifts into fiction — the same reasoning as the
	// dead-entry checks on the exemption maps elsewhere in this directory.
	for field := range askKeyRenames {
		found := false
		for _, f := range fields {
			if f == field {
				found = true
			}
		}
		if !found {
			t.Errorf("askKeyRenames names %q, which is not a field of agent.Answer. Remove the entry: "+
				"a rename table that describes fields that do not exist is one nobody can read to "+
				"find out which keys are deliberate (#981).", field)
		}
	}

	// TWO FIELDS CANNOT SHARE ONE KEY, and this arm exists because the mutation
	// that found it survived the first harness run: adding `"Ungrounded":
	// "citations"` to the table above made this guard credit the ungrounded
	// verdict to the citations key and pass with the verdict dropped. One key
	// carries one value, so a collision is always a silenced field rather than a
	// naming choice.
	claimed := map[string]string{}
	for _, field := range fields {
		want := askKeyRenames[field]
		if want == "" {
			want = snakeKey(field)
		}
		if prev, taken := claimed[want]; taken {
			t.Errorf("agent.Answer.%s and .%s both expect the response key %q. One key carries one "+
				"value, so this is not a rename — it is one verdict being credited to another "+
				"field's key, which is how this guard would pass with a verdict dropped (#981).",
				prev, field, want)
			continue
		}
		claimed[want] = field

		if keys[want] {
			continue
		}
		t.Errorf("%s: agent.Answer.%s never reaches the caller — /v1/ask's response carries no %q "+
			"key.\n\nThe grounding gate MARKS rather than withholds, so an answer the platform "+
			"distrusts is returned as prose either way and these fields are the ENTIRE protection. "+
			"A verdict the caller cannot read is a verdict nobody acts on: before #981 both "+
			"`Ungrounded` and `BudgetExhausted` were computed, recorded in the audit FACT, counted "+
			"by a metric that pages — and dropped at this one hop, so the operator reading the "+
			"answer saw nothing.\n\nAdd the key here, or if the field is genuinely internal add it "+
			"to askKeyRenames with a comment saying why it is not a verdict.",
			fset.Position(serverFile.Pos()), field, want)
	}
}
