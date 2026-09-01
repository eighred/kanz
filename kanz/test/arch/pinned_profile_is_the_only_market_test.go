package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// A WORKING PARENT IS DERIVED AGAINST THE CURVE IT NAMES, NEVER THE NEWEST ONE
// (#897).
//
// # The failure this exists to catch
//
// The OMS enters internal/execution/algo in three places — validateSchedule at
// admission, authorizeChild when a slice arrives, and schedule.Due on the driver
// tick — and all three must derive the SAME children for one parent, forever.
// authorizeChild compares an inbound child's quantity as an exact rational, so a
// difference in any digit refuses the driver's own slice as a forgery and the
// parent stops advancing with every screen showing it working.
//
// For TWAP that is free: every input is a durable field of the order. A
// volume-driven schedule has one more input the order does not carry and the
// client never sent — the shape of the market — and it MOVES. #897's answer is a
// pin: admission resolves ONE published profile version, stamps it on the order,
// and every later derivation resolves that version.
//
// THE WHOLE THING TURNS ON ONE RULE, AND THE RULE IS ONE LINE OF CODE. Exactly
// one function may ask "what is the newest profile", and only admission may call
// it. A driver or a child check written with currentMarket instead of
// scheduleMarket compiles, passes every unit test that uses a single registry,
// and produces a perfectly valid schedule — a different one. It would be found in
// production, by a legitimate child being rejected, on a fleet where the pods
// disagree.
//
// # What this checks
//
// In services/oms/internal/order:
//
//  1. currentMarket has at least one caller and EVERY caller is validateSchedule.
//     Both halves matter: no caller at all means no schedule is ever planned
//     against a measured curve, which is the pre-#897 state, and a second caller
//     is the divergence above.
//  2. algo.UnknownMarket is constructed ONLY inside currentMarket and
//     pinnedMarket. Those two end every failed lookup on it, which is the honest
//     answer for an absent, stale or unresolvable curve; a third site is a path
//     that answers UNKNOWN without ever asking the registry, and the refusal an
//     operator then reads names the MARKET while the cause is that line.
//
// # What it CANNOT check, stated so the name is not overread
//
// That scheduleMarket resolves the RIGHT version, that the three derivation sites
// pass the view they were given rather than a different one, or that the pinned
// curve and the admitted curve are the same curve. Those are behavioural and they
// are proved behaviourally, by
// services/oms/internal/order.TestScheduleE2E_ASecondPodDerivesTheIdenticalSchedule
// and its real-broker sibling TestVolumeProfileReachesASecondPod. This guard
// bounds the one SHAPE that makes those tests possible to keep true.
//
// # Why the AST and not a grep
//
// Because this file's own prose says "currentMarket" nine times, and so does
// schedule_market.go's. A textual check would fire on the paragraphs and pass on
// a real caller written a line differently — the failure
// test/arch/every_algo_is_reachable_test.go already records in its own header.
// Comments are detached (parser.ParseFile with mode 0).

// pinnedMarketPkg is the tree this guard bounds.
const pinnedMarketPkg = "services/oms/internal/order"

// currentMarketFn may be called from exactly one place, and callerOfCurrent names
// it. They are separate constants because the ERROR has to be able to say which
// of the two the offending call was not.
const (
	currentMarketFn  = "currentMarket"
	callerOfCurrent  = "validateSchedule"
	pinnedMarketFn   = "scheduleMarket"
	unknownMarketSym = "UnknownMarket"
	// resolverFn is the pinned lookup. It and currentMarketFn are the only two
	// functions that may end on algo.UnknownMarket — see the walk below.
	resolverFn = "pinnedMarket"
)

func TestOnlyAdmissionReadsTheCurrentVolumeProfile(t *testing.T) {
	root := moduleRoot(t)
	dir := filepath.Join(root, filepath.FromSlash(pinnedMarketPkg))

	files := goFilesUnder(t, dir)
	fset := token.NewFileSet()

	var (
		nonTest        int
		currentCallers []string
		unknownSites   []string
		sawCurrentDecl bool
		sawPinnedDecl  bool
	)

	for _, f := range files {
		if strings.HasSuffix(f.rel, "_test.go") {
			// A TEST MAY DO EITHER. It is not what derives a working parent's
			// schedule in production, and the two-pod tests deliberately drive both
			// paths to prove they differ.
			continue
		}
		nonTest++
		rel := pinnedMarketPkg + "/" + f.rel
		file, err := parser.ParseFile(fset, filepath.Join(dir, filepath.FromSlash(f.rel)), f.body, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}

		// enclosing tracks which function body the walk is inside, so a call can be
		// attributed to its caller rather than only to its file.
		var enclosing string
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.FuncDecl:
				enclosing = node.Name.Name
				switch node.Name.Name {
				case currentMarketFn:
					sawCurrentDecl = true
				case pinnedMarketFn:
					sawPinnedDecl = true
				}
			case *ast.SelectorExpr:
				// s.currentMarket(...) — a method on the service.
				if node.Sel.Name == currentMarketFn {
					currentCallers = append(currentCallers, enclosing+" ("+rel+")")
				}
				// algo.UnknownMarket{} — the pre-#897 wiring.
				//
				// THE TWO RESOLVERS ARE THE EXEMPTION AND THEY ARE THE POINT. Each
				// ends every failed lookup on algo.UnknownMarket, which is the honest
				// answer for a curve that is absent, stale or unresolvable — the
				// three-value model, held in ONE pair of functions. What must not exist
				// is a fourth site that answers UNKNOWN without asking the registry.
				if node.Sel.Name == unknownMarketSym && enclosing != currentMarketFn && enclosing != resolverFn {
					if id, ok := node.X.(*ast.Ident); ok && id.Name == "algo" {
						unknownSites = append(unknownSites, enclosing+" ("+rel+")")
					}
				}
			}
			return true
		})
	}

	// NON-VACUITY, BOTH HALVES. A renamed package scans nothing; a renamed
	// function makes every arm below trivially satisfied while the property it
	// asserts has quietly stopped existing.
	if nonTest == 0 {
		t.Fatalf("no non-test Go files under %s — the package moved and this guard is protecting "+
			"an empty directory", pinnedMarketPkg)
	}
	if !sawCurrentDecl || !sawPinnedDecl {
		t.Fatalf("%s declares currentMarket=%v scheduleMarket=%v — one of the two seams #897 "+
			"separated has been renamed or removed, so this guard is asserting nothing about the "+
			"thing it exists for",
			pinnedMarketPkg, sawCurrentDecl, sawPinnedDecl)
	}

	// ARM 1: currentMarket is called from admission and nowhere else. The
	// declaration itself is not a call, so the only entry expected is
	// validateSchedule's.
	sort.Strings(currentCallers)
	var offenders []string
	for _, c := range currentCallers {
		if strings.HasPrefix(c, callerOfCurrent+" ") {
			continue
		}
		offenders = append(offenders, c)
	}
	if len(offenders) > 0 {
		t.Errorf("%s reads the CURRENT volume profile from %v.\n\n"+
			"Only %s may, because admission is the only moment the choice is RECORDED — it stamps "+
			"the version on the order and every later derivation resolves that one. A derivation "+
			"after admission that reads `current` re-plans a working parent against a market it "+
			"was never sized for: the driver derives one schedule, authorizeChild derives another, "+
			"the exact-rational quantity comparison fails, and a legitimate child is refused as a "+
			"forgery while the parent advances no further.\n\n"+
			"Use %s, which resolves the version the ORDER names.",
			pinnedMarketPkg, offenders, callerOfCurrent, pinnedMarketFn)
	}
	if len(currentCallers) == 0 {
		t.Errorf("nothing in %s calls %s, so no schedule is ever planned against a measured curve "+
			"and every VWAP or POV order is refused at admission — the state #897 closed",
			pinnedMarketPkg, currentMarketFn)
	}

	// ARM 2: the pre-#897 wiring is gone. algo.UnknownMarket in this package is a
	// path that hands the algorithms UNKNOWN on purpose, which is now a
	// volume-driven order refused for a reason that names the market rather than
	// the wiring.
	if len(unknownSites) > 0 {
		sort.Strings(unknownSites)
		t.Errorf("%s constructs algo.%s at %v.\n\n"+
			"That was correct while this service held no market data and is a defect now that it "+
			"does (#897): the two functions above resolve a published profile and fall back to "+
			"UnknownMarket THEMSELVES when there is nothing to resolve, so a second construction "+
			"here is a path that refuses a volume-driven order without ever asking the registry. "+
			"The refusal an operator then reads names the MARKET while the cause is this line.",
			pinnedMarketPkg, unknownMarketSym, unknownSites)
	}
}
