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

// A COMPOSITION ROOT MAY NOT KEEP GROWING (#643).
//
// # What the size actually costs, which is not readability
//
// Everything inside a `func` in `package main` is unreachable by any test: the
// constructions exist only as locals. services/oms/cmd/oms's runConsumers reached
// 1,474 lines and 61 constructors, and the two seams it left nil sat as bare
// `nil` arguments on ONE line in the middle of it. One was the instrument
// classifier, which made every sector and issuer mandate limit pass silently for
// months (#640). Six lines below them, the same call wired a different seam with
// a paragraph explaining why a nil seam is unacceptable — the author was looking
// at the nils while writing it.
//
// The estate's compensating control is the ~52 guards in this directory that
// parse cmd/ as source. That strategy is deliberate and it works, for properties
// somebody thought to encode. It structurally cannot cover "this seam should not
// have been nil", because a nil argument is syntactically identical to a
// deliberate one. What it CAN do is make the place those defects hide stop
// getting bigger.
//
// # A RATCHET, NOT AN EXEMPTION LIST
//
// The two roots over the cap carry a BUDGET: the line count they had when this
// guard landed. They may shrink freely and may not grow by a single line. That is
// the difference between a guard that stops the accretion today and a list that
// records permission to continue — #643's own follow-up measured the OMS root
// growing 1,228 → 1,655 lines while the issue sat open, and every one of those
// additions was individually reasonable.
//
// Three arms, and the last two are what keep the list honest:
//
//   - a root over the cap with no budget → FAIL, naming it;
//   - a budgeted root above its budget → FAIL, it grew;
//   - a budgeted root at or under the CAP → FAIL as a stale entry, delete it;
//   - a budgeted root more than budgetSlack under its budget → FAIL, re-baseline,
//     so an extraction tightens the ratchet in the same commit that earns it.
//
// # SCOPE
//
// Functions in `package main` under services/*/cmd/**. Not kanz/cmd/* — those
// are operator CLIs whose length is an ergonomics question, not an untestability
// one, and widening this guard to them without evidence would be a rule looking
// for violations.
const (
	// maxCompositionRootLines is what an unbudgeted composition-root function may
	// reach. Chosen from the estate as it stands rather than from taste: the
	// largest root that is NOT one of the two known offenders is
	// venue-okx's serve at 358 lines, so 400 leaves ordinary growth alone and
	// catches the next function that starts becoming a runConsumers.
	maxCompositionRootLines = 400
	// budgetSlack is how far under its budget a ratcheted function may drift
	// before the entry must be re-baselined. Without it the ratchet loosens
	// silently: 400 lines could be extracted and the budget would still permit
	// putting them back.
	budgetSlack = 60
)

// compositionRootBudget is the line count each over-cap root is allowed, and may
// only be revised DOWNWARD. Every entry names the issue that retires it.
var compositionRootBudget = map[string]int{
	// #643. Was 1,474 lines and 61 constructors with two bare nils. All four
	// extractions the issue named have now landed — the pre-trade gate
	// (pretrade.go, which took both nils with it), the venue router (router.go),
	// the outbox instrumentation (outbox.go) and the read surface
	// (readsurface.go) — and each one is now reachable by a test that could not
	// be written while it was a local.
	//
	// A FIFTH WAS PAID FOR RATHER THAN PLANNED (#713): that change needed four
	// lines of shutdown wiring here, this ratchet refused them, and the
	// dual-control gate's three postures moved to dualcontrol.go instead of the
	// budget going up. That is the guard working as intended on a change that had
	// nothing to do with #643.
	//
	// What is left is subscription wiring and goroutine lifecycles: real work, and
	// NOT the same shape, because it has no product to return and assert about.
	// Retiring this entry means deciding what that half should look like, which is
	// a separate question from this one.
	//
	// A SIXTH RISE WAS PAID FOR RATHER THAN AVOIDED (#1007): the realised-candle
	// fold that makes a participation cap measurable needed a registry binding,
	// three service options and one more subscription goroutine here. The
	// registry construction, the metrics and the delivery policy all moved to
	// participation.go, the way volprofile.go did for #897; what is left in this
	// function is the twenty-one lines that genuinely are subscription wiring and
	// a goroutine lifecycle, which is the half this entry already says it has no
	// extraction shape for.
	"services/oms/cmd/oms/main.go:runConsumers": 1223,
	// #643. Its own doc says composition-root wiring escapes every unit test and
	// that this service has twice shipped a crashing root with a green suite. Two
	// of its constructions are already extracted into named builders with tests
	// (liquidity.go, margin.go); the rest are not.
	"services/risk-engine/cmd/risk-engine/main.go:runEngine": 699,
}

func TestNoCompositionRootGrowsWithoutSaying(t *testing.T) {
	root := moduleRoot(t)
	found := compositionRootLines(t, root)

	// NON-VACUITY. A walk that matched nothing — a moved services/ tree, a
	// changed layout — would pass every arm below while checking nothing, which
	// is the failure this whole directory keeps relearning.
	if len(found) < 10 {
		t.Fatalf("only %d composition-root functions were found under services/*/cmd — this guard "+
			"is asserting nothing", len(found))
	}

	seen := map[string]bool{}
	names := make([]string, 0, len(found))
	for name := range found {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		lines := found[name]
		budget, budgeted := compositionRootBudget[name]
		if budgeted {
			seen[name] = true
		}
		switch {
		case !budgeted && lines > maxCompositionRootLines:
			t.Errorf("%s is %d lines, over the %d-line cap for a composition root.\n"+
				"  Everything in it is unreachable by any test: the constructions are locals in\n"+
				"  package main. Extract the constructions a test can assert about into named\n"+
				"  builders that RETURN what they built (services/oms/cmd/oms/pretrade.go and\n"+
				"  services/risk-engine/cmd/risk-engine/liquidity.go are the shape), or add a\n"+
				"  budget entry above naming the issue that retires it.",
				name, lines, maxCompositionRootLines)
		case budgeted && lines > budget:
			t.Errorf("%s grew to %d lines against a budget of %d (#643).\n"+
				"  The budget is a RATCHET: this function may shrink and may not grow. Every\n"+
				"  addition to it is individually reasonable, which is how it reached 1,474 lines\n"+
				"  with two bare nils in the middle. Put the new wiring in a named builder beside\n"+
				"  it instead.",
				name, lines, budget)
		case budgeted && lines <= maxCompositionRootLines:
			t.Errorf("the budget entry for %s is STALE: it is now %d lines, at or under the %d-line\n"+
				"  cap. Delete the entry — an exemption that outlives its repair is how the next\n"+
				"  one gets waved through.",
				name, lines, maxCompositionRootLines)
		case budgeted && budget-lines > budgetSlack:
			t.Errorf("%s is %d lines against a budget of %d — %d lines of slack.\n"+
				"  Lower the budget to %d in the same commit that earned it, or the ratchet\n"+
				"  silently permits putting those lines back.",
				name, lines, budget, budget-lines, lines)
		}
	}

	// A BUDGET FOR A FUNCTION THAT NO LONGER EXISTS is the same dead entry in the
	// other direction: it vouches for nothing and reads as coverage.
	for name := range compositionRootBudget {
		if !seen[name] {
			t.Errorf("the budget entry for %s names no composition-root function — it was renamed, "+
				"moved or deleted. Delete the entry.", name)
		}
	}
}

// compositionRootLines maps "path:func" to the line span of every top-level
// function in a `package main` file under services/*/cmd.
//
// PARSED, NOT COUNTED WITH grep. A brace-counting scan matches braces in strings
// and comments, and three guards in this repository have passed while checking
// their own comment text.
func compositionRootLines(t *testing.T, root string) map[string]int {
	t.Helper()
	out := map[string]int{}
	servicesDir := filepath.Join(root, "services")
	err := filepath.Walk(servicesDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		slash := filepath.ToSlash(rel)
		if !strings.Contains(slash, "/cmd/") {
			return nil
		}
		fset := token.NewFileSet()
		f, parseErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if parseErr != nil {
			return fmt.Errorf("parse %s: %w", slash, parseErr)
		}
		if f.Name.Name != "main" {
			return nil
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			start := fset.Position(fn.Pos()).Line
			end := fset.Position(fn.End()).Line
			out[slash+":"+fn.Name.Name] = end - start + 1
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk services/: %v", err)
	}
	return out
}
