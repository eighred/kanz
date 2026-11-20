package capital

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dec"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/jackc/pgx/v5"
)

func physicalOrder() *orderpb.OrderState {
	return &orderpb.OrderState{Side: orderpb.Side_SIDE_BUY, OrderType: orderpb.OrderType_ORDER_TYPE_LIMIT,
		OrderedQuantity: &commonpb.Decimal{Coefficient: 125, Exponent: -2}, LimitPrice: &commonpb.Decimal{Coefficient: 200}}
}

func TestPostgresPhysicalDebitsCompeteForSettlementAndFeeCash(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	for _, balance := range []struct{ currency, total string }{{"USDT", "300"}, {"BNB", "1"}} {
		event := snapshot(1, balance.total)
		event.Currency = balance.currency
		if err := Apply(ctx, pool, event); err != nil {
			t.Fatal(err)
		}
	}
	debits, err := PhysicalDebits(physicalOrder(), "BTC", "USDT", []*commonpb.Money{money("BNB", 1), money("USDT", 2)})
	if err != nil {
		t.Fatal(err)
	}
	reserve := func(id string) error {
		return transaction(pool, func(tx pgx.Tx) error {
			return ReserveMany(ctx, tx, "fund", id, debits, snapshot(1, "0").ObservedAt, time.Minute)
		})
	}
	if err := reserve("first"); err != nil {
		t.Fatal(err)
	}
	if err := reserve("competing"); !errors.Is(err, ErrInsufficient) {
		t.Fatalf("second order: %v", err)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM capital_commitments WHERE order_id='competing'`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("refused order retained partial commitments: %d %v", rows, err)
	}
	for currency, want := range map[string]string{"USDT": "252", "BNB": "1"} {
		var reserved string
		if err := pool.QueryRow(ctx, `SELECT reserved FROM capital_balances WHERE portfolio_id='fund' AND currency=$1`, currency).Scan(&reserved); err != nil || reserved != want {
			t.Fatalf("%s reserved=%s want=%s err=%v", currency, reserved, want, err)
		}
	}
}

func TestPhysicalDebitsExactSettlementAndFees(t *testing.T) {
	fee := money("USDT", 2)
	debits, err := PhysicalDebits(physicalOrder(), "BTC", "USDT", []*commonpb.Money{fee, money("BNB", 1)})
	if err != nil || len(debits) != 2 {
		t.Fatalf("debits=%v err=%v", debits, err)
	}
	if debits[0].CurrencyCode != "BNB" || dec.FromProto(debits[0].Amount).RatString() != "1" || debits[1].CurrencyCode != "USDT" || dec.FromProto(debits[1].Amount).RatString() != "252" {
		t.Fatalf("wrong settlement debits: %v", debits)
	}
	debits[1].Amount.Coefficient = 0
	if fee.Amount.Coefficient != 2 {
		t.Fatal("output aliases source fee")
	}
	sell := physicalOrder()
	sell.Side = orderpb.Side_SIDE_SELL
	debits, err = PhysicalDebits(sell, "BTC", "USDT", []*commonpb.Money{money("USDT", 2)})
	if err != nil || len(debits) != 2 || debits[0].CurrencyCode != "BTC" || dec.FromProto(debits[0].Amount).RatString() != "5/4" || dec.FromProto(debits[1].Amount).RatString() != "2" {
		t.Fatalf("sale proceeds funded a debit: %v %v", debits, err)
	}
	debits, err = PhysicalDebits(physicalOrder(), "BTC", "USDT", []*commonpb.Money{})
	if err != nil || len(debits) != 1 || dec.FromProto(debits[0].Amount).RatString() != "250" {
		t.Fatalf("explicit no-fee terms: %v %v", debits, err)
	}
}

func TestPhysicalDebitsRefusesUnknownOrUnboundedTerms(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*orderpb.OrderState)
	}{
		{"market with mark", func(s *orderpb.OrderState) {
			s.OrderType = orderpb.OrderType_ORDER_TYPE_MARKET
			s.ArrivalPrice = s.LimitPrice
		}},
		{"stop trigger", func(s *orderpb.OrderState) {
			s.OrderType = orderpb.OrderType_ORDER_TYPE_STOP
			s.StopPrice = s.LimitPrice
		}},
		{"missing quantity", func(s *orderpb.OrderState) { s.OrderedQuantity = nil }},
		{"negative quantity", func(s *orderpb.OrderState) { s.OrderedQuantity.Coefficient = -1 }},
		{"missing price", func(s *orderpb.OrderState) { s.LimitPrice = nil }},
		{"zero price", func(s *orderpb.OrderState) { s.LimitPrice.Coefficient = 0 }},
		{"out of domain", func(s *orderpb.OrderState) { s.LimitPrice.Exponent = 1000000 }},
		{"unknown side", func(s *orderpb.OrderState) { s.Side = orderpb.Side_SIDE_UNSPECIFIED }},
		{"margin", func(s *orderpb.OrderState) { s.MarginMode = orderpb.MarginMode_MARGIN_MODE_CROSS }},
		{"leverage", func(s *orderpb.OrderState) { s.Leverage = &commonpb.Decimal{Coefficient: 2} }},
		{"unrepresentable product", func(s *orderpb.OrderState) {
			s.OrderedQuantity = &commonpb.Decimal{Coefficient: 9223372036854775807}
			s.LimitPrice = &commonpb.Decimal{Coefficient: 3}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := physicalOrder()
			tc.change(s)
			got, err := PhysicalDebits(s, "BTC", "USDT", []*commonpb.Money{})
			if !errors.Is(err, ErrInvalid) || got != nil {
				t.Fatalf("got %v, %v", got, err)
			}
		})
	}
	for _, fees := range [][]*commonpb.Money{nil, {nil}, {money("BNB", -1)}, {money("BNB", 1), money("BNB", 2)}, {{CurrencyCode: "BNB"}}, {money(" BNB", 1)}} {
		if got, err := PhysicalDebits(physicalOrder(), "BTC", "USDT", fees); !errors.Is(err, ErrInvalid) || got != nil {
			t.Fatalf("invalid fee vector accepted: %v %v", got, err)
		}
	}
	for _, pair := range [][2]string{{"", "USDT"}, {"BTC", "BTC"}, {"BTC", " USD"}} {
		if got, err := PhysicalDebits(physicalOrder(), pair[0], pair[1], []*commonpb.Money{}); !errors.Is(err, ErrInvalid) || got != nil {
			t.Fatalf("unknown settlement accepted: %v %v", got, err)
		}
	}
}
