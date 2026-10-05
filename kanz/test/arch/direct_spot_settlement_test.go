package arch

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

// A cash margin mode alone does not establish atomic settlement: a cash equity
// venue can settle T+N. Only the two reviewed adapters justify today's build
// convention. New adapters, even cash-only ones, must trigger a domain review.
func directSpotAdapters(modes map[string][]string) error {
	if len(modes) != 2 {
		return fmt.Errorf("unreviewed adapter inventory: %v", modes)
	}
	for _, name := range []string{"BinanceVenue", "OKXVenue"} {
		declared := modes[name]
		if len(declared) != 1 || declared[0] != spotMarginMode {
			return fmt.Errorf("%s no longer declares exactly the reviewed spot regime: %v", name, declared)
		}
	}
	return nil
}

func TestDirectSpotSettlementApplicabilityHasReviewedEvidence(t *testing.T) {
	root := moduleRoot(t)
	if err := directSpotAdapters(venueMarginModes(t, root)); err != nil {
		t.Fatal(err)
	}
	pending := readPendingPosture(t, root)
	if !pending.found || !pending.literal || pending.produced {
		t.Fatal("OMS direct spot applicability disagrees with accounting's settlement producer")
	}
	file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, "services/oms/cmd/oms/settlement_posture.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var bound bool
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok || len(value.Names) != 1 || value.Names[0].Name != "wiredSettlementModel" || len(value.Values) != 1 {
				continue
			}
			id, ok := value.Values[0].(*ast.Ident)
			bound = ok && id.Name == "settlementDirectSpot"
		}
	}
	if !bound {
		t.Fatal("re-derive OMS and accounting's settlement convention together; no unreviewed applicability default")
	}
	// The actual accounting producer, not just its explanatory posture, must
	// still assert the convention whose applicability OMS reports.
	file, err = parser.ParseFile(token.NewFileSet(), filepath.Join(root, "services/accounting/internal/ledger/fill.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var producer bool
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "fillSettlement" || fn.Body == nil || len(fn.Body.List) != 1 {
			continue
		}
		ret, ok := fn.Body.List[0].(*ast.ReturnStmt)
		if !ok || len(ret.Results) != 2 {
			continue
		}
		basis, bOK := ret.Results[0].(*ast.Ident)
		date, dOK := ret.Results[1].(*ast.Ident)
		producer = bOK && dOK && basis.Name == "SettlementSettled" && date.Name == "executedAt"
	}
	if !producer {
		t.Fatal("accounting's settlement producer changed; re-derive OMS applicability")
	}
}

func TestDirectSpotApplicabilityRejectsUnknownAndNewCashVenues(t *testing.T) {
	for name, modes := range map[string]map[string][]string{
		"missing":                {},
		"unknown capability":     {"BinanceVenue": {}, "OKXVenue": {spotMarginMode}},
		"margin":                 {"BinanceVenue": {"MarginMode_MARGIN_MODE_CROSS"}, "OKXVenue": {spotMarginMode}},
		"new cash venue":         {"BinanceVenue": {spotMarginMode}, "OKXVenue": {spotMarginMode}, "CashEquityVenue": {spotMarginMode}},
		"replacement cash venue": {"CashEquityVenue": {spotMarginMode}, "OKXVenue": {spotMarginMode}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := directSpotAdapters(modes); err == nil {
				t.Fatal("unreviewed venue inherited atomic settlement")
			}
		})
	}
}
