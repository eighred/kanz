package consume

import (
	"context"
	"google.golang.org/protobuf/types/known/timestamppb"
	"strings"
	"testing"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	wealthpb "github.com/eighred/kanz/kanz-schemas-go/wealth/v1"
	"google.golang.org/protobuf/proto"

	"github.com/eighred/kanz/internal/wealth"
	"github.com/eighred/kanz/services/wealth/internal/book"
)

func modelPayload(t *testing.T, m *wealthpb.ModelPortfolio) []byte {
	t.Helper()
	b, err := proto.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func wireModel() *wealthpb.ModelPortfolio {
	return &wealthpb.ModelPortfolio{
		ModelId:            "growth-2026",
		RiskProfile:        wealthpb.RiskProfile_RISK_PROFILE_GROWTH,
		ExactTargetWeights: map[string]string{"BTC-USD": "0.6", "ETH-USD": "0.4"},
		ArithmeticVersion:  2, ExactDriftTolerance: "0.05",
		RecordedBy: "operator:akif",
		Reason:     "IC review",
	}
}

// THE MODEL CROSSES THE WIRE COMPLETE. Every field the catalogue's admission rule
// reads must survive the decode: a dropped drift_tolerance is a zero band (every
// household permanently breached), a dropped profile serves nobody, and dropped
// provenance leaves an overwrite on a compacted subject attributable to nobody.
func TestDecodeModelProtoCarriesEveryFieldTheCatalogueNeeds(t *testing.T) {
	got, err := DecodeModelProto(modelPayload(t, wireModel()))
	if err != nil {
		t.Fatalf("DecodeModelProto: %v", err)
	}
	if got.ModelID != "growth-2026" || got.Profile != wealth.ProfileGrowth {
		t.Errorf("identity/profile lost: %+v", got)
	}
	if got.Tolerance != "0.05" {
		t.Errorf("drift_tolerance = %v, want 0.05. Dropped, it decodes to 0 and Breached compares "+
			"max > 0, so every household holding anything is permanently breached", got.Tolerance)
	}
	if got.RecordedBy != "operator:akif" || got.Reason != "IC review" {
		t.Errorf("provenance lost: %+v — this subject is compacted, so the model this one replaced "+
			"is gone and these are the only record of who changed it", got)
	}
	if len(got.Targets) != 2 || got.Targets["BTC-USD"] != "0.6" {
		t.Errorf("targets = %v", got.Targets)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("a well-formed wire model does not survive the catalogue's own rule: %v", err)
	}
}

// A PROFILE THIS BUILD DOES NOT KNOW IS REFUSED, NOT CAST. A numeric cast would
// admit a model onto a domain profile nothing can select, and the model would sit
// in the catalogue looking published while serving nobody.
func TestDecodeRefusesARiskProfileThisBuildDoesNotKnow(t *testing.T) {
	m := wireModel()
	m.RiskProfile = wealthpb.RiskProfile(99)
	if _, err := DecodeModelProto(modelPayload(t, m)); err == nil {
		t.Fatal("an unknown risk profile was decoded into a model. It would be admitted to the " +
			"catalogue and matched to no household, forever")
	}

	hv := &wealthpb.HouseholdValued{CurrencyCode: "USD", AsOf: &timestamppb.Timestamp{Seconds: 1700000000}, RecordedBy: "test:advisor", Reason: "test valuation", HouseholdId: "hh-1", RiskProfile: wealthpb.RiskProfile(99)}
	b, err := proto.Marshal(hv)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	_, err = DecodeProto(b)
	if err == nil {
		t.Fatal("an unknown risk profile was folded into a household. Its drift would report " +
			"no_model forever, which is indistinguishable from a firm that published no targets")
	}
	if !strings.Contains(err.Error(), "risk_profile") {
		t.Errorf("the refusal does not name the field: %v", err)
	}
}

// THE HOUSEHOLD'S RISK PROFILE SURVIVES THE VALUATION DECODE. Before #1010 this
// field had no producer and no path into the domain at all, so SelectModel's only
// input did not exist in the running system.
func TestDecodeProtoCarriesTheRiskProfile(t *testing.T) {
	hv := &wealthpb.HouseholdValued{CurrencyCode: "USD", AsOf: &timestamppb.Timestamp{Seconds: 1700000000}, RecordedBy: "test:advisor", Reason: "test valuation", HouseholdId: "hh-1", RiskProfile: wealthpb.RiskProfile_RISK_PROFILE_BALANCED}
	b, err := proto.Marshal(hv)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, err := DecodeProto(b)
	if err != nil {
		t.Fatalf("DecodeProto: %v", err)
	}
	if got.RiskProfile != wealth.ProfileBalanced {
		t.Errorf("RiskProfile = %v, want BALANCED. Dropped here, the household has no target "+
			"allocation and nothing measures its drift", got.RiskProfile)
	}
}

// THE CATALOGUE FOLD IS TENANT-SCOPED. A model folded from another tenant's
// envelope becomes THIS tenant's target allocation for every household on that
// profile — the #223 cross-tenant fold, applied to allocations.
func TestModelFolderRefusesAnotherTenantsModel(t *testing.T) {
	r := wealth.NewModelRegistry()
	f, err := NewModelFolder("acme", r, nil)
	if err != nil {
		t.Fatalf("NewModelFolder: %v", err)
	}
	env := &envelopepb.Envelope{TenantId: "zenith", EventType: wealth.EventTypeModelPublished}
	if err := f.Handle(context.Background(), env, modelPayload(t, wireModel())); err == nil {
		t.Fatal("another tenant's model was admitted to this tenant's catalogue")
	}
	if r.Count() != 0 {
		t.Errorf("the refused model still landed (%d resident)", r.Count())
	}
}

// A MODEL THE CATALOGUE REFUSES MUST DLQ, not ack. On a compacted subject the
// failing message is the LAST one on its subject and is redelivered on every boot,
// so a silent ack leaves the risk profile with no target allocation and no record
// anywhere of the message meant to supply one.
func TestModelFolderReturnsAnErrorSoARefusedModelDLQs(t *testing.T) {
	m := wireModel()
	m.ExactDriftTolerance = "0"
	f, err := NewModelFolder("acme", wealth.NewModelRegistry(), nil)
	if err != nil {
		t.Fatalf("NewModelFolder: %v", err)
	}
	env := &envelopepb.Envelope{TenantId: "acme", EventType: wealth.EventTypeModelPublished}
	if err := f.Handle(context.Background(), env, modelPayload(t, m)); err == nil {
		t.Fatal("a model with a zero drift band was acked. Every household on its profile would " +
			"read as permanently breached, and the message would never be inspectable")
	}
}

// countingObserver records what the fold handed it.
type countingObserver struct{ seen []wealth.Household }

func (c *countingObserver) Observe(h wealth.Household) { c.seen = append(c.seen, h) }

// THE FOLD EVALUATES DRIFT ON THE FACT THAT CHANGED THE BOOK. That trigger is the
// whole capability: without it nothing asks whether a household has drifted except
// a human remembering to look.
func TestFolderObservesDriftAfterTheBookIsWritten(t *testing.T) {
	obs := &countingObserver{}
	store := book.NewMemoryStore()
	f, err := NewFolder("acme", store, DecodeProto, WithDriftObserver(obs))
	if err != nil {
		t.Fatalf("NewFolder: %v", err)
	}
	hv := &wealthpb.HouseholdValued{CurrencyCode: "USD", AsOf: &timestamppb.Timestamp{Seconds: 1700000000}, RecordedBy: "test:advisor", Reason: "test valuation", HouseholdId: "hh-1", RiskProfile: wealthpb.RiskProfile_RISK_PROFILE_GROWTH}
	b, err := proto.Marshal(hv)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	env := &envelopepb.Envelope{TenantId: "acme", EventType: wealth.EventTypeHouseholdValued}
	if err := f.Handle(context.Background(), env, b); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(obs.seen) != 1 {
		t.Fatalf("the fold evaluated drift %d times for one valuation, want 1 — a household can "+
			"drift past its band with nothing asking", len(obs.seen))
	}
	if obs.seen[0].RiskProfile != wealth.ProfileGrowth {
		t.Errorf("the observer was handed a household with profile %v — without the profile no "+
			"model can be selected", obs.seen[0].RiskProfile)
	}
	if _, ok, _ := store.Get(context.Background(), "hh-1"); !ok {
		t.Error("the household was not written to the book")
	}
}

// A HOUSEHOLD WHOSE DRIFT CANNOT BE COMPUTED IS STILL FOLDED. The book is the
// primary record; trading a missing drift number for a missing household would be
// strictly worse, and a nil observer must not change the fold's behaviour at all.
func TestFolderWithNoObserverStillFolds(t *testing.T) {
	store := book.NewMemoryStore()
	f, err := NewFolder("acme", store, DecodeProto)
	if err != nil {
		t.Fatalf("NewFolder: %v", err)
	}
	hv := &wealthpb.HouseholdValued{CurrencyCode: "USD", AsOf: &timestamppb.Timestamp{Seconds: 1700000000}, RecordedBy: "test:advisor", Reason: "test valuation", HouseholdId: "hh-1"}
	b, err := proto.Marshal(hv)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	env := &envelopepb.Envelope{TenantId: "acme", EventType: wealth.EventTypeHouseholdValued}
	if err := f.Handle(context.Background(), env, b); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if _, ok, _ := store.Get(context.Background(), "hh-1"); !ok {
		t.Error("a fold with no drift observer stopped writing the book")
	}
}

func TestLegacyModelReplacementFailsClosed(t *testing.T) {
	r := wealth.NewModelRegistry()
	f, err := NewModelFolder("acme", r, nil)
	if err != nil {
		t.Fatal(err)
	}
	env := &envelopepb.Envelope{TenantId: "acme", EventType: wealth.EventTypeModelPublished}
	if err := f.Handle(context.Background(), env, modelPayload(t, wireModel())); err != nil {
		t.Fatal(err)
	}
	r.Arm()
	m := wireModel()
	m.ArithmeticVersion = 0
	m.ExactTargetWeights = nil
	m.ExactDriftTolerance = ""
	m.TargetWeights = map[string]float64{"BTC-USD": 0.6, "ETH-USD": 0.4}
	m.DriftTolerance = 0.05
	if err := f.Handle(context.Background(), env, modelPayload(t, m)); err == nil {
		t.Fatal("legacy model admitted")
	}
	if _, err := r.Model("acme", wealth.ProfileGrowth); err == nil {
		t.Fatal("old model survives rejected compacted replacement")
	}
}
