package custody

import (
	"errors"
	"math/big"
	"testing"

	"github.com/eighred/kanz/internal/dec"
	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
)

func TestPostgresStatementPresenceSurvivesReconnect(t *testing.T) {
	const tenant = "custody-presence"
	pool := newCustodyPool(t, tenant)
	requireRestrictedCustodyRole(t, pool)
	applyCustodySchema(t, pool)
	store := NewPostgres(pool)
	consumer, err := NewStatementConsumer(store, tenant, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	msg := presenceStatement("presence")
	exact := &commonpb.Decimal{Coefficient: 1000000000000000001, Exponent: -18}
	msg.Positions = append(msg.Positions, &accountingpb.StatementPosition{InstrumentId: "EXACT", Quantity: exact})
	msg.Cash = append(msg.Cash, &accountingpb.StatementCash{CurrencyCode: "EUR", Balance: exact})
	msg.Transactions = []*accountingpb.StatementTransaction{
		{ExternalRef: "unavailable"},
		{ExternalRef: "zero", Quantity: &commonpb.Decimal{}, Price: &commonpb.Decimal{}, Cash: &commonpb.Decimal{}},
		{ExternalRef: "exact", Quantity: exact, Price: exact, Cash: exact},
	}
	if err := deliver(t, consumer, tenant, msg); err != nil {
		t.Fatal(err)
	}
	// Refuse the whole malformed replacement, retaining the accepted statement.
	for _, field := range []string{"position quantity", "cash balance"} {
		t.Run(field, func(t *testing.T) {
			bad := presenceStatement("invalid")
			if field == "position quantity" {
				bad.Positions[0].Quantity = nil
			} else {
				bad.Cash[0].Balance = nil
			}
			if err := deliver(t, consumer, tenant, bad); err == nil {
				t.Fatal("missing amount reached durable statement storage")
			}
		})
	}
	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM custody_statements`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("refused statement changed durable rows: count=%d err=%v", count, err)
	}
	pool.Close()
	reconnected := newCustodyPool(t, tenant)
	requireRestrictedCustodyRole(t, reconnected)
	statement, err := NewPostgres(reconnected).LatestStatement(t.Context(), subject())
	if err != nil {
		t.Fatal(err)
	}
	if statement.StatementID != "presence" || len(statement.Transactions) != 3 {
		t.Fatalf("reconnect lost the original statement: %+v", statement)
	}
	line := statement.Transactions[0]
	if line.Quantity != nil || line.Price != nil || line.Cash != nil {
		t.Fatalf("unavailable transaction became a durable zero: %+v", line)
	}
	for _, value := range []*big.Rat{statement.Positions["AAPL"], statement.Cash["USD"], statement.Transactions[1].Quantity, statement.Transactions[1].Price, statement.Transactions[1].Cash} {
		if value == nil || value.Sign() != 0 {
			t.Fatalf("reported zero changed after reconnect: %v", value)
		}
	}
	want := new(big.Rat).SetFrac(big.NewInt(1000000000000000001), big.NewInt(1000000000000000000))
	for _, value := range []*big.Rat{statement.Positions["EXACT"], statement.Cash["EUR"], statement.Transactions[2].Quantity, statement.Transactions[2].Price, statement.Transactions[2].Cash} {
		if value == nil || value.Cmp(want) != 0 {
			t.Fatalf("exact 1e-18 figure changed after reconnect: %v", value)
		}
	}
	other := newCustodyPool(t, "custody-presence-other")
	if _, err := NewPostgres(other).LatestStatement(t.Context(), subject()); !errors.Is(err, ErrNoStatement) {
		t.Fatalf("another tenant discovered a statement: %v", err)
	}
}

func TestPostgresStatementPresenceRetainsExactValuesBeyondWireCapacity(t *testing.T) {
	const text = "123456789.000000000000000001"
	pool := newCustodyPool(t, "custody-presence-wide")
	requireRestrictedCustodyRole(t, pool)
	applyCustodySchema(t, pool)
	// This exact durable rational needs a 27-digit coefficient on the wire.
	// Retaining its source value must not fabricate an int64 Decimal encoding.
	value, ok := new(big.Rat).SetString(text)
	if !ok {
		t.Fatal("invalid independent exact fixture")
	}
	statement := Statement{StatementID: "wide", CustodianID: "CUST-A", PortfolioID: "PF1", BusinessDate: t0, ReceivedAt: t0,
		Positions: map[string]*big.Rat{"AAPL": value}, Cash: map[string]*big.Rat{"USD": value}}
	if err := NewPostgres(pool).SaveStatement(t.Context(), statement); err != nil {
		t.Fatal(err)
	}
	pool.Close()
	reconnected := newCustodyPool(t, "custody-presence-wide")
	stored, err := NewPostgres(reconnected).LatestStatement(t.Context(), subject())
	if err != nil {
		t.Fatal(err)
	}
	for _, amount := range []*big.Rat{stored.Positions["AAPL"], stored.Cash["USD"]} {
		if amount == nil || amount.Cmp(value) != 0 {
			t.Fatalf("durable exact value changed: %v", amount)
		}
		if rendered, ok := ExactDecimalText(amount); !ok || rendered != text {
			t.Fatalf("exact review value changed: %q %v", rendered, ok)
		}
		if _, ok := dec.ToProtoExact(amount); ok {
			t.Fatal("unrepresentable coefficient was announced as an exact Decimal")
		}
	}
}
