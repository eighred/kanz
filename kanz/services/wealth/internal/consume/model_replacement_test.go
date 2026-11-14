package consume

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/wealth"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	wealthpb "github.com/eighred/kanz/kanz-schemas-go/wealth/v1"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/wealth/internal/book"
	"github.com/eighred/kanz/services/wealth/internal/drift"
	"github.com/eighred/kanz/services/wealth/internal/server"
	"google.golang.org/protobuf/proto"
)

func replacementModel(id string, profile wealthpb.RiskProfile) *wealthpb.ModelPortfolio {
	return &wealthpb.ModelPortfolio{ModelId: id, RiskProfile: profile, ArithmeticVersion: 2,
		ExactTargetWeights:  map[string]string{"VTI": "0.600000000000000001", "BND": "0.399999999999999999"},
		ExactDriftTolerance: "0.05", RecordedBy: "operator:fixture", Reason: "approved allocation"}
}

func replacementEnvelope(tenant, id string) *envelopepb.Envelope {
	return &envelopepb.Envelope{TenantId: tenant, PartitionKey: id, EventType: wealth.EventTypeModelPublished}
}

func replacementFolder(t *testing.T, tenant string, r *wealth.ModelRegistry) *ModelFolder {
	t.Helper()
	f, err := NewModelFolder(tenant, r, nil)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func requireReplacementModel(t *testing.T, r *wealth.ModelRegistry, tenant string, profile wealth.RiskProfile, id string, targets map[string]string) {
	t.Helper()
	m, err := r.Model(tenant, profile)
	if err != nil || m.ModelID != id || len(m.Targets) != len(targets) {
		t.Fatalf("model %s/%s is unavailable or changed: %+v %v", tenant, id, m, err)
	}
	for key, want := range targets {
		if string(m.Targets[key]) != want {
			t.Fatalf("exact target %s=%s, want %s", key, m.Targets[key], want)
		}
	}
}

func TestModelDecoderRetainsOnlyAttributableUnknownProfileIdentity(t *testing.T) {
	for _, unknown := range []int32{-1, 99, 2147483647} {
		t.Run(fmt.Sprint(unknown), func(t *testing.T) {
			m, err := DecodeModelProto(modelPayload(t, replacementModel("model.identity", wealthpb.RiskProfile(unknown))))
			if err == nil || !strings.Contains(err.Error(), "risk_profile") || m.ModelID != "model.identity" || m.Profile != wealth.ProfileUnspecified || len(m.Targets) != 0 {
				t.Fatalf("unknown profile lost identity, was cast, or exposed usable policy: %+v %v", m, err)
			}
			if m.Validate() == nil {
				t.Fatal("a refused partial decode became an admissible model")
			}
		})
	}
}

func TestUnknownProfileReplacementInvalidatesEveryKnownProfile(t *testing.T) {
	for _, profile := range []wealthpb.RiskProfile{
		wealthpb.RiskProfile_RISK_PROFILE_CONSERVATIVE, wealthpb.RiskProfile_RISK_PROFILE_MODERATE,
		wealthpb.RiskProfile_RISK_PROFILE_BALANCED, wealthpb.RiskProfile_RISK_PROFILE_GROWTH, wealthpb.RiskProfile_RISK_PROFILE_AGGRESSIVE,
	} {
		for _, unknown := range []int32{-1, 99, 2147483647} {
			t.Run(fmt.Sprintf("%s_%d", profile, unknown), func(t *testing.T) {
				r := wealth.NewModelRegistry()
				f := replacementFolder(t, "owner", r)
				env := replacementEnvelope("owner", "model")
				if err := f.Handle(t.Context(), env, modelPayload(t, replacementModel("model", profile))); err != nil {
					t.Fatal(err)
				}
				r.Arm()
				for range 2 { // Repeated poison must retain the superseded known profile.
					if err := f.Handle(t.Context(), env, modelPayload(t, replacementModel("model", wealthpb.RiskProfile(unknown)))); err == nil || !strings.Contains(err.Error(), "risk_profile") {
						t.Fatalf("unknown profile was not explicitly refused: %v", err)
					}
					if _, err := r.Model("owner", wealth.RiskProfile(profile)); !errors.Is(err, wealth.ErrModelRejected) || r.Count() != 0 {
						t.Fatalf("superseded targets remain measured or became unpublished: count=%d err=%v", r.Count(), err)
					}
					if rejected := r.Rejections(); len(rejected) != 1 || !strings.Contains(rejected[0], "risk_profile") {
						t.Fatalf("duplicate poison lost its attributable refusal: %v", rejected)
					}
				}
			})
		}
	}
}

func TestModelReplacementIdentityCannotPoisonOrRepairAnotherResource(t *testing.T) {
	type input struct {
		env     *envelopepb.Envelope
		model   *wealthpb.ModelPortfolio
		payload []byte
	}
	cases := []struct {
		name string
		edit func(*input)
	}{
		{"foreign_tenant", func(i *input) { i.env.TenantId = "other" }},
		{"missing_tenant", func(i *input) { i.env.TenantId = "" }},
		{"missing_key", func(i *input) { i.env.PartitionKey = "" }},
		{"foreign_key", func(i *input) { i.env.PartitionKey = "another" }},
		{"subject_is_not_plain_key", func(i *input) { i.env.PartitionKey = wealth.SubjectModelFor("owner", "victim.model") }},
		{"escaped_key_is_not_plain_key", func(i *input) { i.env.PartitionKey = "victim%2Emodel" }},
		{"conflicting_payload_identity", func(i *input) { i.model.ModelId = "another" }},
		{"missing_payload_identity", func(i *input) { i.model.ModelId = "" }},
		{"blank_payload_identity", func(i *input) { i.model.ModelId = " "; i.env.PartitionKey = " " }},
		{"unbounded_payload_identity", func(i *input) { i.model.ModelId = strings.Repeat("x", 257); i.env.PartitionKey = i.model.ModelId }},
		{"wrong_event_type", func(i *input) { i.env.EventType = wealth.EventTypeHouseholdValued }},
		{"malformed_payload", func(i *input) { i.payload = []byte{0xff} }},
		{"malformed_after_decoded_identity", func(i *input) { i.payload = append(modelPayload(t, i.model), 0xff) }},
		{"oversized_payload", func(i *input) { i.payload = bytes.Repeat([]byte{0}, 2<<20+1) }},
		{"valid_decode_wrong_key", func(i *input) {
			i.model.RiskProfile = wealthpb.RiskProfile_RISK_PROFILE_GROWTH
			i.model.ExactTargetWeights = map[string]string{"VTI": "1"}
			i.env.PartitionKey = "another"
		}},
		{"known_invalid_wrong_key", func(i *input) {
			i.model.RiskProfile = wealthpb.RiskProfile_RISK_PROFILE_GROWTH
			i.model.ExactTargetWeights = nil
			i.env.PartitionKey = "another"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := wealth.NewModelRegistry()
			f := replacementFolder(t, "owner", r)
			original := replacementModel("victim.model", wealthpb.RiskProfile_RISK_PROFILE_GROWTH)
			if err := f.Handle(t.Context(), replacementEnvelope("owner", original.ModelId), modelPayload(t, original)); err != nil {
				t.Fatal(err)
			}
			r.Arm()
			i := input{env: replacementEnvelope("owner", original.ModelId), model: replacementModel(original.ModelId, wealthpb.RiskProfile(99))}
			tc.edit(&i)
			if i.payload == nil {
				i.payload = modelPayload(t, i.model)
			}
			if err := f.Handle(t.Context(), i.env, i.payload); err == nil {
				t.Fatal("unattributable replacement was accepted")
			}
			if r.Count() != 1 || len(r.Rejections()) != 0 {
				t.Fatal("unattributable replacement mutated or poisoned the victim")
			}
			requireReplacementModel(t, r, "owner", wealth.ProfileGrowth, original.ModelId, original.ExactTargetWeights)
			// A conflicting valid repair must not clear an existing rejection either.
			poison := replacementModel(original.ModelId, wealthpb.RiskProfile(99))
			_ = f.Handle(t.Context(), replacementEnvelope("owner", original.ModelId), modelPayload(t, poison))
			before := r.Rejections()
			repair := replacementModel(original.ModelId, wealthpb.RiskProfile_RISK_PROFILE_GROWTH)
			if err := f.Handle(t.Context(), replacementEnvelope("owner", "another"), modelPayload(t, repair)); err == nil {
				t.Fatal("cross-key repair was accepted")
			}
			if !reflect.DeepEqual(before, r.Rejections()) {
				t.Fatal("cross-key repair cleared or rewrote a refusal")
			}
		})
	}
}

func TestModelReplacementRepairIsolationAmbiguityAndAliases(t *testing.T) {
	t.Run("valid_profile_change_resets_rejection_attribution", func(t *testing.T) {
		r := wealth.NewModelRegistry()
		f := replacementFolder(t, "owner", r)
		env := replacementEnvelope("owner", "model")
		for _, profile := range []wealthpb.RiskProfile{wealthpb.RiskProfile_RISK_PROFILE_GROWTH, wealthpb.RiskProfile_RISK_PROFILE_BALANCED} {
			m := replacementModel("model", profile)
			if err := f.Handle(t.Context(), env, modelPayload(t, m)); err != nil {
				t.Fatal(err)
			}
			r.Arm()
			_ = f.Handle(t.Context(), env, modelPayload(t, replacementModel("model", wealthpb.RiskProfile(99))))
			if _, err := r.Model("owner", wealth.RiskProfile(profile)); !errors.Is(err, wealth.ErrModelRejected) {
				t.Fatal(err)
			}
			if profile == wealthpb.RiskProfile_RISK_PROFILE_BALANCED {
				if _, err := r.Model("owner", wealth.ProfileGrowth); !errors.Is(err, wealth.ErrNoModelForProfile) {
					t.Fatalf("valid profile change left stale rejection: %v", err)
				}
			}
		}
	})
	t.Run("duplicate_poison_repeated_repair_and_exact_targets", func(t *testing.T) {
		r := wealth.NewModelRegistry()
		f := replacementFolder(t, "owner", r)
		m := replacementModel("model.with.dots", wealthpb.RiskProfile_RISK_PROFILE_GROWTH)
		env := replacementEnvelope("owner", m.ModelId)
		for range 3 {
			if err := f.Handle(t.Context(), env, modelPayload(t, m)); err != nil {
				t.Fatal(err)
			}
			r.Arm()
			requireReplacementModel(t, r, "owner", wealth.ProfileGrowth, m.ModelId, m.ExactTargetWeights)
			got, _ := r.Model("owner", wealth.ProfileGrowth)
			got.Targets["VTI"] = "1" // A read alias cannot change stored targets.
			requireReplacementModel(t, r, "owner", wealth.ProfileGrowth, m.ModelId, m.ExactTargetWeights)
			poison := proto.Clone(m).(*wealthpb.ModelPortfolio)
			poison.RiskProfile = wealthpb.RiskProfile(99)
			for range 2 {
				if err := f.Handle(t.Context(), env, modelPayload(t, poison)); err == nil {
					t.Fatal("poison was accepted")
				}
				if _, err := r.Model("owner", wealth.ProfileGrowth); !errors.Is(err, wealth.ErrModelRejected) {
					t.Fatalf("duplicate poison forgot the known profile: %v", err)
				}
			}
		}
		if err := f.Handle(t.Context(), env, modelPayload(t, m)); err != nil || len(r.Rejections()) != 0 {
			t.Fatalf("matching repair did not clear refusal: %v", err)
		}
	})
	t.Run("tenants_profiles_and_escaped_aliases_are_independent", func(t *testing.T) {
		r := wealth.NewModelRegistry()
		for _, tenant := range []string{"owner", "other"} {
			for _, spec := range []struct {
				id      string
				profile wealthpb.RiskProfile
			}{{"model.with.dots", wealthpb.RiskProfile_RISK_PROFILE_GROWTH}, {"model%2Ewith%2Edots", wealthpb.RiskProfile_RISK_PROFILE_BALANCED}} {
				f := replacementFolder(t, tenant, r)
				if err := f.Handle(t.Context(), replacementEnvelope(tenant, spec.id), modelPayload(t, replacementModel(spec.id, spec.profile))); err != nil {
					t.Fatal(err)
				}
			}
		}
		r.Arm()
		f := replacementFolder(t, "owner", r)
		_ = f.Handle(t.Context(), replacementEnvelope("owner", "model.with.dots"), modelPayload(t, replacementModel("model.with.dots", wealthpb.RiskProfile(99))))
		if _, err := r.Model("owner", wealth.ProfileGrowth); !errors.Is(err, wealth.ErrModelRejected) {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			tenant, id string
			profile    wealth.RiskProfile
		}{{"owner", "model%2Ewith%2Edots", wealth.ProfileBalanced}, {"other", "model.with.dots", wealth.ProfileGrowth}, {"other", "model%2Ewith%2Edots", wealth.ProfileBalanced}} {
			requireReplacementModel(t, r, tc.tenant, tc.profile, tc.id, replacementModel(tc.id, 0).ExactTargetWeights)
		}
	})
	t.Run("poison_does_not_silently_resolve_ambiguity", func(t *testing.T) {
		r := wealth.NewModelRegistry()
		f := replacementFolder(t, "owner", r)
		for _, id := range []string{"one", "two"} {
			if err := f.Handle(t.Context(), replacementEnvelope("owner", id), modelPayload(t, replacementModel(id, wealthpb.RiskProfile_RISK_PROFILE_GROWTH))); err != nil {
				t.Fatal(err)
			}
		}
		r.Arm()
		if _, err := r.Model("owner", wealth.ProfileGrowth); !errors.Is(err, wealth.ErrModelProfileAmbiguous) {
			t.Fatal(err)
		}
		_ = f.Handle(t.Context(), replacementEnvelope("owner", "one"), modelPayload(t, replacementModel("one", wealthpb.RiskProfile(99))))
		if _, err := r.Model("owner", wealth.ProfileGrowth); !errors.Is(err, wealth.ErrModelRejected) {
			t.Fatalf("poison selected the other ambiguous model: %v", err)
		}
		if err := f.Handle(t.Context(), replacementEnvelope("owner", "one"), modelPayload(t, replacementModel("one", wealthpb.RiskProfile_RISK_PROFILE_GROWTH))); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Model("owner", wealth.ProfileGrowth); !errors.Is(err, wealth.ErrModelProfileAmbiguous) {
			t.Fatalf("repair silently resolved two retained claimants: %v", err)
		}
	})
}

func replacementHousehold() wealth.Household {
	return wealth.Household{HouseholdID: "replacement-household", RiskProfile: wealth.ProfileGrowth, CurrencyCode: "USD",
		AsOf: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), RecordedBy: "advisor:fixture", Reason: "administrator valuation",
		Accounts: []wealth.Account{{AccountID: "account", Cash: "0", Holdings: []wealth.Holding{
			{InstrumentID: "VTI", AssetClass: "EQUITY", MarketValue: "800"},
			{InstrumentID: "BND", AssetClass: "FIXED_INCOME", MarketValue: "200"},
		}}}}
}

func requireReplacementView(t *testing.T, tenant string, store book.Store, r *wealth.ModelRegistry, outcome drift.Outcome, measured bool) {
	t.Helper()
	monitor := drift.New(tenant, r, nil, nil)
	h, ok, err := store.Get(t.Context(), "replacement-household")
	if err != nil || !ok {
		t.Fatalf("household unavailable: %t %v", ok, err)
	}
	result := monitor.Evaluate(h)
	if result.Outcome != outcome || result.Evaluated != measured {
		t.Fatalf("monitor misstates superseded model state: %+v", result)
	}
	ready := &server.Readiness{}
	ready.Set(true)
	srv := server.New(ready, nil, tenant, store, server.WithDriftMonitor(monitor))
	req := httptest.NewRequest(http.MethodGet, "/v1/households/"+h.HouseholdID, nil)
	req.Header.Set(auth.HeaderPrincipalTenant, tenant)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("household read failed: %d %s", rec.Code, rec.Body)
	}
	var body struct {
		Total string         `json:"total_value"`
		Drift map[string]any `json:"drift"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Total != "1000" || body.Drift["outcome"] != string(outcome) || body.Drift["evaluated"] != measured {
		t.Fatalf("served household hid valuation or unreadability: %+v", body)
	}
	if !measured {
		for _, field := range []string{"max", "total", "by_instrument", "tolerance", "breached", "model_id"} {
			if _, present := body.Drift[field]; present {
				t.Fatalf("unmeasured household exposed fabricated %s: %v", field, body.Drift)
			}
		}
		return
	}
	// 800/1000 minus 0.600000000000000001 = 0.199999999999999999;
	// the BND leg is its negative, so total absolute drift is exactly twice it.
	if body.Drift["max"] != "0.199999999999999999" || body.Drift["total"] != "0.399999999999999998" || body.Drift["breached"] != true || body.Drift["tolerance"] != "0.05" {
		t.Fatalf("served drift lost exact targets: %v", body.Drift)
	}
	by, ok := body.Drift["by_instrument"].(map[string]any)
	if !ok || by["VTI"] != "0.199999999999999999" || by["BND"] != "-0.199999999999999999" {
		t.Fatalf("served instrument deviations are not exact: %v", by)
	}
}

func TestModelReplacementHouseholdDriftRefusalAndRepair(t *testing.T) {
	r := wealth.NewModelRegistry()
	f := replacementFolder(t, "owner", r)
	store := book.NewMemoryStore()
	if err := store.Put(t.Context(), replacementHousehold()); err != nil {
		t.Fatal(err)
	}
	env := replacementEnvelope("owner", "model")
	m := replacementModel("model", wealthpb.RiskProfile_RISK_PROFILE_GROWTH)
	if err := f.Handle(t.Context(), env, modelPayload(t, m)); err != nil {
		t.Fatal(err)
	}
	r.Arm()
	requireReplacementView(t, "owner", store, r, drift.OutcomeBreached, true)
	for range 2 {
		_ = f.Handle(t.Context(), env, modelPayload(t, replacementModel("model", wealthpb.RiskProfile(99))))
		requireReplacementView(t, "owner", store, r, drift.OutcomeInvalidModel, false)
		if err := f.Handle(t.Context(), env, modelPayload(t, m)); err != nil {
			t.Fatal(err)
		}
		requireReplacementView(t, "owner", store, r, drift.OutcomeBreached, true)
	}
}
