package order

import (
	"context"
	"math/big"
	"strings"
	"testing"

	"github.com/eighred/kanz/internal/fillfact"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestFeeRevisionJournalPreventsABAAndRollsBack(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	original := &orderpb.Fill{OrderId: "order", FillId: "alias", Venue: "venue", VenueAccountId: "account", VenueExecutionId: "trade", InstrumentId: "BTC-USD", Side: orderpb.Side_SIDE_BUY, Quantity: &commonpb.Decimal{Coefficient: 1}, Price: &commonpb.Decimal{Coefficient: 100}, Fee: &commonpb.Money{Amount: &commonpb.Decimal{Coefficient: 2}, CurrencyCode: "USD"}, ExecutedAt: timestamppb.New(t0)}
	revise := func(previous *orderpb.Fill, id string, fee int64) *orderpb.Fill {
		t.Helper()
		digest, err := fillfact.EconomicDigest(previous)
		if err != nil {
			t.Fatal(err)
		}
		v := proto.Clone(previous).(*orderpb.Fill)
		v.Fee.Amount = &commonpb.Decimal{Coefficient: fee}
		v.Recovery = &orderpb.ExecutionRecoveryProvenance{CaseId: id, FeeApproval: &orderpb.ExecutionFeeApproval{ProposalId: id, Digest: strings.Repeat("a", 64), Proposer: "maker", Approver: "checker", ApprovedAt: timestamppb.New(t0), PreviousExecutionDigest: digest, PreviousFee: proto.Clone(previous.Fee).(*commonpb.Money)}}
		return v
	}
	first := revise(original, "first", 1)
	second := revise(first, "second", 2)
	for _, tc := range []struct {
		fill          *orderpb.Fill
		delta         int64
		fresh, commit bool
	}{
		{first, 1, true, false}, // failed surrounding cash/outbox transaction
		{first, 1, true, true},
		{second, -1, true, true},
		{first, 0, false, true}, // ABA replay must not refund again
	} {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		delta, fresh, err := fillfact.RecordFeeRevision(ctx, tx, "oms", original, tc.fill)
		if err != nil || fresh != tc.fresh || delta.Cmp(big.NewRat(tc.delta, 1)) != 0 {
			_ = tx.Rollback(ctx)
			t.Fatalf("delta=%v fresh=%v err=%v", delta, fresh, err)
		}
		if tc.commit {
			err = tx.Commit(ctx)
		} else {
			err = tx.Rollback(ctx)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM execution_fee_revisions`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("rows=%d err=%v", count, err)
	}
	for _, sql := range []string{`UPDATE execution_fee_revisions SET approval_digest=repeat('b',64)`, `DELETE FROM execution_fee_revisions`, `TRUNCATE execution_fee_revisions`} {
		if _, err := pool.Exec(ctx, sql); err == nil {
			t.Fatal("immutable fee evidence modified")
		}
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id','other',true)`); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM execution_fee_revisions`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("cross tenant rows=%d err=%v", count, err)
	}
}
