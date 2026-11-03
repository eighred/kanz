package order

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/dualcontrol"
	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/fillfact"
	"github.com/eighred/kanz/internal/outbox"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/oms/internal/capital"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func feeRecoveryFixture(t *testing.T) (*Postgres, context.Context, RecoveryCase, *orderpb.Fill) {
	return feeRecoveryFixtureMode(t, false)
}

func feeRecoveryFixtureMode(t *testing.T, funded bool) (*Postgres, context.Context, RecoveryCase, *orderpb.Fill) {
	t.Helper()
	s := NewPostgres(newPool(t))
	ctx := bus.WithTenantID(context.Background(), testTenant)
	d := func(n int64) *commonpb.Decimal { return &commonpb.Decimal{Coefficient: n} }
	st := &orderpb.OrderState{OrderId: "fee-order", PortfolioId: "fund", Venue: "BINANCE", VenueAccountId: "account", InstrumentId: "BTC-USD", Side: orderpb.Side_SIDE_BUY, OrderedQuantity: d(1), LeavesQuantity: d(1), Status: orderpb.OrderStatus_ORDER_STATUS_ROUTED}
	fillVersion := int64(0)
	if funded {
		if err := capital.Apply(ctx, s.pool, capital.CashEvent{PortfolioID: "fund", Currency: "USD", Revision: 1, Total: "1000", Complete: true, ObservedAt: t0}); err != nil {
			t.Fatal(err)
		}
		st.Status = orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW
		if err := s.CreateFunded(ctx, st, nil, []*commonpb.Money{{CurrencyCode: "USD", Amount: d(100)}}, t0, time.Minute); err != nil {
			t.Fatal(err)
		}
		st.Status = orderpb.OrderStatus_ORDER_STATUS_ROUTED
		if err := s.Save(ctx, st, 0, nil, ""); err != nil {
			t.Fatal(err)
		}
		fillVersion = 1
	} else if err := s.Create(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	f := &orderpb.Fill{OrderId: st.OrderId, FillId: "original", VenueExecutionId: "trade", Venue: st.Venue, VenueAccountId: st.VenueAccountId, InstrumentId: st.InstrumentId, Side: st.Side, Quantity: d(1), Price: d(100), Fee: &commonpb.Money{Amount: d(2), CurrencyCode: "USD"}, ExecutedAt: timestamppb.New(t0)}
	next, err := ApplyFill(st, f, t0)
	if err != nil {
		t.Fatal(err)
	}
	var emitter Emitter
	record, err := emitter.FillFact(ctx, f, next)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Save(ctx, next, fillVersion, []outbox.Record{record}, f.FillId); err != nil {
		t.Fatal(err)
	}
	m := RecoveryMapping{testTenant, "fund", "BINANCE", "account", "verified-account"}
	version, err := s.RecordRecoveryMapping(ctx, m, execution.AccountProof{Verified: true, ExchangeAccountID: m.ExchangeAccount})
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.ObserveRecovery(ctx, "fee-case", st.OrderId, "source", []byte("venue fee differs"))
	if err != nil {
		t.Fatal(err)
	}
	revised := proto.Clone(f).(*orderpb.Fill)
	revised.Fee.Amount = d(1)
	if err := s.FreezeRecoveryHistory(ctx, c, version, next, execution.OrderView{State: execution.OrderViewFilled, ExecutedQuantity: d(1), Fills: []*orderpb.Fill{revised}}, t0); err != nil {
		t.Fatal(err)
	}
	c, err = s.RecoveryCase(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CommitRecovery(ctx, c, t0); !errors.Is(err, fillfact.ErrExecutionIdentityConflict) {
		t.Fatalf("unapproved correction: %v", err)
	}
	if err := s.BlockRecovery(ctx, c, next, "fee correction requires approval", t0); err != nil {
		t.Fatal(err)
	}
	c, err = s.RecoveryCase(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	return s, ctx, c, f
}

func TestFeeProposalRequiresTwoPeopleAndPreservesOriginalExecution(t *testing.T) {
	s, ctx, c, original := feeRecoveryFixture(t)
	if err := s.ProposeFeeCorrection(ctx, c, "user:maker", "venue statement verified", t0); err != nil {
		t.Fatal(err)
	}
	proposal, err := s.FeeProposal(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(proposal.Changes) != 1 || proposal.Changes[0].Previous.Fee.Amount.Coefficient != 2 || proposal.Changes[0].Revised.Fee.Amount.Coefficient != 1 {
		t.Fatalf("review terms=%v", proposal)
	}
	if _, err := s.ReadFeeCorrection(ctx, "other-fund", c.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("portfolio leak: %v", err)
	}
	if _, err := s.ReadFeeCorrection(ctx, "fund", c.ID); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		actor, digest string
		at            time.Time
		want          error
	}{
		{" USER:MAKER ", proposal.Digest, t0, dualcontrol.ErrSelfApproval},
		{"user:checker", "wrong", t0, dualcontrol.ErrPayloadChanged},
		{"user:checker", proposal.Digest, t0.Add(dualcontrol.DefaultTTL), dualcontrol.ErrExpired},
	} {
		if err := s.ApproveFeeCorrection(ctx, c, tc.actor, tc.digest, tc.at); !errors.Is(err, tc.want) {
			t.Fatalf("approval error=%v want=%v", err, tc.want)
		}
	}
	if err := s.ApproveFeeCorrection(ctx, c, "user:checker", proposal.Digest, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	c, err = s.RecoveryCase(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CommitRecovery(ctx, c, t0.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	state, _, err := s.Load(ctx, c.OrderID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != orderpb.OrderStatus_ORDER_STATUS_FILLED || dec.Cmp(state.FilledQuantity, original.Quantity) != 0 {
		t.Fatalf("trade state changed: %v", state)
	}
	var evidence []byte
	if err := s.pool.QueryRow(ctx, `SELECT fill FROM order_executions WHERE order_id=$1`, c.OrderID).Scan(&evidence); err != nil {
		t.Fatal(err)
	}
	var stored orderpb.Fill
	if err := proto.Unmarshal(evidence, &stored); err != nil || !fillfact.SameExecution(&stored, original) {
		t.Fatalf("original rewritten: %v", err)
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM order_fills WHERE order_id=$1`, c.OrderID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("extra execution: %d %v", count, err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM execution_fee_revisions WHERE book='oms'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("revisions: %d %v", count, err)
	}
	c, err = s.RecoveryCase(ctx, c.ID)
	if err != nil || c.Status != "investigating" || c.Checkpoint != 1 {
		t.Fatalf("case closed before book proof: %+v %v", c, err)
	}
	if err := s.ApproveFeeCorrection(ctx, c, "user:checker", proposal.Digest, t0.Add(2*dualcontrol.DefaultTTL)); err != nil {
		t.Fatalf("expired command replay lost committed approval: %v", err)
	}
	if err := s.ProposeFeeCorrection(ctx, c, "user:maker", "venue statement verified", t0.Add(2*dualcontrol.DefaultTTL)); err != nil {
		t.Fatalf("proposal replay lost committed evidence: %v", err)
	}
	heads, err := s.postedRecoveryExecutions(ctx, state)
	if err != nil || !fillfact.SameExecution(heads[fillfact.ExecutionKey(original)], proposal.Changes[0].Revised) {
		t.Fatalf("next recovery did not load revised fee: %v %v", heads, err)
	}
	for _, sql := range []string{`UPDATE execution_fee_proposals SET digest=repeat('b',64)`, `DELETE FROM execution_fee_approvals`, `TRUNCATE execution_fee_approvals`} {
		if _, err := s.pool.Exec(ctx, sql); err == nil {
			t.Fatal("approval evidence modified")
		}
	}
}

func TestFundedFeeCorrectionKeepsActualLiabilityUntilAccounting(t *testing.T) {
	s, ctx, c, _ := feeRecoveryFixtureMode(t, true)
	if err := s.ProposeFeeCorrection(ctx, c, "user:maker", "venue statement verified", t0); err != nil {
		t.Fatal(err)
	}
	proposal, err := s.FeeProposal(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ApproveFeeCorrection(ctx, c, "user:checker", proposal.Digest, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	c, err = s.RecoveryCase(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CommitRecovery(ctx, c, t0.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	var required, executed, reserved string
	if err := s.pool.QueryRow(ctx, `SELECT required_debit,executed_debit FROM capital_commitments WHERE order_id='fee-order'`).Scan(&required, &executed); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT reserved FROM capital_balances WHERE portfolio_id='fund' AND currency='USD'`).Scan(&reserved); err != nil {
		t.Fatal(err)
	}
	if required != "101" || executed != "101" || reserved != "101" {
		t.Fatalf("corrected required=%s executed=%s reserved=%s, want 101 pending accounting", required, executed, reserved)
	}
}
