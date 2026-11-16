package custody

import (
	"errors"
	"strings"
	"testing"
	"time"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func statementTimeDurableWire() *accountingpb.CustodianStatement {
	msg := statementTimeWire("retained-time")
	msg.Transactions[0].TradeDate = nil
	msg.Transactions[0].SettlementDate = nil
	epoch := proto.Clone(msg.Transactions[1]).(*accountingpb.StatementTransaction)
	epoch.ExternalRef = "trade-epoch"
	epoch.TradeDate, epoch.SettlementDate = &timestamppb.Timestamp{}, &timestamppb.Timestamp{}
	msg.Transactions = append(msg.Transactions, epoch)
	return msg
}

func statementTimeSubject() Subject {
	return Subject{PortfolioID: "TIME-PF", CustodianID: "TIME-CUST", BusinessDate: time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)}
}

func assertStatementTimeDurable(t *testing.T, got Statement) {
	t.Helper()
	if got.StatementID != "retained-time" || !got.BusinessDate.Equal(time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)) ||
		!got.ReceivedAt.Equal(time.Date(2026, 11, 14, 14, 15, 16, 123456000, time.UTC)) || len(got.Transactions) != 3 {
		t.Fatalf("durable statement identity/time changed: %+v", got)
	}
	if !got.Transactions[0].TradeDate.IsZero() || !got.Transactions[0].SettlementDate.IsZero() ||
		!got.Transactions[1].TradeDate.Equal(time.Date(2026, 11, 13, 9, 30, 0, 123456789, time.UTC)) ||
		!got.Transactions[1].SettlementDate.Equal(time.Date(2026, 11, 17, 9, 30, 0, 987654321, time.UTC)) ||
		!got.Transactions[2].TradeDate.Equal(time.Unix(0, 0).UTC()) || !got.Transactions[2].SettlementDate.Equal(time.Unix(0, 0).UTC()) {
		t.Fatal("JSON transaction nanoseconds, optional absence or explicit epoch changed")
	}
}

func TestPostgresStatementTimesRefusalSurvivesReconnect(t *testing.T) {
	const tenant = "custody-statement-times"
	pool := newCustodyPool(t, tenant)
	requireRestrictedCustodyRole(t, pool)
	applyCustodySchema(t, pool)
	store := NewPostgres(pool)
	consumer, err := NewStatementConsumer(store, tenant, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := statementTimeDeliver(t, consumer, tenant, statementTimeDurableWire()); err != nil {
			t.Fatal(err)
		}
	}
	for _, field := range statementTimeFields() {
		for name, timestamp := range statementTimeMalformed() {
			for _, id := range []string{"retained-time", "new-invalid-time"} {
				t.Run(field.name+"/"+name+"/"+id, func(t *testing.T) {
					msg := statementTimeDurableWire()
					msg.StatementId = id
					field.set(msg, timestamp)
					if err := statementTimeDeliver(t, consumer, tenant, msg); err == nil || !strings.Contains(err.Error(), field.name) {
						t.Fatalf("malformed time was not refused: %v", err)
					}
					got, err := store.LatestStatement(t.Context(), statementTimeSubject())
					if err != nil {
						t.Fatal(err)
					}
					assertStatementTimeDurable(t, got)
					var rows int
					if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM custody_statements`).Scan(&rows); err != nil || rows != 1 {
						t.Fatalf("invalid replacement or partial/new statement persisted: rows=%d err=%v", rows, err)
					}
				})
			}
		}
	}
	pool.Close()
	reconnected := newCustodyPool(t, tenant)
	requireRestrictedCustodyRole(t, reconnected)
	got, err := NewPostgres(reconnected).LatestStatement(t.Context(), statementTimeSubject())
	if err != nil {
		t.Fatal(err)
	}
	assertStatementTimeDurable(t, got)
	for _, day := range []time.Time{time.Date(1969, 12, 31, 0, 0, 0, 0, time.UTC), time.Date(1970, 1, 2, 0, 0, 0, 0, time.UTC)} {
		subject := statementTimeSubject()
		subject.BusinessDate = day
		if _, err := NewPostgres(reconnected).LatestStatement(t.Context(), subject); !errors.Is(err, ErrNoStatement) {
			t.Fatalf("invalid nanos left a normalized wrong-day row: %v", err)
		}
	}
	other := newCustodyPool(t, tenant+"-other")
	if _, err := NewPostgres(other).LatestStatement(t.Context(), statementTimeSubject()); !errors.Is(err, ErrNoStatement) {
		t.Fatalf("other tenant discovered statement: %v", err)
	}
}
