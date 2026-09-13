package wealth

import (
	"errors"
	"strings"
	"testing"
)

func growth() ModelPortfolio {
	return ModelPortfolio{
		ModelID:    "growth-2026",
		Profile:    ProfileGrowth,
		Targets:    map[string]Number{"BTC-USD": "0.6", "ETH-USD": "0.4"},
		Tolerance:  "0.05",
		RecordedBy: "operator:akif",
		Reason:     "IC review",
	}
}

// THE THREE ABSENCES ARE THREE DIFFERENT ANSWERS. This is the whole reason the
// registry returns sentinel errors instead of (ModelPortfolio, bool): "the replay
// has not landed", "the firm published no model for this profile" and "the
// household asserts no profile" are three different people's problems, and
// collapsing them is how a rolling restart reads as a firm with no targets.
func TestModelLookupDistinguishesUnarmedFromEmptyFromNoProfile(t *testing.T) {
	r := NewModelRegistry()

	if _, err := r.Model("acme", ProfileGrowth); !errors.Is(err, ErrModelCatalogueUnarmed) {
		t.Fatalf("an UNARMED catalogue answered %v, want ErrModelCatalogueUnarmed. Treating an "+
			"unfinished replay as an empty catalogue is EXEC-M13: every household reports no target "+
			"allocation for the first seconds of every rolling restart", err)
	}

	r.Arm()
	_, err := r.Model("acme", ProfileGrowth)
	if !errors.Is(err, ErrNoModelForProfile) {
		t.Fatalf("an ARMED, empty catalogue answered %v, want ErrNoModelForProfile", err)
	}
	if errors.Is(err, ErrModelCatalogueUnarmed) {
		t.Error("an armed, empty catalogue still reports itself unarmed — the two states are now " +
			"indistinguishable, which is the confusion this test exists to prevent")
	}

	// A household with no profile is not a statement about the catalogue.
	_, err = r.Model("acme", ProfileUnspecified)
	if !errors.Is(err, ErrNoModelForProfile) {
		t.Errorf("an unspecified profile answered %v", err)
	}
	if !strings.Contains(err.Error(), "asserts no risk profile") {
		t.Errorf("the refusal for an unspecified profile does not say the HOUSEHOLD is the problem: %v", err)
	}
}

func TestModelResolvesThePublishedModelForAProfile(t *testing.T) {
	r := NewModelRegistry()
	if err := r.Put("acme", growth()); err != nil {
		t.Fatalf("Put: %v", err)
	}
	r.Arm()

	got, err := r.Model("acme", ProfileGrowth)
	if err != nil {
		t.Fatalf("Model: %v", err)
	}
	if got.ModelID != "growth-2026" || got.Tolerance != "0.05" {
		t.Errorf("resolved %+v, want the published growth model with its own band", got)
	}
	// A profile nobody published a model for stays a miss, even with a catalogue
	// that holds models — otherwise one model would silently serve every profile.
	if _, err := r.Model("acme", ProfileConservative); !errors.Is(err, ErrNoModelForProfile) {
		t.Errorf("CONSERVATIVE resolved to something: %v", err)
	}
}

// TENANT ISOLATION AT THE CATALOGUE. model_id is an operator-chosen string, so two
// tenants naming a model "growth" is a coincidence; keyed by model id alone one
// tenant's published targets would become the other's on the next replay (#243's
// shape, applied to allocations).
func TestModelIsScopedToItsTenant(t *testing.T) {
	r := NewModelRegistry()
	acme := growth()
	other := growth()
	other.Targets = map[string]Number{"BTC-USD": "1.0"}
	if err := r.Put("acme", acme); err != nil {
		t.Fatalf("Put acme: %v", err)
	}
	if err := r.Put("zenith", other); err != nil {
		t.Fatalf("Put zenith: %v", err)
	}
	r.Arm()

	got, err := r.Model("zenith", ProfileGrowth)
	if err != nil {
		t.Fatalf("Model zenith: %v", err)
	}
	if len(got.Targets) != 1 {
		t.Errorf("zenith resolved acme's model (%d targets) — one tenant's target allocation is "+
			"governing another's households", len(got.Targets))
	}
	if _, err := r.Model("nobody", ProfileGrowth); !errors.Is(err, ErrNoModelForProfile) {
		t.Errorf("a tenant with no models resolved one: %v", err)
	}
}

// A REPUBLISH REPLACES, IT DOES NOT ACCUMULATE. The subject is compacted on
// model_id, so the registry must be too — otherwise a model edited twice would
// look like two models claiming one profile and the profile would go unresolvable.
func TestPutReplacesAModelWithTheSameID(t *testing.T) {
	r := NewModelRegistry()
	if err := r.Put("acme", growth()); err != nil {
		t.Fatalf("Put: %v", err)
	}
	edited := growth()
	edited.Tolerance = "0.02"
	if err := r.Put("acme", edited); err != nil {
		t.Fatalf("Put again: %v", err)
	}
	r.Arm()

	if n := r.Count(); n != 1 {
		t.Fatalf("catalogue holds %d models after republishing one id, want 1", n)
	}
	got, err := r.Model("acme", ProfileGrowth)
	if err != nil {
		t.Fatalf("Model: %v", err)
	}
	if got.Tolerance != "0.02" {
		t.Errorf("tolerance = %v, want the republished 0.02 — the catalogue kept the superseded model", got.Tolerance)
	}
}

// TWO MODELS, ONE PROFILE, FAIL CLOSED. SelectModel resolves by first match, so
// with two resident the answer would be whichever the compacted replay delivered
// first — different per pod and per restart. A household measured against an
// arbitrary one of two allocations is worse than one not measured at all, because
// the number looks authoritative.
func TestTwoModelsClaimingOneProfileMakeItUnresolvable(t *testing.T) {
	r := NewModelRegistry()
	first := growth()
	second := growth()
	second.ModelID = "growth-2025"
	if err := r.Put("acme", first); err != nil {
		t.Fatalf("Put first: %v", err)
	}
	if err := r.Put("acme", second); err != nil {
		t.Fatalf("Put second: %v — an ambiguous catalogue must be ADMITTED and refused at "+
			"resolution, not refused at Put: the second model is a legitimate message on its own "+
			"compacted subject", err)
	}
	r.Arm()

	_, err := r.Model("acme", ProfileGrowth)
	if !errors.Is(err, ErrModelProfileAmbiguous) {
		t.Fatalf("two models claiming GROWTH resolved to %v. One of them is now silently the target "+
			"allocation for every household on that profile, chosen by map iteration order", err)
	}
	if !strings.Contains(err.Error(), "growth-2025") || !strings.Contains(err.Error(), "growth-2026") {
		t.Errorf("the refusal does not name both models, so an operator cannot tell which subject to "+
			"purge: %v", err)
	}
	// The other profiles are unaffected — the failure is scoped to the ambiguous
	// profile, not to the catalogue.
	balanced := growth()
	balanced.ModelID, balanced.Profile = "balanced-2026", ProfileBalanced
	if err := r.Put("acme", balanced); err != nil {
		t.Fatalf("Put balanced: %v", err)
	}
	if _, err := r.Model("acme", ProfileBalanced); err != nil {
		t.Errorf("an ambiguity on GROWTH broke BALANCED too: %v", err)
	}
}

// EVERY VALIDATION RULE EXISTS BECAUSE BREAKING IT PRODUCES A CONFIDENTLY WRONG
// DRIFT RATHER THAN AN ERROR. A refused model is also RECORDED, because on a
// compacted subject the failing message is redelivered on every boot and the
// catalogue must be able to say "unreadable" rather than "unpublished".
func TestPutRefusesAndRecordsAModelThatWouldProduceWrongDrift(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*ModelPortfolio)
		wantErr string
	}{
		{"zero tolerance", func(m *ModelPortfolio) { m.Tolerance = "0" }, "drift_tolerance"},
		{"tolerance above 1", func(m *ModelPortfolio) { m.Tolerance = "1.5" }, "drift_tolerance"},
		{"unspecified profile", func(m *ModelPortfolio) { m.Profile = ProfileUnspecified }, "risk_profile"},
		{"weights sum to 0.9", func(m *ModelPortfolio) { m.Targets = map[string]Number{"BTC-USD": "0.9"} }, "sum to"},
		{"no targets", func(m *ModelPortfolio) { m.Targets = nil }, "target_weights"},
		{"no model id", func(m *ModelPortfolio) { m.ModelID = "" }, "model_id"},
		{"no recorded_by", func(m *ModelPortfolio) { m.RecordedBy = "" }, "recorded_by"},
		{"no reason", func(m *ModelPortfolio) { m.Reason = "" }, "reason"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := growth()
			tc.mutate(&m)
			r := NewModelRegistry()
			err := r.Put("acme", m)
			if err == nil {
				t.Fatalf("the catalogue admitted a model with %s. Every household on its profile "+
					"would then be measured against it", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("the refusal does not name %q, so an operator cannot tell what to fix: %v", tc.wantErr, err)
			}
			r.Arm()
			if got := r.Rejections(); len(got) != 1 {
				t.Fatalf("Rejections() = %v, want the refused model recorded. On a COMPACTED subject "+
					"the failing message is redelivered on every boot, so a catalogue that forgets it "+
					"cannot tell 'this model is unreadable' from 'nobody published one'", got)
			}
			want := ErrModelRejected
			if m.Profile != ProfileGrowth {
				want = ErrNoModelForProfile
			}
			if _, err := r.Model("acme", ProfileGrowth); !errors.Is(err, want) {
				t.Errorf("a refused model still resolved: %v", err)
			}
		})
	}
}

// A GOOD REPUBLISH CLEARS THE REJECTION. Otherwise the operator fixes the file,
// republishes, and the catalogue keeps reporting the model as unreadable forever.
func TestPutClearsAPriorRejection(t *testing.T) {
	r := NewModelRegistry()
	bad := growth()
	bad.Tolerance = "0"
	if err := r.Put("acme", bad); err == nil {
		t.Fatal("the bad model was admitted")
	}
	if err := r.Put("acme", growth()); err != nil {
		t.Fatalf("Put fixed model: %v", err)
	}
	if got := r.Rejections(); len(got) != 0 {
		t.Errorf("Rejections() = %v after the model was fixed and republished", got)
	}
}

// AN UNTENANTED MODEL IS REFUSED. The catalogue is keyed per tenant; admitting one
// with no tenant would serve it to every tenant at once.
func TestPutRefusesAnUntenantedModel(t *testing.T) {
	if err := NewModelRegistry().Put("", growth()); err == nil {
		t.Fatal("a model with no tenant was admitted to the catalogue")
	}
}

func TestRejectedReplacementInvalidatesPreviousModel(t *testing.T) {
	r := NewModelRegistry()
	m := growth()
	if err := r.Put("acme", m); err != nil {
		t.Fatal(err)
	}
	r.Arm()
	m.Targets["BTC-USD"] = "0.9"
	got, err := r.Model("acme", ProfileGrowth)
	if err != nil || got.Targets["BTC-USD"] != "0.6" {
		t.Fatal("input map alias", got, err)
	}
	got.Targets["BTC-USD"] = "0.1"
	got, err = r.Model("acme", ProfileGrowth)
	if err != nil || got.Targets["BTC-USD"] != "0.6" {
		t.Fatal("output map alias", got, err)
	}
	m.LegacyPrecision = true
	if err := r.Put("acme", m); !errors.Is(err, ErrLegacyPrecision) {
		t.Fatal(err)
	}
	if _, err := r.Model("acme", ProfileGrowth); !errors.Is(err, ErrModelRejected) {
		t.Fatal("superseded model remains active", err)
	}
	if r.Count() != 0 {
		t.Fatal("rejected model counted active")
	}
	if err := r.Put("acme", growth()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Model("acme", ProfileGrowth); err != nil {
		t.Fatal(err)
	}
}
