package consume

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	alt "github.com/eighred/kanz/internal/alternatives"
	altpb "github.com/eighred/kanz/kanz-schemas-go/alternatives/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/services/alternatives/internal/fund"
	"github.com/eighred/kanz/services/alternatives/internal/server"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type requiredInputKind struct {
	name, subject string
	typ           alt.EventType
}

func requiredInputKinds() []requiredInputKind {
	return []requiredInputKind{
		{"committed", alt.SubjectCommitted, alt.EventCommit},
		{"called", alt.SubjectCalled, alt.EventCall},
		{"distributed", alt.SubjectDistributed, alt.EventDistribution},
		{"marked", alt.SubjectMarked, alt.EventNAVMark},
	}
}

func (k requiredInputKind) commitment(id, book string) string {
	if k.typ == alt.EventCommit {
		return id
	}
	return book
}

func (k requiredInputKind) message(id, book string, amt *commonpb.Decimal, at *timestamppb.Timestamp) proto.Message {
	switch k.typ {
	case alt.EventCommit:
		return &altpb.Commitment{CommitmentId: id, FundId: "fund", InvestorId: "investor", CommittedAmount: amt,
			CommitmentDate: at, CurrencyCode: "USD", RecordedBy: "operator:fixture", Reason: "test notice"}
	case alt.EventCall:
		return &altpb.CapitalCall{CallId: id, CommitmentId: book, Amount: amt, CallDate: at, RecordedBy: "operator:fixture", Reason: "test notice"}
	case alt.EventDistribution:
		return &altpb.Distribution{DistributionId: id, CommitmentId: book, Amount: amt, DistributionDate: at,
			Kind: altpb.DistributionKind_DISTRIBUTION_KIND_RETURN_OF_CAPITAL, RecordedBy: "operator:fixture", Reason: "test notice"}
	default:
		return &altpb.NAVMark{MarkId: id, CommitmentId: book, Nav: amt, AsOf: at, RecordedBy: "operator:fixture", Reason: "test notice"}
	}
}

type requiredInputFault struct {
	name, errorField string
	amt              *commonpb.Decimal
	at               *timestamppb.Timestamp
}

func requiredInputFaults() []requiredInputFault {
	valid := timestamppb.New(time.Date(2026, 6, 1, 23, 59, 59, 0, time.UTC))
	amount := &commonpb.Decimal{Coefficient: 10125, Exponent: -2}
	return []requiredInputFault{
		{"missing_amount", "amount", nil, valid},
		{"missing_date", "date", amount, nil},
		{"negative_nanos", "date", amount, &timestamppb.Timestamp{Seconds: valid.Seconds, Nanos: -1}},
		{"overflow_nanos", "date", amount, &timestamppb.Timestamp{Seconds: valid.Seconds, Nanos: 1_000_000_000}},
		{"before_year_one", "date", amount, &timestamppb.Timestamp{Seconds: -62135596801}},
		{"after_year_9999", "date", amount, &timestamppb.Timestamp{Seconds: 253402300800}},
	}
}

func marshalRequiredInput(t *testing.T, msg proto.Message) []byte {
	t.Helper()
	b, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDecodeProtoRequiredLifecycleInputs(t *testing.T) {
	for _, kind := range requiredInputKinds() {
		for _, fault := range requiredInputFaults() {
			t.Run(kind.name+"_"+fault.name, func(t *testing.T) {
				body := marshalRequiredInput(t, kind.message("event", "book", fault.amt, fault.at))
				e, err := DecodeProto(kind.subject)(body)
				if err == nil || e != nil || !strings.Contains(err.Error(), fault.errorField) {
					t.Fatalf("required %s was converted into a financial event: event=%+v err=%v", fault.name, e, err)
				}
			})
		}
	}
}

func TestDecodeProtoRequiredInputCompatibility(t *testing.T) {
	for _, kind := range requiredInputKinds() {
		for _, tc := range []struct {
			name string
			amt  *commonpb.Decimal
			at   *timestamppb.Timestamp
			want string
		}{
			{"explicit_zero", &commonpb.Decimal{Exponent: 64}, timestamppb.New(t0()), "0"},
			{"negative_exact", &commonpb.Decimal{Coefficient: -125, Exponent: -2}, timestamppb.New(t0()), "-5/4"},
			{"positive_exact", &commonpb.Decimal{Coefficient: 1234567890123456789, Exponent: -18}, timestamppb.New(t0()), "1234567890123456789/1000000000000000000"},
			{"epoch", &commonpb.Decimal{Coefficient: 1}, &timestamppb.Timestamp{}, "1"},
			{"year_one", &commonpb.Decimal{Coefficient: 1}, &timestamppb.Timestamp{Seconds: -62135596800}, "1"},
			{"valid_nanos", &commonpb.Decimal{Coefficient: 1}, &timestamppb.Timestamp{Seconds: 1700000000, Nanos: 999999999}, "1"},
			{"last_valid_second", &commonpb.Decimal{Coefficient: 1}, &timestamppb.Timestamp{Seconds: 253402300799, Nanos: 999999999}, "1"},
		} {
			t.Run(kind.name+"_"+tc.name, func(t *testing.T) {
				body := marshalRequiredInput(t, kind.message("event", "book", tc.amt, tc.at))
				e, err := DecodeProto(kind.subject)(body)
				if err != nil {
					t.Fatal(err)
				}
				// Wire seconds/nanos and literal rational expectations are independent
				// of the decoder's AsTime and Decimal conversion helpers.
				wantTime := time.Unix(tc.at.Seconds, int64(tc.at.Nanos)).UTC()
				if e.EventID != "event" || e.CommitmentID != kind.commitment("event", "book") || e.Type != kind.typ ||
					e.Amount.RatString() != tc.want || !e.Date.Equal(wantTime) || e.Date.Location() != time.UTC {
					t.Fatalf("valid input changed: %+v amount=%s, want %s at %s", e, e.Amount, tc.want, wantTime)
				}
			})
		}
	}
}

type countedFund struct {
	fund.Store
	appends int
}

func (s *countedFund) Append(ctx context.Context, e *alt.Event) error {
	s.appends++
	return s.Store.Append(ctx, e)
}

func assertRequiredInputNoClaim(t *testing.T, store fund.Store, tenant string, kind requiredInputKind, fault requiredInputFault) {
	t.Helper()
	id := kind.name + "_" + fault.name
	book := kind.commitment(id, "book_"+id)
	counted := &countedFund{Store: store}
	folder, err := NewFolder(tenant, counted, DecodeProto(kind.subject))
	if err != nil {
		t.Fatal(err)
	}
	env := &envelopepb.Envelope{TenantId: tenant, EventType: kind.subject}
	bad := marshalRequiredInput(t, kind.message(id, book, fault.amt, fault.at))
	if err := folder.Handle(t.Context(), env, bad); err == nil || !strings.Contains(err.Error(), fault.errorField) {
		t.Fatalf("bad input was not refused at decode: %v", err)
	}
	if counted.appends != 0 {
		t.Fatal("refused input reached Append and could claim a durable ID")
	}
	events, err := store.Journal(t.Context(), book)
	if err != nil || len(events) != 0 {
		t.Fatalf("bad input changed the journal: %d %v", len(events), err)
	}
	validDate := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	good := marshalRequiredInput(t, kind.message(id, book, &commonpb.Decimal{Coefficient: 10125, Exponent: -2}, timestamppb.New(validDate)))
	for range 2 {
		if err := folder.Handle(t.Context(), env, good); err != nil {
			t.Fatalf("same-domain-ID correction/duplicate failed: %v", err)
		}
	}
	events, err = store.Journal(t.Context(), book)
	if err != nil || len(events) != 1 || events[0].EventID != id || events[0].Amount.RatString() != "405/4" || !events[0].Date.Equal(validDate) {
		t.Fatalf("refusal consumed the ID or duplicate changed valid evidence: %+v %v", events, err)
	}
}

func TestFolderRequiredInputsDoNotClaimIdentity(t *testing.T) {
	for _, kind := range requiredInputKinds() {
		for _, fault := range requiredInputFaults() {
			t.Run(kind.name+"_"+fault.name, func(t *testing.T) {
				assertRequiredInputNoClaim(t, fund.NewMemoryStore(), testTenant, kind, fault)
			})
		}
	}
}

func requiredFinancialFacts(book string) []struct {
	kind requiredInputKind
	msg  proto.Message
} {
	kinds := requiredInputKinds()
	start := timestamppb.New(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	end := timestamppb.New(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	return []struct {
		kind requiredInputKind
		msg  proto.Message
	}{
		{kinds[0], kinds[0].message(book, book, &commonpb.Decimal{Coefficient: 1000}, start)},
		{kinds[1], kinds[1].message("call", book, &commonpb.Decimal{Coefficient: 100}, start)},
		{kinds[2], kinds[2].message("distribution", book, &commonpb.Decimal{Coefficient: 100}, end)},
		{kinds[3], kinds[3].message("mark", book, &commonpb.Decimal{Coefficient: 100}, end)},
	}
}

func assertRequiredFinancialView(t *testing.T, store fund.Store, book string, nav, tvpi, irr float64) string {
	t.Helper()
	ready := &server.Readiness{}
	ready.Set(true)
	srv := server.New(ready, nil, store)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/commitments/"+book, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("served position failed: %d %s", rec.Code, rec.Body)
	}
	var view map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"committed": "1000.00", "called": "100.00", "uncalled": "900.00", "distributed": "100.00", "nav": fmt.Sprintf("%.2f", nav)} {
		if view[key] != want {
			t.Fatalf("served %s=%v, want %s", key, view[key], want)
		}
	}
	// One 365-day interval: -100 + (100 + NAV)/(1+r) = 0.
	// Thus r=NAV/100, DPI=1, RVPI=NAV/100 and TVPI=1+NAV/100.
	for key, want := range map[string]float64{"tvpi": tvpi, "dpi": 1, "rvpi": nav / 100, "irr": irr} {
		got, ok := view[key].(float64)
		if !ok || math.Abs(got-want) > 1e-8 {
			t.Fatalf("served %s=%v, want %.10g within 1e-8", key, view[key], want)
		}
	}
	pos, err := fund.Materialize(t.Context(), store, book)
	if err != nil {
		t.Fatal(err)
	}
	curve := pos.JCurve()
	if len(curve) != 2 || !curve[0].Date.Equal(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)) ||
		!curve[1].Date.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) || curve[0].Cumulative != -100 || curve[1].Cumulative != 0 {
		t.Fatalf("dated cashflow chronology changed: %+v", curve)
	}
	if !pos.NAVDate.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("residual valuation date changed: %s", pos.NAVDate)
	}
	return rec.Body.String()
}

func TestRequiredInputsPreserveServedDatedFinancialState(t *testing.T) {
	store := fund.NewMemoryStore()
	const book = "financial-book"
	for _, fact := range requiredFinancialFacts(book) {
		folder, _ := NewFolder(testTenant, store, DecodeProto(fact.kind.subject))
		if err := folder.Handle(t.Context(), &envelopepb.Envelope{EventType: fact.kind.subject}, marshalRequiredInput(t, fact.msg)); err != nil {
			t.Fatal(err)
		}
	}
	before := assertRequiredFinancialView(t, store, book, 100, 2, 1)
	for _, kind := range requiredInputKinds() {
		for _, fault := range requiredInputFaults() {
			folder, _ := NewFolder(testTenant, store, DecodeProto(kind.subject))
			id := "bad_" + kind.name + "_" + fault.name
			if kind.typ == alt.EventCommit {
				id = book // A missing commitment amount must not be blessed as a duplicate.
			}
			if err := folder.Handle(t.Context(), &envelopepb.Envelope{EventType: kind.subject}, marshalRequiredInput(t, kind.message(id, book, fault.amt, fault.at))); err == nil {
				t.Fatalf("accepted %s/%s", kind.name, fault.name)
			}
		}
	}
	if after := assertRequiredFinancialView(t, store, book, 100, 2, 1); after != before {
		t.Fatal("refused inputs changed served financial state")
	}
	kind := requiredInputKinds()[3]
	folder, _ := NewFolder(testTenant, store, DecodeProto(kind.subject))
	env := &envelopepb.Envelope{EventType: kind.subject}
	end := timestamppb.New(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	// Existing IDs stay immutable; a distinct-ID restatement at the same as-of
	// remains a new fact. Lexical IDs preserve the fold's existing tie ordering.
	for _, id := range []string{"mark", "mark-restated", "mark-restated"} {
		if err := folder.Handle(t.Context(), env, marshalRequiredInput(t, kind.message(id, book, &commonpb.Decimal{Coefficient: 120}, end))); err != nil {
			t.Fatal(err)
		}
		if id == "mark" {
			assertRequiredFinancialView(t, store, book, 100, 2, 1)
		}
	}
	assertRequiredFinancialView(t, store, book, 120, 2.2, 1.2)
	events, err := store.Journal(t.Context(), book)
	if err != nil || len(events) != 5 {
		t.Fatalf("duplicates/restatement changed history cardinality: %d %v", len(events), err)
	}
	for _, e := range events {
		if e.EventID == "mark" && e.Amount.RatString() != "100" {
			t.Fatal("restatement rewrote historical NAV evidence")
		}
	}
}
