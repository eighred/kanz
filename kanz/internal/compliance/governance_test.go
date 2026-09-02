package compliance

// Governance verdict tests (#926).
//
// The point of the verdict is that two states an operator acts on DIFFERENTLY
// used to increment one counter and print one sentence. So each test below names
// which state it is proving distinguishable, and the registry cases build the
// real shapes rather than asserting the enum against itself.

import (
	"context"
	"testing"
	"time"

	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
)

// THE #916 SHAPE, AND THE REASON THIS ISSUE EXISTS.
//
// Scheduling a mandate change evicted the version in force from the compacted
// stream, so a replica booted holding ONLY the future-dated version. The registry
// was Armed and Complete — the message applied cleanly — and the portfolio
// resolved as ungoverned. Every health signal was green and the only trace was a
// counter that reads exactly like a portfolio nobody has mandated yet.
func TestAFutureDatedOnlyMandateReportsLapsedNotNeverMandated(t *testing.T) {
	reg := NewMandateRegistry()
	future := t0.Add(30 * 24 * time.Hour)
	reg.now = func() time.Time { return t0 }
	mustPut(t, reg, versioned(7, future))

	m, gov, err := reg.Mandate(context.Background(), "t1", "p1", t0)
	if err != nil {
		t.Fatal(err)
	}
	if m != nil {
		t.Fatalf("a future-dated version was returned as in force: %v", m)
	}
	if gov != MandateLapsed {
		t.Fatalf("governance = %s, want mandate_lapsed.\n\n"+
			"This portfolio HAS a published mandate and none of its versions is in force — the "+
			"#916 shape. Reported as never_mandated it reads like onboarding, which is how #916 "+
			"went unnoticed while orders were admitted with no constraints (#926).", gov)
	}
	if !gov.NoMandate() {
		t.Fatal("a lapsed mandate did not report as ungoverned — the ADMISSION decision is the same " +
			"for both states, and only the diagnosis differs")
	}
}

// THE BENIGN ONE, asserted so the pair is genuinely discriminated rather than the
// registry answering "lapsed" to everything.
func TestAKeyWithNoVersionsReportsNeverMandated(t *testing.T) {
	reg := NewMandateRegistry()
	m, gov, err := reg.Mandate(context.Background(), "t1", "nobody-mandated-this", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if m != nil {
		t.Fatalf("a mandate was returned for a key with no versions: %v", m)
	}
	if gov != NeverMandated {
		t.Fatalf("governance = %s, want never_mandated", gov)
	}
	if !gov.NoMandate() {
		t.Fatal("never_mandated did not report as ungoverned")
	}
}

// AND THE GOVERNED CASE, so a registry that answered ungoverned to everything
// would fail rather than satisfy both tests above.
func TestAnInForceMandateReportsGoverned(t *testing.T) {
	reg := NewMandateRegistry()
	reg.now = func() time.Time { return t0.Add(-time.Hour) }
	mustPut(t, reg, versioned(1, t0))

	m, gov, err := reg.Mandate(context.Background(), "t1", "p1", t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if gov != Governed {
		t.Fatalf("governance = %s, want governed", gov)
	}
	if m.GetVersion() != 1 {
		t.Fatalf("version = %d, want 1", m.GetVersion())
	}
	if gov.NoMandate() {
		t.Fatal("a governed portfolio reported as ungoverned")
	}
}

// THE ZERO VALUE IS NOT A VERDICT, and it must read as ungoverned rather than as
// governed — a caller that forgot to set one must fail closed.
func TestTheZeroVerdictIsUngoverned(t *testing.T) {
	var g Governance
	if g != GovernanceUnspecified {
		t.Fatalf("the zero Governance is %s, want unspecified", g)
	}
	if !g.NoMandate() {
		t.Fatal("the zero Governance reported as GOVERNED — a caller that forgot to set a verdict " +
			"would admit orders with no constraints, which is the fail-open direction")
	}
}

// EVERY VERDICT HAS A DISTINCT LABEL, because they become metric label values and
// two verdicts sharing one string would re-collapse the distinction at the
// dashboard — which is where it is actually read.
func TestEveryVerdictRendersDistinctly(t *testing.T) {
	seen := map[string]Governance{}
	for _, g := range Governances() {
		s := g.String()
		if s == "" || s == "unspecified" {
			t.Fatalf("verdict %d renders as %q — it would be indistinguishable from an unset one", g, s)
		}
		if prev, dup := seen[s]; dup {
			t.Fatalf("verdicts %s and %s both render as %q — the distinction is lost at the metric label",
				prev, g, s)
		}
		seen[s] = g
	}
	if len(seen) != 3 {
		t.Fatalf("Governances() names %d verdicts, want 3 — a verdict missing from the list gets no "+
			"seeded metric series, so an alert over it queries an empty vector and never fires", len(seen))
	}
}

// THE GATE PASSES THE VERDICT TO THE OBSERVER, which is where it reaches a
// counter label. Without this the registry could tell the two apart and nothing
// downstream could.
func TestTheGateHandsTheVerdictToTheUngovernedObserver(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  MandateSource
		want Governance
	}{
		{"never mandated", verdictMandates{NeverMandated}, NeverMandated},
		{"lapsed", verdictMandates{MandateLapsed}, MandateLapsed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got Governance
			g := NewPreTradeGate(NewEngine(nil), MapBookSource{"p1": currentBook()}, tc.src, nil, nil, nil,
				WithUngovernedObserver(func(_, _ string, v Governance) { got = v }))
			d, err := g.Evaluate(context.Background(), OrderDelta{
				TenantID: "t1", PortfolioID: "p1", InstrumentID: "AAA",
			})
			if err != nil {
				t.Fatal(err)
			}
			if !d.Ungoverned {
				t.Fatal("the decision did not report Ungoverned")
			}
			if got != tc.want {
				t.Fatalf("observer received %s, want %s — the counter label is where an operator "+
					"sees which of the two states this is (#926)", got, tc.want)
			}
		})
	}
}

// verdictMandates is a source that answers one fixed verdict.
type verdictMandates struct{ v Governance }

func (m verdictMandates) Mandate(context.Context, string, string, time.Time) (*compliancepb.Mandate, Governance, error) {
	return nil, m.v, nil
}
