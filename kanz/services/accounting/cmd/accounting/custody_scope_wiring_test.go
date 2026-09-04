package main

// A MULTI-CUSTODIAN MISCONFIGURATION MUST NOT REACH A RUNNING POD (#1006).
//
// The book side of the custody comparison used to load the whole portfolio
// regardless of the custodian on the subject, so a portfolio custodied in two
// places reported every position held at the other as MISSING_AT_CUSTODIAN — the
// entire book, twice, burying the one break that means a fill never reached the
// ledger.
//
// The repair scopes the book to the custodian's exchange accounts, which needs a
// declaration. A declaration that is MISSING is the defect arriving as a config
// change, so it is refused at STARTUP rather than discovered as a break queue
// somebody has to interpret. The composition root is where that refusal lives,
// and wiring escapes every unit test in the packages beneath it — so it is
// asserted here, on the function main actually calls.

import (
	"log/slog"
	"strings"
	"testing"
)

// The refusal this whole change exists to make impossible to skip.
func TestTwoCustodiansWithoutAnAccountDeclarationRefusesToStart(t *testing.T) {
	cfg := baseCfg()
	cfg.CustodyPairs = []string{"PF1:CUST-A", "PF1:CUST-B"}

	plane, err := buildPlane(t, cfg, &capturingHandler{})
	if err == nil {
		t.Fatalf("the plane started with two custodians and no account declaration (plane=%v).\n\n"+
			"Every run would compare the WHOLE portfolio book against ONE custodian's statement, so "+
			"each custodian's run reports every position held at the other as MISSING_AT_CUSTODIAN. "+
			"The control would be running, green, and asserting a wrong answer.", plane != nil)
	}
	for _, want := range []string{"PF1", "CUST-A", "ACCOUNTING_CUSTODY_ACCOUNTS"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q, so an operator cannot act on it: %v", want, err)
		}
	}
}

// The same configuration, correctly declared, starts and schedules both pairs.
func TestTwoCustodiansWithAnAccountDeclarationStarts(t *testing.T) {
	cfg := baseCfg()
	cfg.CustodyPairs = []string{"PF1:CUST-A", "PF1:CUST-B"}
	cfg.CustodyAccounts = []string{"PF1:CUST-A:okx-sub-1", "PF1:CUST-B:bin-main"}

	plane, err := buildPlane(t, cfg, &capturingHandler{})
	if err != nil {
		t.Fatalf("a correctly declared two-custodian portfolio was refused: %v", err)
	}
	if plane.scheduler == nil {
		t.Fatal("no scheduler was armed — nothing would reconcile on any cadence")
	}
	if got := plane.scheduler.Pairs(); len(got) != 2 {
		t.Fatalf("pairs = %+v, want both custodians scheduled", got)
	}
}

// A single-custodian portfolio needs no declaration and does not move — the whole
// book against its one custodian is correct, and every existing deployment is
// this shape.
func TestASingleCustodianPortfolioStartsWithNoDeclaration(t *testing.T) {
	h := &capturingHandler{}
	plane, err := buildPlane(t, baseCfg(), h)
	if err != nil {
		t.Fatalf("a single-custodian portfolio was refused: %v", err)
	}
	if plane.scheduler == nil {
		t.Fatal("no scheduler was armed")
	}
	// "One custodian, whole book" is CORRECT and "several custodians, whole book"
	// is the defect. From outside the process they look identical, so the correct
	// case is stated out loud rather than left as an absence.
	if !h.sawAt(slog.LevelInfo, "compares the whole portfolio book") {
		t.Error("the plane did not record WHY it compares the whole book. An operator reading the log " +
			"cannot then tell a correct single-custodian scope from the unscoped defect")
	}
}

// A declaration that names something the pairs do not is a typo that would scope
// nothing, and it must not start either.
func TestAnAccountDeclarationForAnUnconfiguredPairRefusesToStart(t *testing.T) {
	for _, tc := range []struct {
		name     string
		accounts []string
		wantIn   string
	}{
		{"unknown portfolio", []string{"PF9:CUST-A:okx-sub-1"}, "not in ACCOUNTING_CUSTODY_PAIRS"},
		{"unknown custodian", []string{"PF1:CUST-Z:okx-sub-1"}, "not a configured pair"},
		{"malformed entry", []string{"PF1:CUST-A"}, "is not"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseCfg()
			cfg.CustodyAccounts = tc.accounts
			if _, err := buildPlane(t, cfg, &capturingHandler{}); err == nil {
				t.Fatal("the plane started on a declaration that scopes nothing")
			} else if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("refusal does not say why (want %q): %v", tc.wantIn, err)
			}
		})
	}
}
