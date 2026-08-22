package arch

import (
	"fmt"
	"go/ast"
	"go/token"
	"sort"
	"strings"
	"testing"
)

// EVERY MANDATE CONSUMER MUST HANDLE EVERY TERMINAL LOOKUP SENTINEL (#619).
//
// # What this exists to stop
//
// MandateRegistry.Mandate returns two kinds of error, and they need opposite
// treatment:
//
//	TRANSIENT   the read failed        → return it, let the message redeliver
//	TERMINAL    the mandate data is    → refuse ONCE and ack; a redelivery
//	            unreadable or ambiguous   re-reads the same bytes forever
//
// Both call sites are written as "handle the terminal ones by name, return
// everything else". That shape is correct and it is also a trap: adding a THIRD
// terminal sentinel silently makes it transient at every call site that was not
// edited, and the failure is not a refusal — it is an infinite redelivery loop.
//
// This is not hypothetical. ErrMandateUnreadable (#619) was added to the registry
// and to the pre-trade gate, and the post-trade monitor was missed on the first
// pass. Its own doc comment names the cost: "a redelivery loop on a position FACT
// is how a monitor stops monitoring everything else" — one unparseable mandate
// would have stopped every OTHER portfolio being evaluated. The guard is here
// because the omission was made, not because it was imagined.
//
// # What this checks
//
// Every non-test file that calls a MandateSource's Mandate(...) must name every
// terminal sentinel below. It is a crude check — presence of an identifier, not
// proof of correct handling — and deliberately so: the behavioural half is
// covered by tests (compliance's TestAnUnreadableMandateIsRefusedEvenWhenMandates
// AreNotRequired and monitor's TestAnUnreadableMandateIsAckedRatherThanRedelivered
// Forever). What no test can cover is the sentinel that does not exist yet, and
// the call site nobody remembered to open. Presence is what an AST can see, and
// it fails the build at exactly the moment a fourth sentinel is added.
//
// # Adding a terminal sentinel
//
// Add it to terminalMandateSentinels. The build then fails until every call site
// names it, which is the reminder this guard exists to give.

// terminalMandateSentinels are the MandateRegistry errors that never resolve on a
// retry, so a caller must turn each into a verdict rather than a redelivery.
var terminalMandateSentinels = []string{
	// The registry cannot say WHOSE mandate governs this portfolio. The ambiguity
	// is in the mandate data, not the read.
	"ErrMandateTenantUnresolved",
	// A mandate for this portfolio WAS published and could not be applied. The
	// stream is compacted, so the failing message is the last one that subject
	// serves until an operator republishes.
	"ErrMandateUnreadable",
}

func TestEveryMandateLookupHandlesEveryTerminalSentinel(t *testing.T) {
	root := moduleRoot(t)

	type site struct {
		file    string
		handles map[string]bool
	}
	var sites []site

	walkGoFiles(t, root, ".", token.NewFileSet(), func(rel string, f *ast.File) {
		if strings.HasSuffix(rel, "_test.go") {
			return
		}
		// The registry itself DEFINES the sentinels and is not a consumer of its
		// own lookup; including it would make the guard trivially satisfied by the
		// declarations.
		if strings.HasSuffix(rel, "internal/compliance/mandate.go") {
			return
		}

		calls := false
		named := map[string]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Mandate" && len(call.Args) == 4 {
					calls = true
				}
			}
			if id, ok := n.(*ast.Ident); ok {
				named[id.Name] = true
			}
			return true
		})
		if !calls {
			return
		}
		handles := map[string]bool{}
		for _, s := range terminalMandateSentinels {
			handles[s] = named[s]
		}
		sites = append(sites, site{file: rel, handles: handles})
	})

	// NON-VACUITY. Two non-test call sites resolve a mandate today — the OMS
	// pre-trade gate (internal/compliance/gate.go) and the post-trade monitor
	// (services/compliance/internal/monitor/monitor.go). A scan finding fewer has
	// lost sight of one, and the one it lost is the one that would loop.
	if len(sites) < 2 {
		var found []string
		for _, s := range sites {
			found = append(found, s.file)
		}
		t.Fatalf("found %d mandate lookup call site(s) (%v) — expected at least 2 (the pre-trade "+
			"gate and the post-trade monitor). The call shape moved and this guard is asserting "+
			"nothing", len(sites), found)
	}

	var problems []string
	for _, s := range sites {
		for _, sentinel := range terminalMandateSentinels {
			if !s.handles[sentinel] {
				problems = append(problems, fmt.Sprintf("%s resolves a mandate but never names %s",
					s.file, sentinel))
			}
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		t.Errorf("%d mandate lookup(s) do not account for a TERMINAL sentinel:\n\n  %s\n\n"+
			"A terminal error that falls through to the transient arm is returned to the bus, and the "+
			"message is redelivered against mandate data that cannot change — an infinite loop that "+
			"starves every other portfolio of evaluation, not a refusal (#619). Turn it into a verdict: "+
			"refuse once, ack, and say so.",
			len(problems), strings.Join(problems, "\n  "))
	}
}

// EVERY MANDATE CONSUMER IN A COMPOSITION ROOT COUNTS WHAT IT DROPPED (#619).
//
// The registry marks the portfolio and the gate refuses its orders whether or not
// this seam is wired, so this is not what makes the failure safe — it is what
// makes it VISIBLE ESTATE-WIDE. Without it the only signals are a log line and a
// per-order refusal, and a broken mandate on a portfolio that is not trading today
// produces neither: the hole sits in the registry of every pod that boots, and
// nothing counts it until somebody happens to trade that book.
//
// Default-deny across composition roots rather than a list of the two that exist,
// because the third service to consume the mandate stream is the one that will be
// wired without it.
func TestEveryMandateConsumerCountsItsRejections(t *testing.T) {
	root := moduleRoot(t)

	var missing []string
	seen := 0
	walkGoFiles(t, root, ".", token.NewFileSet(), func(rel string, f *ast.File) {
		if strings.HasSuffix(rel, "_test.go") || !strings.Contains(rel, "/cmd/") {
			return
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "NewMandateConsumer" {
				return true
			}
			seen++
			for _, arg := range call.Args {
				inner, ok := arg.(*ast.CallExpr)
				if !ok {
					continue
				}
				if s, ok := inner.Fun.(*ast.SelectorExpr); ok && s.Sel.Name == "WithMandateRejectionObserver" {
					return true
				}
				if id, ok := inner.Fun.(*ast.Ident); ok && id.Name == "WithMandateRejectionObserver" {
					return true
				}
			}
			missing = append(missing, rel)
			return true
		})
	})

	// NON-VACUITY. Two composition roots feed a mandate registry today: the OMS
	// (its pre-trade gate) and the compliance service (its post-trade monitor).
	if seen < 2 {
		t.Fatalf("found %d composition-root NewMandateConsumer call(s) — expected at least 2 "+
			"(services/oms and services/compliance). The construction moved and this guard is "+
			"asserting nothing", seen)
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("%d composition root(s) build a MandateConsumer with no rejection observer:\n\n  %s\n\n"+
			"A mandate that cannot be applied leaves its portfolio un-governable until someone "+
			"republishes it, and on a book that is not trading today nothing else will say so (#619). "+
			"Pass comp.WithMandateRejectionObserver and increment a counter.",
			len(missing), strings.Join(missing, "\n  "))
	}
}
