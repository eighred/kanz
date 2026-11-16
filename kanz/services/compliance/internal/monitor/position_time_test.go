package monitor

import (
	"context"
	"io"
	"log/slog"
	"math/big"
	"reflect"
	"testing"
	"time"

	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/compliancebus"
	"github.com/eighred/kanz/internal/treasury"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type positionTimeSources struct {
	registry                                       comp.MandateSource
	mandates, classified, cash, marks, drag, clock int
	mandateAt                                      time.Time
}

func (s *positionTimeSources) Mandate(ctx context.Context, tenant, portfolio string, at time.Time) (*compliancepb.Mandate, comp.Governance, error) {
	s.mandates++
	s.mandateAt = at
	return s.registry.Mandate(ctx, tenant, portfolio, at)
}

func (s *positionTimeSources) Classify(context.Context, string, time.Time) (comp.Attributes, bool) {
	s.classified++
	return comp.Attributes{Sector: "TECH"}, true
}

func (s *positionTimeSources) Spendable(string) (*commonpb.Decimal, string, *comp.CashCompleteness, bool) {
	s.cash++
	return decv(0, 0), "USD", nil, true
}

func (s *positionTimeSources) Mark(string) *big.Rat {
	s.marks++
	return big.NewRat(100, 1)
}

func positionTimePayload(t *testing.T, portfolio, instrument string, qty, value int64, at *timestamppb.Timestamp) []byte {
	t.Helper()
	data, err := proto.Marshal(&domainpb.PositionState{PortfolioId: portfolio, InstrumentId: instrument,
		Quantity: decv(qty, 0), MarketValue: &commonpb.Money{Amount: decv(value, 0), CurrencyCode: "USD"}, AsOf: at})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func positionTimeMalformed() []struct {
	name string
	at   *timestamppb.Timestamp
} {
	return []struct {
		name string
		at   *timestamppb.Timestamp
	}{
		{"negative-nanos", &timestamppb.Timestamp{Seconds: 1700000000, Nanos: -1}},
		{"negative-second-nanos", &timestamppb.Timestamp{Seconds: 1700000000, Nanos: -1000000000}},
		{"overflow-nanos", &timestamppb.Timestamp{Seconds: 1700000000, Nanos: 1000000000}},
		{"below-minimum-second", &timestamppb.Timestamp{Seconds: -62135596801}},
		{"above-maximum-second", &timestamppb.Timestamp{Seconds: 253402300800}},
	}
}

func positionTimeBooks(m *Monitor) map[bookKey]book {
	m.mu.Lock()
	defer m.mu.Unlock()
	copy := make(map[bookKey]book, len(m.books))
	for key, value := range m.books {
		b := *value
		b.positions = make(map[string]comp.Position, len(value.positions))
		for id, p := range value.positions {
			if p.Quantity != nil {
				p.Quantity = proto.Clone(p.Quantity).(*commonpb.Decimal)
			}
			if p.MarketValue != nil {
				p.MarketValue = proto.Clone(p.MarketValue).(*commonpb.Money)
			}
			b.positions[id] = p
		}
		copy[key] = b
	}
	return copy
}

func TestPositionTimeValidityBeforeEffects(t *testing.T) {
	for _, bad := range positionTimeMalformed() {
		for _, action := range []string{"new-book", "update", "flat"} {
			t.Run(bad.name+"/"+action, func(t *testing.T) {
				s := &positionTimeSources{registry: concentrationRegistry(t)}
				fb, rec := &fakeBus{}, &recordingRecorder{}
				m := NewMonitor(nil, s, s, NewEmitter(fb), rec, slog.New(slog.NewTextHandler(io.Discard, nil)),
					WithCashSource(s), WithMarkSource(s), WithCashDragObserver(func(string, string, treasury.Drag) { s.drag++ }))
				m.now = func() time.Time { s.clock++; return t0.Add(24 * time.Hour) }
				key := bookKey{"t1", "p1"}
				m.books[key] = &book{positions: map[string]comp.Position{
					"A": {InstrumentID: "A", Quantity: decv(1, 0), MarketValue: &commonpb.Money{Amount: decv(100, 0), CurrencyCode: "USD"}},
					"B": {InstrumentID: "B", Quantity: decv(1, 0), MarketValue: &commonpb.Money{Amount: decv(100, 0), CurrencyCode: "USD"}},
				}, lastStatus: compliancepb.ComplianceStatus_COMPLIANCE_STATUS_PASS, warnedUngoverned: true}
				m.books[bookKey{"t1", "idle"}] = &book{positions: map[string]comp.Position{}, emptiedAt: t0, warnedUngoverned: true}
				m.lastBookGC = t0
				before := positionTimeBooks(m)
				portfolio, qty, value := "p1", int64(3), int64(300)
				if action == "new-book" {
					portfolio = "new"
				}
				if action == "flat" {
					qty, value = 0, 0
				}
				if err := m.Handle(testCtx(), &envelopepb.Envelope{TenantId: "t1"}, positionTimePayload(t, portfolio, "A", qty, value, bad.at)); err != nil {
					t.Fatalf("malformed position must ACK: %v", err)
				}
				if !reflect.DeepEqual(before, positionTimeBooks(m)) || !m.lastBookGC.Equal(t0) {
					t.Error("malformed time changed positions/status/warning/idle state or ran book GC")
				}
				if s.clock+s.cash+s.marks+s.drag+s.mandates+s.classified != 0 || len(rec.records) != 0 || len(fb.events) != 0 {
					t.Fatalf("malformed time reached a clock, equity join, observer, mandate/classifier, record or emitter: %+v records=%d events=%d", s, len(rec.records), len(fb.events))
				}
			})
		}
	}
}

type positionTimeClassifier struct{ at []time.Time }

func (c *positionTimeClassifier) Classify(_ context.Context, _ string, at time.Time) (comp.Attributes, bool) {
	c.at = append(c.at, at)
	return comp.Attributes{Sector: "TECH"}, true
}

type positionTimeRecorder struct {
	records []comp.DecisionRecord
	sink    comp.DecisionRecorder
}

func (r *positionTimeRecorder) Record(ctx context.Context, record comp.DecisionRecord) error {
	r.records = append(r.records, record)
	return r.sink.Record(ctx, record)
}

func TestPositionTimePreservesExistingClocksAndLegacyTimes(t *testing.T) {
	// Expected instants are independently specified, not obtained by normalizing
	// any invalid timestamp. Nil's existing epoch behavior is deliberately retained.
	for _, tc := range []struct {
		name  string
		stamp *timestamppb.Timestamp
		want  time.Time
	}{
		{"absent", nil, time.Unix(0, 0).UTC()},
		{"explicit-epoch", &timestamppb.Timestamp{}, time.Unix(0, 0).UTC()},
		{"legacy-year-one", &timestamppb.Timestamp{Seconds: -62135596800}, time.Time{}},
		{"negative-nanos-valid-counterpart", &timestamppb.Timestamp{Seconds: 1699999999, Nanos: 999999999}, time.Unix(1699999999, 999999999).UTC()},
		{"whole-negative-second-counterpart", &timestamppb.Timestamp{Seconds: 1699999999}, time.Unix(1699999999, 0).UTC()},
		{"overflow-nanos-valid-counterpart", &timestamppb.Timestamp{Seconds: 1700000001}, time.Unix(1700000001, 0).UTC()},
		{"negative-epoch", &timestamppb.Timestamp{Seconds: -1, Nanos: 123456789}, time.Unix(-1, 123456789).UTC()},
		{"offset-and-nanoseconds", timestamppb.New(time.Date(2026, 7, 2, 3, 4, 5, 123456789, time.FixedZone("+03", 3*3600))), time.Date(2026, 7, 2, 0, 4, 5, 123456789, time.UTC)},
		{"minimum-nonzero", &timestamppb.Timestamp{Seconds: -62135596800, Nanos: 1}, time.Date(1, 1, 1, 0, 0, 0, 1, time.UTC)},
		{"maximum", &timestamppb.Timestamp{Seconds: 253402300799, Nanos: 999999999}, time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry := concentrationRegistry(t)
			// The sector lookup must still use current reference data even for an
			// old observation. One TECH holding is 100%, exceeding the 60% cap.
			mandate, _, err := registry.Mandate(testCtx(), "t1", "p1", t0)
			if err != nil {
				t.Fatal(err)
			}
			mandate = proto.Clone(mandate).(*compliancepb.Mandate)
			mandate.Version++
			mandate.Rules[0].GetConcentration().Dimension = compliancepb.Dimension_DIMENSION_SECTOR
			if err := registry.Put(mandate); err != nil {
				t.Fatal(err)
			}
			s := &positionTimeSources{registry: registry}
			classifier, fb := &positionTimeClassifier{}, &fakeBus{}
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			rec := &positionTimeRecorder{sink: compliancebus.NewBusRecorder(fb, logger)}
			dropped := 0
			m := NewMonitor(nil, s, classifier, NewEmitter(fb), rec, logger, WithDroppedRecordObserver(func() { dropped++ }))
			now := t0.Add(90 * 24 * time.Hour)
			m.now = func() time.Time { return now }
			err = m.Handle(testCtx(), &envelopepb.Envelope{TenantId: "t1"}, positionTimePayload(t, "p1", "A", 1, 100, tc.stamp))
			if (err != nil) != tc.want.IsZero() {
				t.Fatalf("existing zero-time publish contract changed: %v", err)
			}
			if !s.mandateAt.Equal(now) || len(classifier.at) == 0 {
				t.Fatal("current mandate/classifier resolution did not run")
			}
			for _, at := range classifier.at {
				if !at.Equal(now) {
					t.Fatal("classifier was backdated to observation time")
				}
			}
			if len(rec.records) != 1 || !rec.records[0].Result.GetEvaluatedAt().AsTime().Equal(tc.want) || rec.records[0].TenantID != "t1" {
				t.Fatal("decision observation time or tenant changed")
			}
			if tc.want.IsZero() {
				if dropped != 1 || len(fb.events) != 0 || m.books[bookKey{"t1", "p1"}].lastStatus != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_UNSPECIFIED {
					t.Fatal("legacy zero-time publish refusal/status reset changed")
				}
			} else {
				if dropped != 0 || len(fb.events) != 2 {
					t.Fatalf("valid observation record/breach outputs=%d dropped=%d", len(fb.events), dropped)
				}
				for _, event := range fb.events {
					if !event.EventTime.Equal(tc.want) {
						t.Fatal("record/breach observation time changed")
					}
				}
			}
		})
	}
}

func TestPositionTimeMalformedUpdatesCannotCreateOrSuppressABreach(t *testing.T) {
	fb, rec := &fakeBus{}, &recordingRecorder{}
	m := NewMonitor(nil, concentrationRegistry(t), nil, NewEmitter(fb), rec, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.now = clockAt(t0.Add(time.Hour))
	env := &envelopepb.Envelope{TenantId: "t1"}
	apply := func(instrument string, qty, value int64, stamp *timestamppb.Timestamp) {
		t.Helper()
		if err := m.Handle(testCtx(), env, positionTimePayload(t, "p1", instrument, qty, value, stamp)); err != nil {
			t.Fatal(err)
		}
	}
	apply("A", 1, 100, timestamppb.New(t0))
	apply("B", 1, 100, timestamppb.New(t0))
	fb.events, rec.records = nil, nil
	before := positionTimeBooks(m)
	bad := &timestamppb.Timestamp{Seconds: 1700000000, Nanos: -1}
	apply("A", 3, 300, bad)
	if !reflect.DeepEqual(before, positionTimeBooks(m)) || len(fb.events)+len(rec.records) != 0 {
		t.Fatal("malformed update created a breach")
	}
	observed := time.Date(2026, 7, 2, 0, 4, 5, 123456789, time.UTC)
	apply("A", 3, 300, timestamppb.New(observed))
	// 300/(300+100) = 3/4, above the existing 3/5 concentration cap.
	if len(rec.records) != 1 || len(fb.events) != 1 || rec.records[0].Result.Status != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH || !fb.events[0].EventTime.Equal(observed) {
		t.Fatal("valid recovery did not emit the independently expected breach")
	}
	before = positionTimeBooks(m)
	apply("A", 0, 0, bad)
	if !reflect.DeepEqual(before, positionTimeBooks(m)) {
		t.Fatal("malformed flat update cleared holdings or breach status")
	}
	apply("A", 3, 300, timestamppb.New(observed.Add(time.Second)))
	if len(rec.records) != 1 || len(fb.events) != 1 {
		t.Fatal("malformed flat update rearmed an already reported breach")
	}
}
