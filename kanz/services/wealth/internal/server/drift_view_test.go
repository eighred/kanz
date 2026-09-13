package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/wealth"
	"github.com/eighred/kanz/services/wealth/internal/book"
	"github.com/eighred/kanz/services/wealth/internal/drift"
)

// seedProfiled puts a household holding 800 VTI / 200 BND on the given profile —
// 80/20 by weight, measured below against a 60/40 model.
func seedProfiled(t *testing.T, store book.Store, profile wealth.RiskProfile) {
	t.Helper()
	h := wealth.Household{
		HouseholdID: "HH-D", CurrencyCode: "USD", AsOf: time.Unix(1700000000, 0), RecordedBy: "test:advisor", Reason: "test valuation",
		RiskProfile: profile,
		Accounts: []wealth.Account{{AccountID: "A1", Cash: "0", Holdings: []wealth.Holding{
			{InstrumentID: "VTI", AssetClass: "EQUITY", MarketValue: "800"},
			{InstrumentID: "BND", AssetClass: "FIXED_INCOME", MarketValue: "200"},
		}}},
	}
	if err := store.Put(context.Background(), h); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func armedMonitor(t *testing.T) *drift.Monitor {
	t.Helper()
	r := wealth.NewModelRegistry()
	err := r.Put(testTenant, wealth.ModelPortfolio{
		ModelID:    "balanced-2026",
		Profile:    wealth.ProfileBalanced,
		Targets:    map[string]wealth.Number{"VTI": "0.6", "BND": "0.4"},
		Tolerance:  "0.05",
		RecordedBy: "operator:akif",
		Reason:     "IC review",
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	r.Arm()
	return drift.New(testTenant, r, nil, nil)
}

func getHousehold(t *testing.T, s *Server) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, asTenant("GET", "/v1/households/HH-D", testTenant))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body
}

// THE HOUSEHOLD VIEW ANSWERS WHETHER THE BOOK MATCHES ITS TARGET. Before #1010 it
// reported holdings, weights and asset-class exposure and said nothing about the
// allocation the household is supposed to hold — the drift half of internal/wealth
// was reachable from nowhere, including this surface.
func TestHouseholdViewReportsDriftFromTheModel(t *testing.T) {
	store := book.NewMemoryStore()
	r := &Readiness{}
	r.Set(true)
	s := New(r, nil, testTenant, store, WithDriftMonitor(armedMonitor(t)))
	seedProfiled(t, store, wealth.ProfileBalanced)

	body := getHousehold(t, s)
	if got := body["risk_profile"]; got != "BALANCED" {
		t.Errorf("risk_profile = %v, want the named profile (an ordinal is something an operator "+
			"has to go look up)", got)
	}
	d, ok := body["drift"].(map[string]any)
	if !ok {
		t.Fatalf("no drift block on the household view: %v", body["drift"])
	}
	if d["evaluated"] != true {
		t.Fatalf("drift.evaluated = %v on a household with an armed model: %v", d["evaluated"], d)
	}
	if d["model_id"] != "balanced-2026" {
		t.Errorf("drift.model_id = %v — the view does not say WHICH target it was measured against", d["model_id"])
	}
	// 800/1000 = 0.80 against a 0.60 target ⇒ +0.20, four times the 0.05 band.
	if got, _ := d["max"].(string); got != "0.2" {
		t.Errorf("drift.max = %v, want 0.20", got)
	}
	if d["breached"] != true {
		t.Errorf("drift.breached = %v on a book 20 points off a 5-point band", d["breached"])
	}
}

// AN UNEVALUATED HOUSEHOLD SAYS SO, RATHER THAN OMITTING THE BLOCK OR REPORTING
// ZERO. Both of those read to a human as "this book matches its model" — the exact
// confusion between "not measured" and "measured, and fine" that left the whole
// capability dark.
func TestHouseholdViewNeverPresentsAnUnmeasuredBookAsInBand(t *testing.T) {
	cases := []struct {
		name    string
		monitor DriftEvaluator
		profile wealth.RiskProfile
		want    drift.Outcome
	}{
		{"no catalogue wired at all", nil, wealth.ProfileBalanced, drift.OutcomeCatalogueUnarmed},
		{"household asserts no profile", armedMonitor(t), wealth.ProfileUnspecified, drift.OutcomeNoProfile},
		{"no model for this profile", armedMonitor(t), wealth.ProfileAggressive, drift.OutcomeNoModel},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := book.NewMemoryStore()
			r := &Readiness{}
			r.Set(true)
			opts := []Option{}
			if tc.monitor != nil {
				opts = append(opts, WithDriftMonitor(tc.monitor))
			}
			s := New(r, nil, testTenant, store, opts...)
			seedProfiled(t, store, tc.profile)

			d, ok := getHousehold(t, s)["drift"].(map[string]any)
			if !ok {
				t.Fatal("the drift key is absent. A missing block reads as 'this book matches its " +
					"model'; an unmeasured household must say so explicitly")
			}
			if d["evaluated"] != false {
				t.Errorf("drift.evaluated = %v on an unmeasured household", d["evaluated"])
			}
			if got := d["outcome"]; got != string(tc.want) {
				t.Errorf("drift.outcome = %v, want %q", got, tc.want)
			}
			if _, present := d["max"]; present {
				t.Error("an unmeasured household carries a drift number. A 0 here is the absence of " +
					"a measurement, and nothing on the surface would say so")
			}
			if d["reason"] == "" || d["reason"] == nil {
				t.Error("no reason — an advisor sees 'not evaluated' with nothing to act on")
			}
		})
	}
}
