package custody

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func statementTimeWire(id string) *accountingpb.CustodianStatement {
	line := &accountingpb.StatementTransaction{
		ExternalRef: "trade-1", InstrumentId: "AAPL", CurrencyCode: "USD",
		Quantity: &commonpb.Decimal{Coefficient: 1}, Price: &commonpb.Decimal{Coefficient: 2}, Cash: &commonpb.Decimal{Coefficient: -2},
		TradeDate:      timestamppb.New(time.Date(2026, 11, 13, 9, 30, 0, 123456789, time.UTC)),
		SettlementDate: timestamppb.New(time.Date(2026, 11, 17, 9, 30, 0, 987654321, time.UTC)),
	}
	second := proto.Clone(line).(*accountingpb.StatementTransaction)
	second.ExternalRef = "trade-2"
	return &accountingpb.CustodianStatement{
		StatementId: id, CustodianId: "TIME-CUST", PortfolioId: "TIME-PF",
		BusinessDate: timestamppb.New(time.Date(2026, 11, 13, 23, 59, 59, 999999999, time.UTC)),
		ReceivedAt:   timestamppb.New(time.Date(2026, 11, 14, 14, 15, 16, 123456000, time.UTC)),
		Positions:    []*accountingpb.StatementPosition{{InstrumentId: "AAPL", Quantity: &commonpb.Decimal{Coefficient: 2}}},
		Cash:         []*accountingpb.StatementCash{{CurrencyCode: "USD", Balance: &commonpb.Decimal{Coefficient: -4}}},
		Grain:        accountingpb.StatementGrain_STATEMENT_GRAIN_TRANSACTIONS,
		Transactions: []*accountingpb.StatementTransaction{line, second},
	}
}

type statementTimeField struct {
	name string
	set  func(*accountingpb.CustodianStatement, *timestamppb.Timestamp)
}

func statementTimeFields() []statementTimeField {
	return []statementTimeField{
		{"business_date", func(m *accountingpb.CustodianStatement, ts *timestamppb.Timestamp) { m.BusinessDate = ts }},
		{"received_at", func(m *accountingpb.CustodianStatement, ts *timestamppb.Timestamp) { m.ReceivedAt = ts }},
		{"trade_date", func(m *accountingpb.CustodianStatement, ts *timestamppb.Timestamp) { m.Transactions[1].TradeDate = ts }},
		{"settlement_date", func(m *accountingpb.CustodianStatement, ts *timestamppb.Timestamp) {
			m.Transactions[1].SettlementDate = ts
		}},
	}
}

func statementTimeMalformed() map[string]*timestamppb.Timestamp {
	return map[string]*timestamppb.Timestamp{
		"negative_nanos":    {Seconds: 0, Nanos: -1},
		"overflow_nanos":    {Seconds: 86399, Nanos: 1000000000},
		"seconds_below_min": {Seconds: -62135596801},
		"seconds_above_max": {Seconds: 253402300800},
	}
}

type statementTimeStore struct {
	*MemoryStore
	saves int
}

func (s *statementTimeStore) SaveStatement(ctx context.Context, statement Statement) error {
	s.saves++
	return s.MemoryStore.SaveStatement(ctx, statement)
}

func statementTimeDeliver(t *testing.T, consumer *StatementConsumer, tenant string, msg *accountingpb.CustodianStatement) error {
	t.Helper()
	raw, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	return consumer.Handle(t.Context(), &envelopepb.Envelope{TenantId: tenant}, raw)
}

func TestStatementTimesRejectMalformedWireBeforeStore(t *testing.T) {
	for _, field := range statementTimeFields() {
		for name, timestamp := range statementTimeMalformed() {
			t.Run(field.name+"/"+name, func(t *testing.T) {
				msg := statementTimeWire("invalid-time")
				field.set(msg, timestamp)
				statement, err := statementFromProto(msg)
				if err == nil || !strings.Contains(err.Error(), field.name) || !reflect.DeepEqual(statement, Statement{}) {
					t.Errorf("malformed %s produced a statement: err=%v statement=%+v", field.name, err, statement)
				}
				store := &statementTimeStore{MemoryStore: NewMemoryStore()}
				consumer, err := NewStatementConsumer(store, "custody-time", testLogger())
				if err != nil {
					t.Fatal(err)
				}
				if err := statementTimeDeliver(t, consumer, "custody-time", msg); err == nil || store.saves != 0 {
					t.Errorf("malformed wire reached SaveStatement: err=%v saves=%d", err, store.saves)
				}
			})
		}
	}
	// The independent UTC oracle: normalizing these invalid nanos crosses a day.
	// Refusal must happen before BusinessDay can hide the malformed representation.
	for _, bad := range []*timestamppb.Timestamp{{Seconds: 0, Nanos: -1}, {Seconds: 86399, Nanos: 1000000000}} {
		msg := statementTimeWire("day-boundary")
		msg.BusinessDate = bad
		store := NewMemoryStore()
		consumer, err := NewStatementConsumer(store, "custody-time", testLogger())
		if err != nil {
			t.Fatal(err)
		}
		_ = statementTimeDeliver(t, consumer, "custody-time", msg)
		for _, wrongDay := range []time.Time{time.Date(1969, 12, 31, 0, 0, 0, 0, time.UTC), time.Date(1970, 1, 2, 0, 0, 0, 0, time.UTC)} {
			if _, err := store.LatestStatement(t.Context(), Subject{PortfolioID: "TIME-PF", CustodianID: "TIME-CUST", BusinessDate: wrongDay}); !errors.Is(err, ErrNoStatement) {
				t.Errorf("invalid representation created a normalized wrong-day statement: %v", err)
			}
		}
	}
}

func TestStatementTimeNilAndValidCompatibility(t *testing.T) {
	t.Run("nanoseconds_and_business_day", func(t *testing.T) {
		msg := statementTimeWire("valid-time")
		msg.ReceivedAt.Nanos = 123456789
		statement, err := statementFromProto(msg)
		if err != nil {
			t.Fatal(err)
		}
		if !statement.BusinessDate.Equal(time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)) ||
			!statement.ReceivedAt.Equal(time.Date(2026, 11, 14, 14, 15, 16, 123456789, time.UTC)) ||
			!statement.Transactions[1].TradeDate.Equal(time.Date(2026, 11, 13, 9, 30, 0, 123456789, time.UTC)) ||
			!statement.Transactions[1].SettlementDate.Equal(time.Date(2026, 11, 17, 9, 30, 0, 987654321, time.UTC)) {
			t.Fatal("valid time precision or UTC business date changed")
		}
		memory := NewMemoryStore()
		if err := memory.SaveStatement(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
		got, err := memory.LatestStatement(t.Context(), statement.Subject())
		if err != nil || !got.ReceivedAt.Equal(statement.ReceivedAt) || !got.Transactions[1].TradeDate.Equal(statement.Transactions[1].TradeDate) {
			t.Fatalf("memory changed valid nanoseconds: %v", err)
		}
	})
	for _, sample := range []struct {
		name string
		ts   *timestamppb.Timestamp
		want time.Time
	}{
		{"unix_epoch", &timestamppb.Timestamp{}, time.Unix(0, 0).UTC()},
		{"negative_valid_seconds", &timestamppb.Timestamp{Seconds: -1, Nanos: 999999999}, time.Date(1969, 12, 31, 23, 59, 59, 999999999, time.UTC)},
		{"maximum_valid", &timestamppb.Timestamp{Seconds: 253402300799, Nanos: 999999999}, time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)},
	} {
		t.Run(sample.name, func(t *testing.T) {
			msg := statementTimeWire(sample.name)
			for _, field := range statementTimeFields() {
				field.set(msg, sample.ts)
			}
			got, err := statementFromProto(msg)
			if err != nil || !got.ReceivedAt.Equal(sample.want) || !got.Transactions[1].TradeDate.Equal(sample.want) || !got.Transactions[1].SettlementDate.Equal(sample.want) {
				t.Fatalf("valid canonical time changed: %v", err)
			}
			wantDay := time.Date(sample.want.Year(), sample.want.Month(), sample.want.Day(), 0, 0, 0, 0, time.UTC)
			if !got.BusinessDate.Equal(wantDay) {
				t.Fatal("valid business date changed")
			}
		})
	}
	for _, field := range statementTimeFields() {
		for _, absent := range []struct {
			name string
			ts   *timestamppb.Timestamp
		}{{"nil", nil}, {"go_zero_year1", &timestamppb.Timestamp{Seconds: -62135596800}}} {
			t.Run(field.name+"/"+absent.name, func(t *testing.T) {
				msg := statementTimeWire("absence")
				field.set(msg, absent.ts)
				got, err := statementFromProto(msg)
				if field.name == "business_date" || field.name == "received_at" {
					if err == nil || !strings.Contains(err.Error(), field.name) {
						t.Fatal("existing domain-required time refusal changed")
					}
				} else if err != nil || (field.name == "trade_date" && !got.Transactions[1].TradeDate.IsZero()) || (field.name == "settlement_date" && !got.Transactions[1].SettlementDate.IsZero()) {
					t.Fatalf("optional zero time convention changed: %v", err)
				}
			})
		}
	}
}
