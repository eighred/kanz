package arch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The feed remains dark under #588. Its processing contract must nevertheless
// retain revision identity and keep income away from the generic cash fold.
func TestCorporateActionLifecycleBoundary(t *testing.T) {
	read := func(path string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(moduleRoot(t), filepath.FromSlash(path)))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	domain := read("services/accounting/internal/corpact/corpact.go")
	if !strings.Contains(domain, "ledger.ActionEntryID(c.PortfolioID, c.ActionID, c.Revision)") || !strings.Contains(domain, "ledger.ValidateActionEntry(e)") {
		t.Fatal("corporate actions require validated portfolio/action/revision identity (#1038)")
	}
	fold := read("services/accounting/internal/ledger/ledger.go")
	start := strings.Index(fold, "func (f basisFold) foldCorpAct(")
	if start < 0 {
		t.Fatal("corporate-action fold missing")
	}
	end := strings.Index(fold[start:], "// Replay")
	if start < 0 || end < 0 {
		t.Fatal("corporate-action fold boundary missing")
	}
	if strings.Contains(fold[start:start+end], "case CorpActDividend") || strings.Contains(fold[start:start+end], "case CorpActCoupon") {
		t.Fatal("income must use ex-date accrual and confirmed-payment lifecycle, not generic cash fold (#1038)")
	}
	lifecycle := read("services/accounting/internal/ledger/corporate_action.go")
	for _, required := range []string{"if e.actionStage == 2", "add(b.Accrued", "a.PaidAt.IsZero()", "heads[[2]string{e.PortfolioID, a.ActionID}]"} {
		if !strings.Contains(lifecycle, required) {
			t.Errorf("missing lifecycle boundary %q", required)
		}
	}
}
