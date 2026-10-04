package order

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/cashview"
	"github.com/eighred/kanz/internal/dec"
	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/oms/internal/capital"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type legacyAuthorityFixture struct{ terms LegacyTerms }

func (f legacyAuthorityFixture) ResolveLegacy(context.Context, *orderpb.OrderState, time.Time) (LegacyTerms, error) {
	return f.terms, nil
}

func TestLegacyImportBeforeAccountingReplayPreservesCompetingCash(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	now := time.Now().UTC()
	store := NewPostgres(pool)
	old := &orderpb.OrderState{OrderId: "old", PortfolioId: "fund", InstrumentId: "BTC-USD", Side: orderpb.Side_SIDE_BUY,
		OrderType: orderpb.OrderType_ORDER_TYPE_LIMIT, OrderedQuantity: d(1, 0), LeavesQuantity: d(1, 0), LimitPrice: d(80, 0), Status: orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW}
	if err := store.Create(ctx, old, nil); err != nil {
		t.Fatal(err)
	}
	terms := LegacyTerms{Source: "approved-legacy-policy", Version: "v1", ApprovedBy: "treasury-control", Required: []*commonpb.Money{{CurrencyCode: "USD", Amount: d(80, 0)}}}
	if err := store.ReconcileLegacy(ctx, old.OrderId, legacyAuthorityFixture{terms}, now); err != nil {
		t.Fatal(err)
	}
	if err := store.ReconcileLegacy(ctx, old.OrderId, legacyAuthorityFixture{terms}, now); !errors.Is(err, capital.ErrConflict) {
		t.Fatalf("duplicate import=%v", err)
	}
	if err := capital.Apply(ctx, pool, capital.CashEvent{PortfolioID: "fund", Currency: "USD", Revision: 1, Total: dec.Exact("100"), Complete: true, ObservedAt: now}); err != nil {
		t.Fatal(err)
	}
	newOrder := func(id string, price int64) *orderpb.OrderState {
		return &orderpb.OrderState{OrderId: id, PortfolioId: "fund", InstrumentId: "BTC-USD", Side: orderpb.Side_SIDE_BUY,
			OrderType: orderpb.OrderType_ORDER_TYPE_LIMIT, OrderedQuantity: d(1, 0), LeavesQuantity: d(1, 0), LimitPrice: d(price, 0), Status: orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW}
	}
	if err := store.CreateFunded(ctx, newOrder("too-much", 30), nil, []*commonpb.Money{{CurrencyCode: "USD", Amount: d(30, 0)}}, now, time.Minute); !errors.Is(err, capital.ErrInsufficient) {
		t.Fatalf("legacy commitment did not block competing order: %v", err)
	}
	if err := store.CreateFunded(ctx, newOrder("covered", 20), nil, []*commonpb.Money{{CurrencyCode: "USD", Amount: d(20, 0)}}, now, time.Minute); err != nil {
		t.Fatal(err)
	}
	var reserved string
	if err := pool.QueryRow(ctx, `SELECT reserved FROM capital_balances WHERE portfolio_id='fund' AND currency='USD'`).Scan(&reserved); err != nil || reserved != "100" {
		t.Fatalf("reserved=%s err=%v", reserved, err)
	}
}

func TestLegacyScheduledChildSharesParentObligation(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	now := time.Now().UTC()
	store := NewPostgres(pool)
	parent := &orderpb.OrderState{OrderId: "old-parent", PortfolioId: "fund", InstrumentId: "BTC-USD", Side: orderpb.Side_SIDE_BUY,
		OrderType: orderpb.OrderType_ORDER_TYPE_LIMIT, OrderedQuantity: d(1, 0), LimitPrice: d(100, 0), Status: orderpb.OrderStatus_ORDER_STATUS_WORKING_SCHEDULED, ExecutionSchedule: &orderpb.ExecutionSchedule{}}
	child := &orderpb.OrderState{OrderId: "old-child", ParentOrderId: parent.OrderId, PortfolioId: "fund", InstrumentId: parent.InstrumentId, Side: parent.Side,
		OrderType: parent.OrderType, OrderedQuantity: d(6, -1), LimitPrice: parent.LimitPrice, Status: orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW}
	if err := store.Create(ctx, parent, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(ctx, child, nil); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		id    string
		bound int64
	}{{parent.OrderId, 100}, {child.OrderId, 60}} {
		terms := LegacyTerms{Source: "approved-legacy-policy", Version: "v1", ApprovedBy: "treasury-control", Required: []*commonpb.Money{{CurrencyCode: "USD", Amount: d(item.bound, 0)}}}
		if err := store.ReconcileLegacy(ctx, item.id, legacyAuthorityFixture{terms}, now); err != nil {
			t.Fatalf("%s: %v", item.id, err)
		}
	}
	if err := capital.Apply(ctx, pool, capital.CashEvent{PortfolioID: "fund", Currency: "USD", Revision: 1, Total: dec.Exact("100"), Complete: true, ObservedAt: now}); err != nil {
		t.Fatal(err)
	}
	var owners, members int
	var reserved string
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM capital_commitments`).Scan(&owners); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM capital_members`).Scan(&members); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT reserved FROM capital_balances WHERE portfolio_id='fund' AND currency='USD'`).Scan(&reserved); err != nil {
		t.Fatal(err)
	}
	if owners != 1 || members != 2 || reserved != "100" {
		t.Fatalf("legacy schedule double-counted: owners=%d members=%d reserved=%s", owners, members, reserved)
	}
}

func TestLegacyImportRefusesAppliedCashPrefix(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := capital.Apply(ctx, pool, capital.CashEvent{PortfolioID: "fund", Currency: "USD", Revision: 1, Total: dec.Exact("100"), Complete: true, ObservedAt: now}); err != nil {
		t.Fatal(err)
	}
	store := NewPostgres(pool)
	old := &orderpb.OrderState{OrderId: "late-import", PortfolioId: "fund", InstrumentId: "BTC-USD", Side: orderpb.Side_SIDE_BUY,
		OrderType: orderpb.OrderType_ORDER_TYPE_LIMIT, OrderedQuantity: d(1, 0), LimitPrice: d(50, 0), Status: orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW}
	if err := store.Create(ctx, old, nil); err != nil {
		t.Fatal(err)
	}
	terms := LegacyTerms{Source: "approved-legacy-policy", Version: "v1", ApprovedBy: "treasury-control", Required: []*commonpb.Money{{CurrencyCode: "USD", Amount: d(50, 0)}}}
	if err := store.ReconcileLegacy(ctx, old.OrderId, legacyAuthorityFixture{terms}, now); !errors.Is(err, capital.ErrUnknown) {
		t.Fatalf("import after cash replay=%v", err)
	}
	var members int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM capital_members`).Scan(&members); err != nil || members != 0 {
		t.Fatalf("partial legacy import: members=%d err=%v", members, err)
	}
}

func TestLegacyActivationReplaysAuthenticatedCashHistory(t *testing.T) {
	// This deliberately synthetic future-source fixture proves the reconciliation
	// algorithm only. The deployed accounting publisher still reports corporate
	// actions and accruals unproduced, so this is no production activation proof.
	pool := newPool(t)
	ctx := bus.WithTenantID(context.Background(), testTenant)
	now := time.Now().UTC()
	store := NewPostgres(pool)
	old := &orderpb.OrderState{OrderId: "legacy-https", PortfolioId: "fund", InstrumentId: "BTC-USD", Side: orderpb.Side_SIDE_BUY,
		OrderType: orderpb.OrderType_ORDER_TYPE_LIMIT, OrderedQuantity: d(1, 0), LimitPrice: d(80, 0), Status: orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW}
	if err := store.Create(ctx, old, nil); err != nil {
		t.Fatal(err)
	}
	msg := &accountingpb.PortfolioCashBalance{PortfolioId: "fund", BaseCurrency: "USD", Total: d(100, 0), AsOf: timestamppb.New(now), KnowledgeTime: timestamppb.New(now),
		CurrencyCoverage: &domainpb.InputCoverage{Contributed: 1}, Completeness: &accountingpb.BalanceCompleteness{ProducedEntryTypes: []string{"trade", "cash", "fee", "corporate_action", "accrual"}},
		CashCommit: &accountingpb.CashCommitCoverage{Revision: 1, Complete: true}}
	payload, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	page := cashview.CommitPage{TenantID: testTenant, PortfolioID: "fund", Currency: "USD", Through: 1, Next: 1, Records: []cashview.CommitRecord{{Revision: 1, Payload: payload}}}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-oms" || r.URL.Path != "/v1/portfolios/fund/cash-commits" || r.URL.Query().Get("currency") != "USD" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(page)
	}))
	defer server.Close()
	history, err := capital.NewHistoryClient(server.URL, server.Client(), func(context.Context) (string, error) { return "test-oms", nil })
	if err != nil {
		t.Fatal(err)
	}
	terms := LegacyTerms{Source: "approved-legacy-policy", Version: "v1", ApprovedBy: "treasury-control", Required: []*commonpb.Money{{CurrencyCode: "USD", Amount: d(80, 0)}}}
	if err := store.ReconcileLegacyPopulation(ctx, legacyAuthorityFixture{terms}, history, now); err != nil {
		t.Fatal(err)
	}
	var active bool
	var reserved string
	if err := pool.QueryRow(ctx, `SELECT enabled FROM capital_activation`).Scan(&active); err != nil || !active {
		t.Fatalf("activation=%t err=%v", active, err)
	}
	if err := pool.QueryRow(ctx, `SELECT reserved FROM capital_balances WHERE portfolio_id='fund' AND currency='USD'`).Scan(&reserved); err != nil || reserved != "80" {
		t.Fatalf("imported obligation=%s err=%v", reserved, err)
	}
}
