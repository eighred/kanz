package arch

import (
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

// THE BOOK OF RECORD SETTLES EVERYTHING AT EXECUTION, AND THAT MUST BE A STATED
// POSTURE RATHER THAN A SILENCE (#1043).
//
// Until the settlement axis landed, every fill was booked as fully settled the
// instant it matched: ledger.FromFill emitted the position leg and the FULL cash
// leg at the venue's execution time, and no field anywhere in the journal could
// express "I traded it, I do not yet own it". NAV, exposure, leverage, mandate
// headroom, margin and buying power (internal/cashview) all inherited that.
//
// T+0 IS THE RIGHT MODEL TODAY AND IT IS STILL A DECISION. Both venue adapters
// are crypto spot, where the base asset and the quote cash move in the same
// exchange transaction, so nothing in this estate is currently mis-booked. What
// was missing is that nobody could tell the two readings apart from outside: a
// book that never tracks settlement and a book that tracks it with nothing
// outstanding produce identical numbers, an identically empty unsettled ladder,
// and an identically reconciling NAV.
//
// # What this binds together
//
//	BinanceVenue.MarginModes, OKXVenue.MarginModes    the capability that justifies T+0
//	settlement_basis_posture.go                       the posture that rests on it
//	services/accounting/cmd/accounting/main.go        the process that publishes it
//
// THE JUSTIFICATION IS DERIVED, NOT TYPED IN. This guard does not contain the
// words "crypto spot" as an assertion about the world; it reads the two
// connectors' declared margin modes off the AST and requires the posture to agree
// with what it finds. The day a venue adapter declares a non-CASH margin mode —
// the capability signal #417 introduced and the OMS already refuses orders
// against — the T+0 justification has expired and this guard fails, in the
// direction that says "re-derive the posture", rather than passing because a
// sentence in a Go comment still reads well. That is the failure mode
// corporate_action_absence_is_stated_test.go was written for, one axis over: a
// comment justifying a trade-off is dated evidence.
//
// # Why a guard and not a comment
//
// Because the comment IS the thing being protected. Three guards in this
// repository have passed with the checked thing deleted because they matched
// their own prose, so every assertion below reads the AST — never raw source.

// settlementPostureFile is where the posture lives. Cited by path so the citation
// is checkable: a guard pointing at a file that no longer exists asserts nothing.
var settlementPostureFile = filepath.Join(
	"services", "accounting", "cmd", "accounting", "settlement_basis_posture.go")

// settlementCompositionRoot is the process that must publish the posture.
var settlementCompositionRoot = filepath.Join(
	"services", "accounting", "cmd", "accounting", "main.go")

// spotMarginMode is the enum constant a spot-only connector declares.
// order.v1.MarginMode's UNSPECIFIED member IS the cash/spot regime — see
// OKXVenue.marginModeSupported, which spells that out — so a connector returning
// this alone is asserting spot and nothing else.
const spotMarginMode = "MarginMode_MARGIN_MODE_UNSPECIFIED"

// venueMarginModes returns, per venue connector type, the margin-mode enum
// constants its MarginModes method declares.
//
// DISCOVERED BY WALKING services/venue-*, not listed here. A third venue adapter
// added tomorrow is checked without anyone remembering to add it, which is the
// whole point: the posture's justification is "every wired venue is spot", and a
// hand-written list would make that claim about the venues somebody remembered.
func venueMarginModes(t *testing.T, root string) map[string][]string {
	t.Helper()
	dirs, err := filepath.Glob(filepath.Join(root, "services", "venue-*"))
	if err != nil {
		t.Fatalf("glob venue services: %v", err)
	}
	out := map[string][]string{}
	for _, dir := range dirs {
		walkErr := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			fset := token.NewFileSet()
			// No parser.ParseComments: the connectors' own docs describe the modes
			// in prose ("CASH ONLY, AND THE WIRE IS WHY"), and a scan that could see
			// comments would be reading the explanation instead of the declaration.
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				return nil // not this guard's business; the build catches it
			}
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Name.Name != "MarginModes" || fn.Body == nil {
					continue
				}
				if fn.Recv == nil || len(fn.Recv.List) == 0 {
					continue
				}
				recv := receiverTypeName(fn.Recv.List[0].Type)
				var modes []string
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					sel, ok := n.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					if strings.HasPrefix(sel.Sel.Name, "MarginMode_") {
						modes = append(modes, sel.Sel.Name)
					}
					return true
				})
				sort.Strings(modes)
				out[recv] = modes
			}
			return nil
		})
		if walkErr != nil {
			t.Fatalf("walk %s: %v", dir, walkErr)
		}
	}
	return out
}

// settlementPendingPosture is the posture stated for ledger.SettlementPending,
// read off settlementBasisPostures' composite literal.
type settlementPendingPosture struct {
	found    bool
	produced bool
	// literal is true when `produced` is written as a bool LITERAL rather than an
	// expression. A posture computed from something is not a posture.
	literal bool
	why     string
	arm     string
}

// readPendingPosture parses settlement_basis_posture.go and returns what it says
// about ledger.SettlementPending.
func readPendingPosture(t *testing.T, root string) settlementPendingPosture {
	t.Helper()
	path := filepath.Join(root, settlementPostureFile)
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", settlementPostureFile, err)
	}

	var got settlementPendingPosture
	ast.Inspect(f, func(n ast.Node) bool {
		kv, ok := n.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		sel, ok := kv.Key.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "SettlementPending" {
			return true
		}
		lit, ok := kv.Value.(*ast.CompositeLit)
		if !ok {
			return true
		}
		got.found = true
		for _, el := range lit.Elts {
			field, ok := el.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			name, ok := field.Key.(*ast.Ident)
			if !ok {
				continue
			}
			switch name.Name {
			case "produced":
				if id, ok := field.Value.(*ast.Ident); ok && (id.Name == "true" || id.Name == "false") {
					got.literal = true
					got.produced = id.Name == "true"
				}
			case "why":
				got.why = concatStringLit(field.Value)
			case "arm":
				got.arm = concatStringLit(field.Value)
			}
		}
		return false
	})
	return got
}

// concatStringLit flattens a `"a" + "b" + "c"` chain of string literals. Anything
// else yields "", which every caller treats as unstated.
func concatStringLit(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return ""
		}
		s, err := strconv.Unquote(v.Value)
		if err != nil {
			return ""
		}
		return s
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return ""
		}
		return concatStringLit(v.X) + concatStringLit(v.Y)
	}
	return ""
}

// TestSettlementPostureAgreesWithTheVenueCapabilitiesThatJustifyIt is the arm
// that keeps the T+0 claim from outliving its evidence.
func TestSettlementPostureAgreesWithTheVenueCapabilitiesThatJustifyIt(t *testing.T) {
	root := moduleRoot(t)
	modes := venueMarginModes(t, root)

	// NON-VACUITY, BEFORE ANYTHING IS JUDGED. A moved connector tree or a renamed
	// method would leave this map empty, and an empty map satisfies "every venue
	// is spot-only" perfectly while checking nothing.
	if len(modes) < 2 {
		t.Fatalf("found MarginModes declarations on %d venue connector(s) (%v) — expected at "+
			"least the two this estate ships. The declarations moved and this guard is asserting "+
			"nothing about what justifies booking every fill as settled at execution", len(modes), modes)
	}

	var nonSpot []string
	for recv, declared := range modes {
		if len(declared) == 0 {
			// Declaring NOTHING is not declaring spot. An empty capability set means
			// the venue did not assert support, which is the platform's UNKNOWN, and
			// UNKNOWN cannot be the evidence for a T+0 assumption.
			nonSpot = append(nonSpot, recv+" declares no margin mode at all")
			continue
		}
		for _, m := range declared {
			if m != spotMarginMode {
				nonSpot = append(nonSpot, recv+" declares "+m)
			}
		}
	}
	sort.Strings(nonSpot)

	pending := readPendingPosture(t, root)
	if !pending.found {
		t.Fatalf("settlementBasisPostures in %s states nothing for ledger.SettlementPending.\n\n"+
			"That entry IS the posture: it is what puts "+
			"kanz_accounting_settlement_basis_produced{basis=\"pending\"} on /metrics at 0 and says "+
			"why. Without it the gauge reports a basis nobody described, and an operator reading a "+
			"book where every trade is final at the instant it matched has nothing to tell them so.",
			settlementPostureFile)
	}
	if !pending.literal {
		t.Errorf("the ledger.SettlementPending posture in %s does not write `produced` as a bool "+
			"literal.\n\nA posture computed from configuration is not a posture: nothing in this "+
			"estate can place an instrument that settles later than it executes, in any "+
			"deployment, so a false that depends on an environment variable would be claiming the "+
			"opposite is reachable by setting one.", settlementPostureFile)
	}

	switch {
	case len(nonSpot) == 0 && pending.produced:
		t.Errorf("%s says something produces ledger.SettlementPending entries, but every venue "+
			"connector declares spot only (%s).\n\nNothing in this estate can place an instrument "+
			"that settles later than it executes, so "+
			"kanz_accounting_settlement_basis_produced{basis=\"pending\"} would report 1 for a "+
			"producer that does not exist — a claim the estate makes about itself, which is worse "+
			"than the silence this posture replaced.", settlementPostureFile, spotMarginMode)

	case len(nonSpot) > 0 && !pending.produced:
		t.Errorf("A VENUE CONNECTOR NO LONGER DECLARES SPOT ONLY, and %s still says nothing "+
			"produces a traded-not-settled entry:\n\n  %s\n\n"+
			"The whole justification for booking every fill as settled at the instant it executed "+
			"is that every wired adapter is crypto spot, where the asset and the cash move in one "+
			"exchange transaction. A connector that can work a non-CASH margin regime is trading an "+
			"instrument family whose settlement is not atomic with the match, and ledger."+
			"fillSettlement is still returning SettlementSettled for it — so the book reports that "+
			"the fund owns, and may spend against, a position it has merely bought.\n\n"+
			"Re-derive the posture: ledger.fillSettlement must return SettlementPending with the "+
			"contractual date for that venue, and this entry must then say so.",
			settlementPostureFile, strings.Join(nonSpot, "\n  "))
	}

	if !pending.produced {
		if strings.TrimSpace(pending.why) == "" {
			t.Errorf("the ledger.SettlementPending posture in %s is unproduced with no `why`. An "+
				"entry that does not say why nothing produces it is a zero on a dashboard with no "+
				"sentence attached", settlementPostureFile)
		}
		if strings.TrimSpace(pending.arm) == "" {
			t.Errorf("the ledger.SettlementPending posture in %s is unproduced with no `arm`. "+
				"Every unwired posture in this service names what would arm it — without that this "+
				"is a permanent excuse rather than a tracked absence", settlementPostureFile)
		}
	}
}

// AND THE PROCESS MUST PUBLISH IT. The posture above exists only because the
// accounting composition root calls stateSettlementBasisPosture. Deleting that
// call leaves `go build`, `go vet` and the whole services/accounting/... suite
// green — the posture's own unit tests call the function directly, so they prove
// it is CORRECT without proving it is REACHED. That is the composition-root blind
// spot this repository has shipped crashes through twice.
//
// Asserted off the AST with comments excluded, because main.go's own comment
// names the function: a guard that grepped raw source would match the explanation
// and keep passing with the call deleted.
func TestAccountingCompositionRootStatesTheSettlementBasisPosture(t *testing.T) {
	const fn = "stateSettlementBasisPosture"
	path := filepath.Join(moduleRoot(t), settlementCompositionRoot)

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", settlementCompositionRoot, err)
	}

	var called bool
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == fn {
			called = true
		}
		return true
	})
	if !called {
		t.Fatalf("%s never calls %s().\n\n"+
			"Without it kanz_accounting_settlement_basis_produced is never registered, so every "+
			"series is ABSENT rather than zero — and an absent series answers an alert with \"no "+
			"data\", which is the same ambiguity moved out of the book and into the monitoring "+
			"system. The book then goes on booking every fill as settled at the instant it "+
			"executed with nothing anywhere saying that it does (#1043).",
			settlementCompositionRoot, fn)
	}
}

// AND THE GAUGE MAY NOT BE REGISTERED BEHIND A BRANCH.
//
// Collectors registered inside an `if producer != nil` make the broker-less
// deployment export NO series, so an `== 0` alert is silent in exactly the state
// it was written for. That has shipped twice in this estate in two consecutive
// changes and was caught both times only by running the binary. There is nothing
// to branch on here — the posture is a fact about the build, not the config — so
// the registration must sit at the top level of the function body.
func TestSettlementBasisGaugeIsRegisteredUnconditionally(t *testing.T) {
	const fn = "seedSettlementBasisPosture"
	path := filepath.Join(moduleRoot(t), settlementPostureFile)

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", settlementPostureFile, err)
	}

	var body *ast.BlockStmt
	for _, decl := range f.Decls {
		if d, ok := decl.(*ast.FuncDecl); ok && d.Name.Name == fn {
			body = d.Body
		}
	}
	if body == nil {
		t.Fatalf("%s no longer declares %s — this guard found nothing to check. If the seeding "+
			"moved, move this assertion with it", settlementPostureFile, fn)
	}

	// Anywhere in the function, to prove the call exists at all; and at the top
	// level, to prove nothing guards it. Both arms, because "not nested" is
	// satisfied vacuously by "not present".
	var anywhere bool
	ast.Inspect(body, func(n ast.Node) bool {
		if isMustRegisterCall(n) {
			anywhere = true
		}
		return true
	})
	if !anywhere {
		t.Fatalf("%s no longer calls MustRegister — the gauge is never registered and every "+
			"kanz_accounting_settlement_basis_produced series is absent", fn)
	}

	var topLevel bool
	for _, stmt := range body.List {
		expr, ok := stmt.(*ast.ExprStmt)
		if !ok {
			continue
		}
		if isMustRegisterCall(expr.X) {
			topLevel = true
		}
	}
	if !topLevel {
		t.Fatalf("%s registers the settlement-basis gauge from inside a nested statement rather "+
			"than at the top level of its body.\n\n"+
			"A collector registered behind a branch is exported by SOME deployments and not "+
			"others, and the ones that skip it report no series at all — so "+
			"`kanz_accounting_settlement_basis_produced{basis=\"pending\"} == 0` matches nothing "+
			"in precisely the deployment whose book settles everything at execution. Registered "+
			"absent is worse than registered zero.", fn)
	}
}

// isMustRegisterCall reports whether n is an `x.MustRegister(...)` call.
func isMustRegisterCall(n ast.Node) bool {
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "MustRegister"
}
