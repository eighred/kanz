package ledger

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"sync"
	"testing"

	"github.com/eighred/kanz/internal/fillfact"
	"github.com/eighred/kanz/internal/outbox"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func approvedLedgerFee(t *testing.T, previous *orderpb.Fill, id string, fee int64) *orderpb.Fill {
	t.Helper()
	digest, err := fillfact.EconomicDigest(previous)
	if err != nil {
		t.Fatal(err)
	}
	v := proto.Clone(previous).(*orderpb.Fill)
	v.Fee.Amount = d(fee, 0)
	v.Recovery = &orderpb.ExecutionRecoveryProvenance{CaseId: id, FeeApproval: &orderpb.ExecutionFeeApproval{ProposalId: id, Digest: strings.Repeat("a", 64), Proposer: "user:maker", Approver: "user:checker", ApprovedAt: timestamppb.New(day(3)), PreviousExecutionDigest: digest, PreviousFee: proto.Clone(previous.Fee).(*commonpb.Money)}}
	return v
}

func TestApprovedFeeCorrectionIsAtomicCashOnlyAndReplaySafe(t *testing.T) {
	for _, backend := range []string{"memory", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var store Store = NewMemoryStore()
			if backend == "postgres" {
				store = NewPostgres(newPool(t))
			}
			ctx := context.Background()
			original := executionEvidenceFixture("account")
			entry := func(f *orderpb.Fill) *Event {
				t.Helper()
				e, err := FromFill("fund", f, "USD", day(4))
				if err != nil {
					t.Fatal(err)
				}
				return e
			}
			if err := store.Append(ctx, entry(original), nil); err != nil {
				t.Fatal(err)
			}
			first := approvedLedgerFee(t, original, "first", 2)
			refusal := errors.New("outbox unavailable")
			if err := store.Append(ctx, entry(first), func(context.Context, Store) ([]outbox.Record, error) { return nil, refusal }); !errors.Is(err, refusal) {
				t.Fatalf("rollback: %v", err)
			}
			var wg sync.WaitGroup
			errs := make(chan error, 2)
			e := entry(first)
			for range 2 {
				wg.Go(func() { errs <- store.Append(ctx, e, nil) })
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatal(err)
				}
			}
			second := approvedLedgerFee(t, first, "second", 1)
			for _, f := range []*orderpb.Fill{second, first, second, original} {
				if err := store.Append(ctx, entry(f), nil); err != nil {
					t.Fatal(err)
				}
			}
			journal, err := store.Journal(ctx, "fund")
			if err != nil || len(journal) != 3 {
				t.Fatalf("journal=%d err=%v", len(journal), err)
			}
			book := Replay("fund", journal)
			if book.Cash["USD"].Cmp(big.NewRat(-101, 1)) != 0 || book.Positions["BTC-USD"].Qty.Cmp(big.NewRat(1, 1)) != 0 {
				t.Fatalf("cash or position changed twice: %+v", book)
			}
			for _, e := range journal {
				if e.Type == EntryFee && (e.Quantity != nil || e.SourceRef != "fill:"+fillfact.ExecutionKey(original)) {
					t.Fatalf("correction lost original attribution: %+v", e)
				}
			}
			stale := approvedLedgerFee(t, first, "stale", 3)
			if err := store.Append(ctx, entry(stale), nil); !errors.Is(err, fillfact.ErrFeeRevision) {
				t.Fatalf("stale baseline: %v", err)
			}
		})
	}
}

func TestApprovedFeeRecoveryOnEmptyBookDoesNotPostImaginaryOriginalFee(t *testing.T) {
	for _, backend := range []string{"memory", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var store Store = NewMemoryStore()
			if backend == "postgres" {
				store = NewPostgres(newPool(t))
			}
			ctx := context.Background()
			revised := approvedLedgerFee(t, executionEvidenceFixture("account"), "new", 2)
			e, err := FromFill("fund", revised, "USD", day(4))
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := store.Append(ctx, e, nil); err != nil {
					t.Fatal(err)
				}
			}
			journal, err := store.Journal(ctx, "fund")
			if err != nil || len(journal) != 1 {
				t.Fatalf("journal=%d err=%v", len(journal), err)
			}
			if got := Replay("fund", journal).Cash["USD"]; got.Cmp(big.NewRat(-102, 1)) != 0 {
				t.Fatal(got)
			}
		})
	}
}
