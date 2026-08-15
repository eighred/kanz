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

// AN ORDER ID MUST BE ONE A VENUE WILL ACCEPT.
//
// # The defect
//
// The order id is not only ours. The connectors stamp it directly as the
// exchange's client order id — `newClientOrderId` at Binance, `clOrdId` at OKX —
// because a deterministic id we choose is what makes a retried placement
// idempotent at the exchange and what lets an ambiguous timeout be resolved by
// asking the venue what it did with THAT id.
//
// api-gateway minted order ids with uuid.NewString(): 36 characters, hyphenated.
// OKX accepts at most 32, letters and digits only, and answers anything else
// with `51000 Parameter clOrdId error`. So EVERY ORDER SUBMITTED THROUGH THE HTTP
// GATEWAY WAS UNPLACEABLE AT OKX — admitted, stored, its ORDER_ACCEPTED FACT
// published, and then refused by the exchange. Binance accepts hyphens and 36
// characters, so the identical order traded normally there, which is why nothing
// noticed: the platform worked on one venue and silently could not trade on the
// other.
//
// # Why a guard rather than trusting the fix
//
// The fix is one line in one file, and uuid.NewString() is the obvious thing to
// reach for the next time somebody needs an identifier. Nothing in the type
// system distinguishes an id that is only ours from one an exchange will parse,
// and the failure is invisible from inside this repository: it compiles, every
// test passes, and the only evidence is an error code from a venue nobody can
// reach without credentials.
//
// # What this checks
//
// No production code assigns a UUID to an OrderId — as a struct field, or by
// assignment. internal/orderid.Mint is the one way to make one.
//
// # What it cannot check
//
// An id that arrives from a CLIENT. Those are refused at the connector
// (okx_venue.go calls orderid.Valid), because the platform does not choose them.
// Nor does it check ids built by concatenation or hashing — internal/orderid's
// own tests, and the schedule package's, assert those are placeable.

// orderIDScopes are the trees searched.
var orderIDScopes = []string{"services", "internal", "cmd", "tools"}

// uuidMinters are the calls that produce a hyphenated UUID.
var uuidMinters = map[string]bool{"NewString": true, "New": true, "NewRandom": true}

// orderIDExempt maps "file:line" to an argued reason a UUID may become an order
// id there, and what retires the entry.
//
// EMPTY. An entry here is a decision that some order cannot be placed at OKX.
var orderIDExempt = map[string]string{}

func TestNoOrderIDIsMintedAsAUUID(t *testing.T) {
	root := moduleRoot(t)

	var offenders []string
	seenExempt := map[string]bool{}
	scanned, sawOrderID := 0, 0

	for _, scope := range orderIDScopes {
		for _, gf := range goFilesUnder(t, filepath.Join(root, scope)) {
			if strings.HasSuffix(gf.rel, "_test.go") {
				// A TEST MAY MINT WHATEVER IT LIKES — including a bad id, which is
				// exactly what the connector's refusal tests need.
				continue
			}
			scanned++
			rel := filepath.ToSlash(filepath.Join(scope, gf.rel))

			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, filepath.Join(root, scope, gf.rel), gf.body, 0)
			if err != nil {
				continue // the compiler owns that failure and reports it better
			}

			// PARSED RATHER THAN GREPPED, and that is not fastidiousness: the fix
			// for this defect left a comment reading "NOT uuid.NewString()" in the
			// very file it repaired, and a text search flags it. A guard whose
			// first finding is the explanation of the bug it checks for is a guard
			// nobody will keep.
			ast.Inspect(file, func(n ast.Node) bool {
				switch v := n.(type) {
				case *ast.KeyValueExpr: // OrderId: uuid.NewString()
					if isOrderIDKey(v.Key) {
						sawOrderID++
						if mintsUUID(v.Value) {
							offenders = append(offenders, at(fset, rel, v.Pos()))
						}
					}
				case *ast.AssignStmt: // x.OrderId = uuid.NewString()
					for i, lhs := range v.Lhs {
						if !isOrderIDKey(lhs) {
							continue
						}
						sawOrderID++
						if i < len(v.Rhs) && mintsUUID(v.Rhs[i]) {
							offenders = append(offenders, at(fset, rel, v.Pos()))
						}
					}
				}
				return true
			})
		}
	}

	// NON-VACUITY, the walk half.
	if scanned < 100 {
		t.Fatalf("scanned only %d non-test files across %v — the walk is broken, not the estate",
			scanned, orderIDScopes)
	}
	// NON-VACUITY, the subject half: if nothing in the module is seen assigning an
	// order id, the matcher stopped matching and this guard asserts nothing.
	if sawOrderID == 0 {
		t.Fatalf("found NO code assigning an order id anywhere in %v — the matcher stopped "+
			"working and this guard is checking nothing", orderIDScopes)
	}

	// Exemptions are keyed by site, so they are resolved after collection.
	var kept []string
	for _, o := range offenders {
		site := strings.SplitN(o, " ", 2)[0]
		if reason, ok := orderIDExempt[site]; ok {
			seenExempt[site] = true
			t.Logf("%s: exempt — %s", site, reason)
			continue
		}
		kept = append(kept, o)
	}

	if len(kept) > 0 {
		sort.Strings(kept)
		t.Errorf("%d site(s) mint an order id as a UUID: %v.\n"+
			"The order id is stamped directly as the exchange's client order id. OKX accepts at "+
			"most 32 characters, letters and digits only, and refuses anything else with "+
			"\"Parameter clOrdId error\" — so a hyphenated 36-character UUID makes the order "+
			"UNPLACEABLE there while Binance accepts it and trades normally. The order is "+
			"admitted, stored and announced, and only the exchange objects.\n"+
			"Use internal/orderid.Mint, which gives the same 128 bits in 32 hex characters.",
			len(kept), kept)
	}

	for site, reason := range orderIDExempt {
		if !seenExempt[site] {
			t.Errorf("exemption for %q (%s) matches no UUID-minted order id — remove the entry "+
				"so it cannot license the next one", site, reason)
		}
	}
}

// isOrderIDKey reports whether an expression names an order-id field, as a bare
// identifier (a composite-literal key) or a selector (x.OrderId).
func isOrderIDKey(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name == "OrderId" || v.Name == "OrderID"
	case *ast.SelectorExpr:
		return v.Sel.Name == "OrderId" || v.Sel.Name == "OrderID"
	}
	return false
}

// mintsUUID reports whether an expression calls a uuid minter, directly or
// wrapped in one call (uuid.New().String()).
func mintsUUID(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "uuid" && uuidMinters[sel.Sel.Name] {
			return true
		}
		// uuid.New().String() — the receiver is the minting call.
		return mintsUUID(sel.X)
	}
	return false
}

func at(fset *token.FileSet, rel string, pos token.Pos) string {
	return rel + ":" + itoa(fset.Position(pos).Line) + " (order id from a UUID)"
}
