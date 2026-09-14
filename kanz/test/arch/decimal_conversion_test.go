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

// dec.ToProto WRAPS when a value does not fit an int64 coefficient at the fixed
// scale — roughly $92bn in money terms. A wrapped coefficient is a fabricated
// number, and on a capital path the system then acts on it: a position valued
// at a wrapped figure, an average fill price that is not the price anything
// filled at.
//
// dec.ToProtoScaled preserves magnitude instead. This guard keeps the capital
// paths on it. ToProto is not banned outright — ~30 reporting and analytics
// callers use it and are unaffected by the ceiling — so the rule is scoped to
// the packages where a wrong number moves money.
// THIS LIST WAS TOO SHORT, AND THE GUARD PASSED ANYWAY (#94).
//
// It named the two OMS packages the rule was written for, so it reported green
// while genuine capital paths kept the wrapping conversion — a guard scoped
// narrower than its own premise is worse than none, because it reads as coverage.
// #94's evidence line said "Every capital path is already off it"; that was
// false when written and stayed false for as long as this list did not move.
//
// What was still wrapping, found by asking what actually spends money rather
// than which package the guard already covered:
//
//	the venue sweep       a live MARKET order to flatten a residual
//	the order submission  SubmitOrder.Quantity, straight to a venue
//	the ledger cash leg   a subscription or redemption in the book of record
//	the balance break     the Expected/Actual/Delta an operator acts on
//	the healed FACTs      the filled size the journal folds as venue truth
//
// The threshold is not exotic. dec.ToProto wraps once the scaled coefficient
// exceeds an int64 — about 92.2 BILLION units at scale 8 — which is $92bn in
// money terms but an ordinary position size in tokens: 1e12 units renders as
// 77662796314.5224192. Both venues here list assets that trade in the trillions.
// internal/execution is the one both venues share. Its ParseDec is where every
// exchange string becomes a number this platform acts on — a fill quantity, a
// fill price, a commission — so it is the highest-leverage entry on this list:
// venue-binance does not have its own copy, it aliases these (bridge.go).
// services/datamaster/internal/server IS DELIBERATELY ABSENT.
//
// It calls dec.ToProto on purpose, and the call is not a conversion: the
// operator-override handler asks `FromProto(ToProto(price)).Cmp(price) != 0`,
// which is an ASSERTION that a chosen price survives the platform's fixed scale
// exactly. ToProtoScaled raises the exponent rather than failing, so it would
// accept the over-precise input the check exists to refuse. Adding the package
// here would turn a correct validation into a guard violation and invite someone
// to "fix" it (#189). The reasoning also lives beside the call, because a rule
// recorded only in the guard is a rule the next reader does not see.
var capitalPathPackages = []string{
	"services/regulatory/internal/server",
	"services/oms/internal/position",
	"services/oms/internal/order",
	"services/venue-okx/internal/okx",
	"services/venue-binance/internal/binance",
	"services/accounting/internal/cashmove",
	"services/datamaster/internal/feed",
	"internal/signal/translate",
	"internal/execution",
	"internal/marketedge",
}

// pendingErrorThreading are capital-path call sites that still use the wrapping
// conversion because their enclosing function has NO error return.
//
// EMPTY, and that is the point: it was the worklist, and the work is done. The
// guard's stale-exemption arm means an entry whose file no longer calls
// dec.ToProto FAILS the build, so this map cannot quietly accumulate excuses.
// A new entry needs a written reason and should be temporary.
var pendingErrorThreading = map[string]string{}

func TestCapitalPathsDoNotUseWrappingToProto(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()
	var offenders []string
	scanned, seenPending := 0, map[string]bool{}

	for _, pkg := range capitalPathPackages {
		dir := filepath.Join(root, filepath.FromSlash(pkg))
		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			scanned++
			f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if perr != nil {
				return fmt.Errorf("parse %s: %w", path, perr)
			}
			rel := filepath.ToSlash(mustRel(root, path))
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "ToProto" {
					return true
				}
				id, ok := sel.X.(*ast.Ident)
				if !ok || id.Name != "dec" {
					return true
				}
				if _, pending := pendingErrorThreading[rel]; pending {
					seenPending[rel] = true
					return true
				}
				offenders = append(offenders, fmt.Sprintf("%s:%d", rel, fset.Position(call.Pos()).Line))
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}

	// NON-VACUITY: if the walk found no files, this guard would pass no matter
	// how many capital paths used the wrapping conversion.
	if scanned == 0 {
		t.Fatal("scanned zero Go files across the capital-path packages — the walk is broken")
	}

	// A declared exception that no longer has any dec.ToProto call is DEAD.
	// Removing it is how the follow-up task reports progress; leaving it lets a
	// future regression hide behind a stale entry.
	for file := range pendingErrorThreading {
		if !seenPending[file] {
			offenders = append(offenders, file+": declared in pendingErrorThreading but calls dec.ToProto nowhere — stale exemption, remove it")
		}
	}

	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("capital-path code calls the WRAPPING dec.ToProto:\n  %s\n\n"+
			"ToProto silently wraps above an int64 coefficient at the fixed scale (~$92bn in "+
			"money terms), and the system then acts on the fabricated number — a position "+
			"valued at a figure nothing is worth, an average price nothing filled at. Use "+
			"dec.ToProtoScaled, which preserves magnitude and reports when it cannot. If the "+
			"enclosing function has no error return, add the FILE to pendingErrorThreading "+
			"with the reason, rather than dropping the ok return.",
			strings.Join(offenders, "\n  "))
	}
}

func mustRel(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return rel
}
