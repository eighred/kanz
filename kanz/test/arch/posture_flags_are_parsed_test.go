package arch

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// A DENY-BY-DEFAULT POSTURE IS PARSED, NEVER STRING-COMPARED (#783).
//
// Seven controls decided whether they were armed with os.Getenv(key) == "true".
// Every other spelling — "True", "TRUE", "1", "T", or a value a mounted secret
// file left whitespace on — evaluated to FALSE, with no error, no warning and no
// log line. The operator set the variable, read it back in the manifest, and
// deployed a control that was off.
//
// THE SPELLINGS ARE ORDINARY. "1" is what a shell writes for a boolean, "TRUE"
// is what a spreadsheet exports, and YAML's `value: |` keeps the newline. Two of
// the seven sat on the capital path: OMS_REQUIRE_MANDATE (an unmandated
// portfolio trades with no compliance rule evaluated) and OMS_REQUIRE_DUAL_CONTROL
// — where the misspelling was worse than a disarm, because arming that control
// without OMS_DUAL_CONTROL_MIN_NOTIONAL is a startup refusal, so "True" ALSO
// suppressed the complaint and the OMS came up with neither the control nor the
// alarm.
//
// internal/env.Bool is the repair and it predates the defect: its own doc names
// these exact variables and calls the behaviour "a maker-checker gate that
// reports itself armed and is not". services/datamaster already used it. Six
// other config packages did not.
//
// # What this checks
//
// A *_REQUIRE_* environment key tested for EQUALITY against a string literal,
// anywhere in non-test Go outside internal/env. That shape is the defect, and it
// is narrow on purpose: a direct os.Getenv is not itself wrong — forty files read
// the environment for keys with no default, and one_env_helper_test.go's header
// defends exactly that. What is forbidden is deciding a posture by comparing its
// text in the direction where a misread DISARMS the control. See the comment on
// the equality check below for why `!=` is a different case.
//
// # What it does not check
//
// Whether the DEFAULT is right, or whether a control should be armed at all.
// deny_by_default_is_visible_test.go asks that every such key is named in a
// manifest so an operator can see it exists; this one asks that the value they
// write is read the way they meant it. Neither can say whether arming it is
// correct — that is a judgement about the estate.
func TestEveryPostureFlagIsParsedNotStringCompared(t *testing.T) {
	root := moduleRoot(t)

	var offenders []string
	scanned := 0
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == "gen" || info.Name() == "testdata" || info.Name() == ".git" ||
				info.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(info.Name(), ".go") || strings.HasSuffix(info.Name(), "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		// internal/env is where the parsing lives; it necessarily handles raw text.
		if strings.HasPrefix(rel, "internal/env/") {
			return nil
		}
		f, perr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", rel, perr)
		}
		scanned++

		ast.Inspect(f, func(n ast.Node) bool {
			// EQUALITY ONLY, AND THE ASYMMETRY IS THE WHOLE RULE.
			//
			// `Getenv(k) == "true"` is armed only by one exact spelling, so every
			// misread DISARMS the control: it fails open, which is the defect.
			//
			// `Getenv(k) != "false"` is the mirror — a default-ON control that only
			// one exact spelling relaxes — so a misread keeps it ARMED. It fails
			// closed. webhook-ingest's WEBHOOK_INGEST_REQUIRE_SIGNAL_TS is written
			// that way on purpose and its field comment says so: "An unparseable
			// value keeps the STRICT default: a typo must not silently relax a
			// trading control."
			//
			// That is a real trade-off and not obviously the wrong one: reading it
			// through env.Bool would let an operator writing "FALSE" get what they
			// meant, and would turn a genuine typo into a refusal to start — which
			// for an ingest edge is a trading outage where today it is a rejected
			// sender. Changing it is a decision for whoever owns that control, so
			// this guard does not force it and does not pretend the shape is absent.
			bin, ok := n.(*ast.BinaryExpr)
			if !ok || bin.Op != token.EQL {
				return true
			}
			// One side must be a call reading a posture key, the other a literal.
			for _, pair := range [2][2]ast.Expr{{bin.X, bin.Y}, {bin.Y, bin.X}} {
				key, ok := postureEnvKey(pair[0])
				if !ok {
					continue
				}
				if _, isLit := stringLiteral(pair[1]); !isLit {
					continue
				}
				offenders = append(offenders, fmt.Sprintf(
					"%s compares %s against a string literal. Read it with env.Bool, which refuses "+
						"a value strconv.ParseBool cannot read instead of silently answering false — "+
						"a control an operator armed with \"1\" or \"TRUE\" is otherwise off, and the "+
						"manifest they would check says it is on (#783)", rel, key))
				return true
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// NON-VACUOUS BY DESIGN: a walk that parsed nothing reports PASS.
	if scanned == 0 {
		t.Fatal("no Go file was scanned — this guard verified NOTHING")
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("posture flags decided by string comparison (%d files scanned):\n  - %s",
			scanned, strings.Join(offenders, "\n  - "))
	}
}

// postureEnvKey reports the *_REQUIRE_* key an expression reads, if it is an
// environment read at all.
//
// It matches os.Getenv("X"), env.Or("X", ...) and env.Lookup("X") — the three
// ways this tree reads a variable as text. A key that does not contain REQUIRE
// is not a posture flag by this repository's naming, and is left alone: the
// *_ALLOW_* family fails in the SAFE direction (a spelling the parser does not
// recognise means the escape hatch does not open), so it is not what this guard
// is sized for.
func postureEnvKey(e ast.Expr) (string, bool) {
	call, ok := e.(*ast.CallExpr)
	if !ok || len(call.Args) == 0 {
		return "", false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return "", false
	}
	switch {
	case pkg.Name == "os" && sel.Sel.Name == "Getenv":
	case pkg.Name == "env" && (sel.Sel.Name == "Or" || sel.Sel.Name == "Lookup"):
	default:
		return "", false
	}
	key, ok := stringLiteral(call.Args[0])
	if !ok || !strings.Contains(key, "REQUIRE") {
		return "", false
	}
	return key, true
}

func stringLiteral(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return s, true
}
