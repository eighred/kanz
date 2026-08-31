package compliance

import (
	"context"
	"testing"
	"time"

	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	lifecyclepb "github.com/eighred/kanz/kanz-schemas-go/lifecycle/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// mustPut files a mandate and fails the test if the registry refuses it. Put
// returns an error now: a mandate missing tenant_id or portfolio_id cannot be
// keyed by (tenant, portfolio) and dropping it silently would read downstream as
// "this portfolio is ungoverned" (#243).
func mustPut(t *testing.T, reg *MandateRegistry, m *compliancepb.Mandate) {
	t.Helper()
	if err := reg.Put(m); err != nil {
		t.Fatalf("registry refused a well-formed mandate: %v", err)
	}
}

func versioned(version uint64, effective time.Time) *compliancepb.Mandate {
	return &compliancepb.Mandate{
		MandateId: "m1", TenantId: "t1", PortfolioId: "p1", Version: version,
		EffectiveAt: timestamppb.New(effective),
	}
}

// TestMandateRegistry_PointInTimeResolution pins the SELECTION ORDER: the latest
// version whose effective_at is at or before the query time, and the newest of
// all for a zero asOf.
//
// THE CLOCK IS PINNED BEFORE EITHER EFFECTIVE DATE, and that is what makes this
// still a resolution test after #884. Put now drops versions no query at or after
// the retention instant can choose (retainSelectable); with the registry's clock
// standing before v1, BOTH versions are scheduled-not-yet-in-force, nothing is
// prunable, and the table below asks exactly what it always asked. Run it against
// the wall clock instead and it would be asserting that a superseded version is
// still resident — which is the leak, not the behaviour.
func TestMandateRegistry_PointInTimeResolution(t *testing.T) {
	reg := NewMandateRegistry()
	v1Eff := t0
	v2Eff := t0.Add(10 * 24 * time.Hour)
	reg.now = func() time.Time { return v1Eff.Add(-time.Hour) }
	mustPut(t, reg, versioned(1, v1Eff))
	mustPut(t, reg, versioned(2, v2Eff))

	cases := []struct {
		name    string
		asOf    time.Time
		wantOK  bool
		wantVer uint64
	}{
		{"before any version", t0.Add(-time.Hour), false, 0},
		{"during v1", t0.Add(5 * 24 * time.Hour), true, 1},
		{"on v2 effective", v2Eff, true, 2},
		{"during v2", t0.Add(15 * 24 * time.Hour), true, 2},
		{"zero asOf ⇒ latest", time.Time{}, true, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, ok, err := reg.Mandate(context.Background(), "t1", "p1", tc.asOf)
			if err != nil {
				t.Fatal(err)
			}
			if ok != tc.wantOK {
				t.Fatalf("ok: want %v got %v", tc.wantOK, ok)
			}
			if ok && m.GetVersion() != tc.wantVer {
				t.Fatalf("version: want %d got %d", tc.wantVer, m.GetVersion())
			}
		})
	}
}

func TestMandateRegistry_IdempotentReplay(t *testing.T) {
	reg := NewMandateRegistry()
	mustPut(t, reg, versioned(1, t0))
	mustPut(t, reg, versioned(1, t0)) // replay same version
	m, ok, _ := reg.Mandate(context.Background(), "t1", "p1", t0)
	if !ok || m.GetVersion() != 1 {
		t.Fatalf("idempotent replay broke resolution: ok=%v m=%v", ok, m)
	}
}

func TestMandateCodec_RoundTripAndKey(t *testing.T) {
	m := concentrationMandate(60)
	s, err := MarshalMandateValue(m)
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnmarshalMandateValue(s)
	if err != nil {
		t.Fatal(err)
	}
	if got.GetVersion() != m.GetVersion() || len(got.GetRules()) != 1 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if want := "compliance.mandate.t1/p1"; MandateConfigKey("t1", "p1") != want {
		t.Fatalf("config key: want %s got %s", want, MandateConfigKey("t1", "p1"))
	}
}

// TestMandateRegistry_ArmedGating pins the readiness-gating contract Arm/Armed
// exist for: a fresh registry must report unarmed (so a caller must not treat
// it as having replayed the mandates in force yet — the fail-open default
// means an unarmed registry answering as if it were caught up is precisely the
// EXEC-M13 window: an OMS or compliance pod that reports Ready before the
// mandate replay has folded admits every order for every portfolio as
// unconstrained), armed once Arm is called, and idempotent under repeat calls
// since the bus may invoke the ready callback more than once across
// resubscribes.
func TestMandateRegistry_ArmedGating(t *testing.T) {
	reg := NewMandateRegistry()
	if reg.Armed() {
		t.Fatal("a freshly constructed registry must be unarmed — it has not replayed anything yet")
	}
	reg.Arm()
	if !reg.Armed() {
		t.Fatal("Arm() must make Armed() report true")
	}
	reg.Arm() // idempotent: a second call must not panic or change the outcome
	if !reg.Armed() {
		t.Fatal("a repeat Arm() call must not un-arm the registry")
	}
}

func TestMandateLoader_AppliesOnlyMandateKeys(t *testing.T) {
	reg := NewMandateRegistry()
	loader := NewMandateLoader(reg)

	// Non-mandate config is ignored.
	m, err := loader.Apply(&lifecyclepb.ConfigChanged{ConfigKey: "risk.var.confidence", NewValue: "0.99"})
	if err != nil || m != nil {
		t.Fatalf("non-mandate key should be ignored, got m=%v err=%v", m, err)
	}

	val, _ := MarshalMandateValue(concentrationMandate(60))
	applied, err := loader.Apply(&lifecyclepb.ConfigChanged{
		ConfigKey: MandateConfigKey("t1", "p1"), NewValue: val,
	})
	if err != nil || applied == nil {
		t.Fatalf("mandate key should apply: m=%v err=%v", applied, err)
	}
	if _, ok, _ := reg.Mandate(context.Background(), "t1", "p1", t0); !ok {
		t.Fatalf("applied mandate not resolvable")
	}
}
