package ledger

import (
	"bytes"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/fillfact"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"
)

func TestPostgresCashSeparatesExecutionsWithinStoredMicrosecond(t *testing.T) {
	ctx := t.Context()
	pool := newPool(t)
	st := NewPostgres(pool)
	one := executionEvidenceFixture("account")
	one.ExecutedAt.Nanos = 321
	two := proto.Clone(one).(*orderpb.Fill)
	two.ExecutedAt.Nanos = 322
	two.OrderId, two.FillId, two.VenueExecutionId = "other", "other-fill", "8"
	corrected := approvedLedgerFee(t, one, "approved-commission", 3)
	entry := func(f *orderpb.Fill) *Event {
		t.Helper()
		e, err := FromFill("fund", f, "USD", day(4))
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	// Reverse receipt order; the timestamp and execution identity each carry
	// information that cannot be inferred from the other or from admission.
	for _, f := range []*orderpb.Fill{two, one, corrected, one, corrected} {
		if err := st.Append(ctx, entry(f), nil); err != nil {
			t.Fatal(err)
		}
	}
	changed := proto.Clone(one).(*orderpb.Fill)
	changed.ExecutedAt.Nanos++
	if err := st.Append(ctx, entry(changed), nil); !errors.Is(err, fillfact.ErrExecutionIdentityConflict) {
		t.Fatalf("same identity with another exact execution time: %v", err)
	}
	for _, column := range []string{"effective", "settled"} {
		e := entry(one)
		if column == "effective" {
			e.Effective = e.Effective.Truncate(time.Microsecond)
		} else {
			e.SettlementDate = e.SettlementDate.Truncate(time.Microsecond)
		}
		if err := st.Append(ctx, e, nil); !errors.Is(err, fillfact.ErrExecutionIdentityConflict) {
			t.Fatalf("inexact incoming %s passed pre-write validation: %v", column, err)
		}
	}
	// This is the WORM row format already present at 7077c75b. Confirm its
	// timestamp columns really lost precision and its evidence bytes did not.
	first := entry(one)
	var effective, settled time.Time
	var retained []byte
	if err := pool.QueryRow(ctx, `SELECT effective_time,settlement_date,execution_evidence FROM ledger_entries WHERE entry_id=$1`, first.EntryID).
		Scan(&effective, &settled, &retained); err != nil || !effective.Equal(day(1)) || !settled.Equal(day(1)) || !bytes.Equal(retained, first.ExecutionEvidence) {
		t.Fatalf("durable source versus timestamp columns: %v %v %v", effective, settled, err)
	}
	rows, err := st.Journal(ctx, "fund")
	if err != nil || len(rows) != 3 {
		t.Fatalf("immutable trades and correction: %d %v", len(rows), err)
	}
	// The old cache includes all three postings at the stored microsecond.
	if err := st.SaveSnapshot(ctx, Replay("fund", rows).Snapshot(day(4))); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		ns                 time.Duration
		cash, first, other int64
		count              int
	}{{320, 0, 0, 0, 0}, {321, -103, 103, 0, 2}, {322, -204, 103, 101, 3}} {
		p, err := MaterializeCash(ctx, NewPostgres(pool), "fund", day(1).Add(tc.ns))
		if err != nil {
			t.Fatal(err)
		}
		debits, unknown := executionDebits(p.events, "USD")
		if unknown != 0 || len(p.events) != tc.count || p.Book.CashBalance("USD").Cmp(big.NewRat(tc.cash, 1)) != 0 ||
			orZero(p.Accounts["account"]["USD"]).Cmp(big.NewRat(tc.cash, 1)) != 0 ||
			orZero(debits[one.OrderId]).Cmp(big.NewRat(tc.first, 1)) != 0 || orZero(debits[two.OrderId]).Cmp(big.NewRat(tc.other, 1)) != 0 {
			t.Fatalf("cutoff %d ns: events=%d cash=%s accounts=%v debits=%v unattributed=%d", tc.ns, len(p.events), p.Book.CashBalance("USD"), p.Accounts, debits, unknown)
		}
	}
}

func TestCashExecutionTimeRequiresMatchingStoredPosting(t *testing.T) {
	fill := executionEvidenceFixture("account")
	fill.ExecutedAt.Nanos = 321
	e, err := FromFill("fund", fill, "USD", day(4))
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Event){
		func(v *Event) { v.Effective = v.Effective.Add(time.Microsecond) },
		func(v *Event) { v.SettlementDate = time.Time{} },
		func(v *Event) { v.Cash = big.NewRat(-100, 1) },
		func(v *Event) { v.Action = &Action{} },
		func(v *Event) { v.SourceRef = "other" },
		func(v *Event) { v.ExecutionEvidence = nil },
		func(v *Event) { v.ExecutionEvidence = []byte{0xff} },
	} {
		candidate := *e
		candidate.Effective, candidate.SettlementDate = day(1), day(1)
		mutate(&candidate)
		before := candidate
		if restoreCashExecutionTime(&candidate) || !candidate.Effective.Equal(before.Effective) ||
			!candidate.SettlementDate.Equal(before.SettlementDate) || coveredExecution(&candidate) != nil {
			t.Fatal("unverified evidence supplied a new clock or order attribution")
		}
	}
}
