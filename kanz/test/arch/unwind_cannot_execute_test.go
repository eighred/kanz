package arch

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// THE UNWIND DECISION PATH MUST NOT BE ABLE TO TRADE (M4, #74).
//
// internal/risk/unwind decides what a breached portfolio WOULD have to shed. Its
// scope is "decides, does not execute", and that is worth an executable guard
// rather than a sentence in a doc comment, because the gap between the two is one
// import.
//
// The pressure is real and it is sympathetic. A breach arrives, the reduction is
// already computed, the venue router is one package away, and turning a proposal
// into an order looks like finishing the job. What makes it not finishing the job
// is that a breach is very often a PRICE MOVING — a book that liquidates into a
// falling market is how a risk control becomes the accelerant. Auto-deleveraging
// that acts is long-term work gated on an operator's judgement, not on the
// arithmetic being ready.
//
// So the rule is not "do not call the exchange". It is that this package cannot
// SEE anything that trades: no order schema to build a command with, no venue
// adapter to send one through, no producer to publish one on.
var unwindForbiddenImports = []struct {
	path string
	why  string
}{
	{"kanz-schemas-go/order/v1", "the order schema — with it, a proposal becomes a SubmitOrder"},
	{"kanz-schemas-go/command/v1", "the command schema — the envelope an order is issued in"},
	{"internal/execution", "the venue router and the exchange adapters"},
	{"pkg/bus", "the producer — publishing a command is executing it, one hop later"},
	{"services/oms", "the order management service"},
}

// unwindPkgDir is the decision path this guard protects.
const unwindPkgDir = "internal/risk/unwind"

func TestUnwindDecisionPathCannotExecute(t *testing.T) {
	root := moduleRoot(t)
	dir := filepath.Join(root, filepath.FromSlash(unwindPkgDir))

	var offences []string
	scanned := 0

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		// Test files are scanned too: a test that imports the venue router to
		// "check the proposal would work" reintroduces exactly the coupling this
		// forbids, and it is the likeliest place for it to arrive.
		scanned++
		f, perr := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if perr != nil {
			return perr
		}
		rel := filepath.ToSlash(mustRel(root, path))
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			for _, banned := range unwindForbiddenImports {
				if strings.Contains(p, banned.path) {
					offences = append(offences, rel+" imports "+p+" — "+banned.why)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}

	// NON-VACUITY. A walk that finds no files passes no matter what the package
	// imports, and a package that was renamed or removed would go quietly green.
	if scanned == 0 {
		t.Fatalf("scanned no Go files under %s — the guard is pointed at nothing, which is not "+
			"the same as the rule holding", unwindPkgDir)
	}

	if len(offences) > 0 {
		sort.Strings(offences)
		t.Fatalf("the unwind decision path can reach something that trades:\n  %s\n\n"+
			"It decides what a breached portfolio WOULD shed and stops there (#74). A breach is "+
			"very often a price moving, and a book that liquidates into a falling market is how a "+
			"risk control becomes the accelerant — so whether to ACT on a proposal is an "+
			"operator's judgement, deliberately not wired.\n\nIf auto-deleveraging is being built "+
			"for real, it does not belong here: put the executing half in its own package with its "+
			"own review, and leave this one unable to place an order.",
			strings.Join(offences, "\n  "))
	}
}
