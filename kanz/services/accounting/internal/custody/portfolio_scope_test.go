package custody

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"testing"

	"github.com/eighred/kanz/services/accounting/internal/recon"
)

func scopeOwner(t *testing.T, b Break, portfolio, custodian string) {
	t.Helper()
	v := reflect.ValueOf(b)
	for field, want := range map[string]string{"PortfolioID": portfolio, "CustodianID": custodian} {
		got := v.FieldByName(field)
		if !got.IsValid() || got.String() != want {
			t.Fatalf("%s owner missing/wrong: %+v", field, b)
		}
	}
}

func TestCustodyTypedPortfolioOwnership(t *testing.T) {
	s := subject()
	detected := detectedQuantityBreak(t0, 100, 90)
	t.Run("comparison_subject", func(t *testing.T) { scopeOwner(t, detected[0], s.PortfolioID, s.CustodianID) })
	t.Run("memory_opaque_ID_and_scoped_resolution", func(t *testing.T) {
		st := NewMemoryStore()
		detected := append([]Break(nil), detected...)
		detected[0].BreakID = BreakID("OTHER", "OTHER-C", recon.BreakQuantity, "opaque")
		if _, err := st.UpsertBreaks(t.Context(), s, detected, t0); err != nil {
			t.Fatal(err)
		}
		other := s
		other.PortfolioID = "OTHER"
		other.CustodianID = "OTHER-C"
		if _, err := st.UpsertBreaks(t.Context(), other, nil, t0); err != nil {
			t.Fatal(err)
		}
		b, err := st.LoadBreak(t.Context(), detected[0].BreakID)
		if err != nil || b.Status != BreakOpen {
			t.Fatalf("ID prefix resolved another portfolio: %+v %v", b, err)
		}
		scopeOwner(t, b, s.PortfolioID, s.CustodianID)
		if _, err := st.UpsertBreaks(t.Context(), s, nil, t0); err != nil {
			t.Fatal(err)
		}
		b, err = st.LoadBreak(t.Context(), detected[0].BreakID)
		if err != nil || b.Status != BreakResolved {
			t.Fatalf("owner did not resolve opaque ID: %+v %v", b, err)
		}
	})
	t.Run("immutable_completion_reconstruction", func(t *testing.T) {
		run := Run{RunID: "scope-run", Subject: s, Outcome: OutcomeBreaks, Breaks: detected, CompletedAt: t0}
		record, err := runRecord(testRunContext(t.Context()), "__system__", run)
		if err != nil {
			t.Fatal(err)
		}
		got, err := runFromRecord(record.Payload)
		if err != nil || len(got.Breaks) != 1 {
			t.Fatalf("completion: %+v %v", got, err)
		}
		scopeOwner(t, got.Breaks[0], s.PortfolioID, s.CustodianID)
	})
	t.Run("run_snapshot_subject", func(t *testing.T) {
		st := NewMemoryStore()
		b := detected[0]
		// A stored completion's outer subject owns its rows. The wire does not
		// carry a second, competing owner on each break.
		v := reflect.ValueOf(&b).Elem()
		for _, field := range []string{"PortfolioID", "CustodianID"} {
			if f := v.FieldByName(field); f.IsValid() {
				f.SetString("")
			}
		}
		if err := st.SaveRun(t.Context(), Run{RunID: "snapshot", Subject: s, Outcome: OutcomeBreaks, Breaks: []Break{b}, CompletedAt: t0}); err != nil {
			t.Fatal(err)
		}
		run, err := st.LatestRun(t.Context(), s.PortfolioID, s.CustodianID)
		if err != nil || len(run.Breaks) != 1 {
			t.Fatalf("snapshot: %+v %v", run, err)
		}
		scopeOwner(t, run.Breaks[0], s.PortfolioID, s.CustodianID)
	})
}

func TestCustodyActionHistoricalDigestExcludesCurrentScope(t *testing.T) {
	a := Action{Tenant: "T", Actor: "alice", RequestID: "R", BreakID: "opaque", Kind: "claim", ExpectedRevision: 1}
	// This is the pre-1713 canonical decision, including its original field order.
	legacy := []byte(`{"Tenant":"T","Actor":"alice","RequestID":"R","BreakID":"opaque","Kind":"claim","Explanation":"","ExpectedRevision":1}`)
	sum := sha256.Sum256(legacy)
	want := hex.EncodeToString(sum[:])
	for _, scope := range [][]string{nil, {}, {"PF-A"}, {"PF-B", "PF-A"}} {
		name := "absent"
		if scope != nil {
			name = "explicit_" + string(rune('0'+len(scope)))
		}
		t.Run(name, func(t *testing.T) {
			v := reflect.ValueOf(&a).Elem().FieldByName("PortfolioScope")
			if v.IsValid() {
				v.Set(reflect.ValueOf(scope))
			}
			if got := a.digest(); got != want {
				t.Fatalf("historical digest changed: %s want %s", got, want)
			}
		})
	}
}
