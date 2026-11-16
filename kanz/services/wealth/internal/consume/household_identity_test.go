package consume

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/eighred/kanz/internal/wealth"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	wealthpb "github.com/eighred/kanz/kanz-schemas-go/wealth/v1"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/wealth/internal/book"
	"github.com/eighred/kanz/services/wealth/internal/drift"
	"github.com/eighred/kanz/services/wealth/internal/server"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func identityHousehold(id string) *wealthpb.HouseholdValued {
	h := replacementHousehold()
	return &wealthpb.HouseholdValued{HouseholdId: id, CurrencyCode: h.CurrencyCode,
		AsOf: timestamppb.New(h.AsOf), RecordedBy: h.RecordedBy, Reason: h.Reason,
		RiskProfile: wealthpb.RiskProfile_RISK_PROFILE_GROWTH,
		Accounts: []*wealthpb.ValuedAccount{{AccountId: "account", Cash: &commonpb.Decimal{}, Holdings: []*wealthpb.ValuedHolding{
			{InstrumentId: "VTI", AssetClass: "EQUITY", MarketValue: &commonpb.Decimal{Coefficient: 800}},
			{InstrumentId: "BND", AssetClass: "FIXED_INCOME", MarketValue: &commonpb.Decimal{Coefficient: 200}},
		}}}}
}

func identityEnvelope(tenant, id string) *envelopepb.Envelope {
	return &envelopepb.Envelope{TenantId: tenant, EventType: wealth.EventTypeHouseholdValued,
		EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, PartitionKey: id}
}

func identityPayload(t *testing.T, h *wealthpb.HouseholdValued) []byte {
	t.Helper()
	b, err := proto.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type identityInput struct {
	env     *envelopepb.Envelope
	h       *wealthpb.HouseholdValued
	payload []byte
}

func invalidHouseholdIdentities() []struct {
	name string
	edit func(*identityInput)
} {
	return []struct {
		name string
		edit func(*identityInput)
	}{
		{"nil_envelope", func(i *identityInput) { i.env = nil }},
		{"foreign_tenant", func(i *identityInput) { i.env.TenantId = "foreign" }},
		{"missing_tenant", func(i *identityInput) { i.env.TenantId = "" }},
		{"wrong_type", func(i *identityInput) { i.env.EventType = wealth.EventTypeModelPublished }},
		{"missing_type", func(i *identityInput) { i.env.EventType = "" }},
		{"observation", func(i *identityInput) { i.env.EventClass = envelopepb.EventClass_EVENT_CLASS_OBSERVATION }},
		{"command", func(i *identityInput) { i.env.EventClass = envelopepb.EventClass_EVENT_CLASS_COMMAND }},
		{"unspecified_class", func(i *identityInput) { i.env.EventClass = envelopepb.EventClass_EVENT_CLASS_UNSPECIFIED }},
		{"missing_key", func(i *identityInput) { i.env.PartitionKey = "" }},
		{"sibling_key", func(i *identityInput) { i.env.PartitionKey = "sibling" }},
		{"subject_key", func(i *identityInput) {
			i.env.PartitionKey = wealth.SubjectHouseholdFor(i.env.TenantId, i.h.HouseholdId)
		}},
		{"escaped_key", func(i *identityInput) {
			i.h.HouseholdId = "house.with.dots"
			i.env.PartitionKey = "house%2Ewith%2Edots"
		}},
		{"conflicting_payload", func(i *identityInput) { i.h.HouseholdId = "sibling" }},
		{"missing_identity", func(i *identityInput) { i.h.HouseholdId = "" }},
		{"blank_identity", func(i *identityInput) { i.h.HouseholdId = " "; i.env.PartitionKey = " " }},
		{"unbounded_identity", func(i *identityInput) {
			i.h.HouseholdId = strings.Repeat("x", 257)
			i.env.PartitionKey = i.h.HouseholdId
		}},
		{"malformed_payload", func(i *identityInput) { i.payload = []byte{0xff} }},
		{"oversized_payload", func(i *identityInput) { i.payload = bytes.Repeat([]byte{0}, 8<<20+1) }},
	}
}

type identityStore struct {
	book.Store
	puts int
	err  error
}

func (s *identityStore) Put(ctx context.Context, h wealth.Household) error {
	s.puts++
	if s.err != nil {
		return s.err
	}
	return s.Store.Put(ctx, h)
}

func requireIdentityHousehold(t *testing.T, store book.Store, want wealth.Household) {
	t.Helper()
	got, ok, err := store.Get(t.Context(), want.HouseholdID)
	if err != nil || !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("household %q changed or disappeared: got=%+v ok=%t err=%v want=%+v", want.HouseholdID, got, ok, err, want)
	}
}

func TestHouseholdIdentityRefusesBeforeWriteAndDrift(t *testing.T) {
	for _, tc := range invalidHouseholdIdentities() {
		t.Run(tc.name, func(t *testing.T) {
			store := &identityStore{Store: book.NewMemoryStore()}
			a, b := replacementHousehold(), replacementHousehold()
			b.HouseholdID, b.Accounts[0].Cash = "sibling", "25"
			for _, h := range []wealth.Household{a, b} {
				if err := store.Store.Put(t.Context(), h); err != nil {
					t.Fatal(err)
				}
			}
			observer := &countingObserver{}
			folder, err := NewFolder("owner", store, DecodeProto, WithDriftObserver(observer))
			if err != nil {
				t.Fatal(err)
			}
			i := identityInput{env: identityEnvelope("owner", a.HouseholdID), h: identityHousehold(a.HouseholdID)}
			i.h.Accounts[0].Cash.Coefficient = 999
			tc.edit(&i)
			if i.payload == nil {
				i.payload = identityPayload(t, i.h)
			}
			if err := folder.Handle(t.Context(), i.env, i.payload); err == nil {
				t.Fatal("invalid resource attribution was accepted")
			}
			if store.puts != 0 || len(observer.seen) != 0 {
				t.Fatalf("refused frame reached effects: puts=%d drift=%d", store.puts, len(observer.seen))
			}
			requireIdentityHousehold(t, store, a)
			requireIdentityHousehold(t, store, b)
		})
	}
}

func TestHouseholdIdentityLiteralKeysRemainDistinct(t *testing.T) {
	store := book.NewMemoryStore()
	observer := &countingObserver{}
	folder, err := NewFolder("owner", store, DecodeProto, WithDriftObserver(observer))
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{"plain", "with.dots", "with%2Edots", "with space", "with/slash", "家庭", strings.Repeat("x", 256)}
	for _, id := range ids {
		t.Run(id, func(t *testing.T) {
			h := identityHousehold(id)
			if err := folder.Handle(t.Context(), identityEnvelope("owner", id), identityPayload(t, h)); err != nil {
				t.Fatal(err)
			}
			want := replacementHousehold()
			want.HouseholdID = id
			requireIdentityHousehold(t, store, want)
		})
	}
	if len(observer.seen) != len(ids) {
		t.Fatal("a canonical literal key was lost or aliased")
	}
}

func TestHouseholdIdentityKeepsExactValuesDuplicatesAndReplacement(t *testing.T) {
	for _, tc := range []struct {
		name   string
		amount *commonpb.Decimal
		want   string
	}{
		{"zero", &commonpb.Decimal{}, "0"},
		{"negative", &commonpb.Decimal{Coefficient: -25}, "-25"},
		{"fraction", &commonpb.Decimal{Coefficient: 1234567890123456789, Exponent: -18}, "1.234567890123456789"},
		{"tiny", &commonpb.Decimal{Coefficient: 1, Exponent: -64}, "1e-64"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := book.NewMemoryStore()
			observer := &countingObserver{}
			folder, err := NewFolder("owner", store, DecodeProto, WithDriftObserver(observer))
			if err != nil {
				t.Fatal(err)
			}
			h := identityHousehold("exact")
			h.Accounts[0].Cash = tc.amount
			wire := identityPayload(t, h)
			before := bytes.Clone(wire)
			for range 2 {
				if err := folder.Handle(t.Context(), identityEnvelope("owner", "exact"), wire); err != nil {
					t.Fatal(err)
				}
			}
			got, ok, err := store.Get(t.Context(), "exact")
			if err != nil || !ok {
				t.Fatal(err)
			}
			cash, err := got.Accounts[0].Cash.Rat()
			want, exact := new(big.Rat).SetString(tc.want)
			if err != nil || !exact || cash.Cmp(want) != 0 || !bytes.Equal(wire, before) || len(observer.seen) != 2 {
				t.Fatal("exact value, duplicate handling, input bytes or observation changed", err)
			}
			// A full replacement may remove accounts; it must not merge old legs.
			h.Accounts = nil
			if err := folder.Handle(t.Context(), identityEnvelope("owner", "exact"), identityPayload(t, h)); err != nil {
				t.Fatal(err)
			}
			got, ok, err = store.Get(t.Context(), "exact")
			if err != nil || !ok || len(got.Accounts) != 0 || len(observer.seen) != 3 {
				t.Fatal("full replacement retained old legs", err)
			}
		})
	}
}

func TestHouseholdIdentityStoreErrorCannotObserveDrift(t *testing.T) {
	failure := errors.New("durable fixture refusal")
	store := &identityStore{Store: book.NewMemoryStore(), err: failure}
	observer := &countingObserver{}
	folder, err := NewFolder("owner", store, DecodeProto, WithDriftObserver(observer))
	if err != nil {
		t.Fatal(err)
	}
	err = folder.Handle(t.Context(), identityEnvelope("owner", "absent"), identityPayload(t, identityHousehold("absent")))
	if !errors.Is(err, failure) || store.puts != 1 || len(observer.seen) != 0 {
		t.Fatal("store refusal became a derived observation", err)
	}
}

func requireIdentityView(t *testing.T, tenant, id string, store book.Store, repaired bool) {
	t.Helper()
	r := wealth.NewModelRegistry()
	model := replacementModel("identity-target", wealthpb.RiskProfile_RISK_PROFILE_GROWTH)
	if err := replacementFolder(t, tenant, r).Handle(t.Context(), replacementEnvelope(tenant, model.ModelId), modelPayload(t, model)); err != nil {
		t.Fatal(err)
	}
	r.Arm()
	ready := &server.Readiness{}
	ready.Set(true)
	srv := server.New(ready, nil, tenant, store, server.WithDriftMonitor(drift.New(tenant, r, nil, nil)))
	for _, version := range []string{"v1", "v2"} {
		req := httptest.NewRequest(http.MethodGet, "/"+version+"/households/"+url.PathEscape(id), nil)
		req.Header.Set(auth.HeaderPrincipalTenant, tenant)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("actual household view: %d %s", rec.Code, rec.Body)
		}
		var got struct {
			ID      string            `json:"household_id"`
			Total   string            `json:"total_value"`
			Weights map[string]string `json:"weights"`
			Drift   struct {
				Evaluated bool   `json:"evaluated"`
				Max       string `json:"max"`
				Total     string `json:"total"`
			} `json:"drift"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		// Independent exact allocations: 800/1000=.8, 200/1000=.2;
		// after repair 1200/2000=.6, 800/2000=.4. Policy differs by 1e-18.
		wantTotal, wantVTI, wantBND, wantMax, wantDrift := "1000", "0.8", "0.2", "0.199999999999999999", "0.399999999999999998"
		if repaired {
			wantTotal, wantVTI, wantBND, wantMax, wantDrift = "2000", "0.6", "0.4", "0.000000000000000001", "0.000000000000000002"
		}
		if got.ID != id || got.Total != wantTotal || got.Weights["VTI"] != wantVTI || got.Weights["BND"] != wantBND || !got.Drift.Evaluated || got.Drift.Max != wantMax || got.Drift.Total != wantDrift {
			t.Fatalf("served exact identity/value/drift mismatch: %+v", got)
		}
		foreign := httptest.NewRequest(http.MethodGet, "/"+version+"/households/"+url.PathEscape(id), nil)
		foreign.Header.Set(auth.HeaderPrincipalTenant, tenant+"-foreign")
		denied := httptest.NewRecorder()
		srv.ServeHTTP(denied, foreign)
		if denied.Code != http.StatusNotFound || strings.Contains(denied.Body.String(), id) {
			t.Fatal("household resource was discoverable by a foreign tenant")
		}
	}
}

func TestHouseholdIdentityServedRefusalAndMatchingRepair(t *testing.T) {
	store := book.NewMemoryStore()
	folder, err := NewFolder("owner", store, DecodeProto)
	if err != nil {
		t.Fatal(err)
	}
	h := identityHousehold("served")
	if err := folder.Handle(t.Context(), identityEnvelope("owner", h.HouseholdId), identityPayload(t, h)); err != nil {
		t.Fatal(err)
	}
	h.Accounts[0].Holdings[0].MarketValue.Coefficient = 1200
	h.Accounts[0].Holdings[1].MarketValue.Coefficient = 800
	if err := folder.Handle(t.Context(), identityEnvelope("owner", "different"), identityPayload(t, h)); err == nil {
		t.Fatal("mismatched key overwrote the served household")
	}
	requireIdentityView(t, "owner", "served", store, false)
	if err := folder.Handle(t.Context(), identityEnvelope("owner", h.HouseholdId), identityPayload(t, h)); err != nil {
		t.Fatal(err)
	}
	requireIdentityView(t, "owner", "served", store, true)
}
