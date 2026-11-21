package ledger

import (
	"context"
	"math/big"
	"testing"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestPostgresCustodyExecutionEconomicsUsesOriginalReceiptPosition(t *testing.T) {
	ctx := t.Context()
	pool := newPool(t)
	st := NewPostgres(pool)
	var bypass bool
	if err := pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_roles WHERE pg_has_role(current_user,oid,'USAGE') AND (rolsuper OR rolbypassrls))").Scan(&bypass); err != nil || bypass {
		t.Fatalf("restricted database role required: %v", err)
	}
	f := executionEvidenceFixture("desk-a")
	f.Side = orderpb.Side_SIDE_SELL
	f.Quantity, f.Price = d(125, -2), d(64, 0)
	f.ExecutedAt = timestamppb.New(day(1).Add(321 * time.Nanosecond))
	appendFill := func(f *orderpb.Fill) *Event {
		t.Helper()
		e, err := FromFill("fund", f, "USD", day(3))
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Append(ctx, e, nil); err != nil {
			t.Fatal(err)
		}
		return e
	}
	a := appendFill(f) // Sell 1.25 x 64 = +80 gross, +79 booked and settled.
	other := proto.Clone(f).(*orderpb.Fill)
	other.VenueAccountId, other.OrderId = "desk-b", "other-order"
	other.Side = orderpb.Side_SIDE_BUY
	other.Fee.Amount = d(0, 0)
	b := appendFill(other)
	first := approvedLedgerFee(t, f, "first-cost", 3)
	appendFill(first)
	appendFill(approvedLedgerFee(t, first, "revised-to-zero", 0))
	missing := proto.Clone(f).(*orderpb.Fill)
	missing.FillId, missing.VenueExecutionId = "raw-missing-fee", "55"
	missing.Fee = nil
	noFee := appendFill(missing)
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := tx.Rollback(ctx); err != nil {
			t.Errorf("rollback ledger fixture: %v", err)
		}
	}()
	for _, c := range []struct {
		pos                  int64
		e                    *Event
		qty, gross, fee, net string
	}{
		{1, a, "-5/4", "80", "1", "79"},
		{2, a, "-5/4", "80", "1", "79"},
		{2, b, "5/4", "-80", "0", "-80"},
		{3, a, "-5/4", "80", "3", "77"},
		{4, a, "-5/4", "80", "0", "80"},
		{1, a, "-5/4", "80", "1", "79"},
	} {
		got, err := ReadCustodyExecutionEconomics(ctx, tx, "fund", c.e.VenueAccountID, c.e.EntryID, f.FillId, f.InstrumentId, day(1), c.pos)
		if err != nil || got == nil {
			t.Fatalf("prefix %d account %s unreadable: %v", c.pos, c.e.VenueAccountID, err)
		}
		if got.Quantity.RatString() != c.qty || got.Price.Cmp(big.NewRat(64, 1)) != 0 || got.GrossCash.RatString() != c.gross ||
			got.NetCash.RatString() != c.net || got.SettledCash.RatString() != c.net || got.Commission.RatString() != c.fee ||
			got.Venue != "BINANCE" || got.VenueExecutionID != "7" || got.CommissionCurrency != "USD" || got.CashCurrency != "USD" ||
			got.SettlementBasis != SettlementSettled || !got.TradeDate.Equal(f.ExecutedAt.AsTime()) || !got.SettlementDate.Equal(f.ExecutedAt.AsTime()) {
			t.Fatalf("unexpected economics at %d: %+v", c.pos, got)
		}
	}
	for _, c := range []struct {
		name, portfolio, account, entry, ref, instrument string
		day                                              time.Time
		pos                                              int64
	}{
		{"future prefix", "fund", "desk-a", a.EntryID, f.FillId, f.InstrumentId, day(1), 6},
		{"entry after cursor", "fund", "desk-b", b.EntryID, f.FillId, f.InstrumentId, day(1), 1},
		{"missing explicit fee", "fund", "desk-a", noFee.EntryID, missing.FillId, f.InstrumentId, day(1), 5},
		{"foreign portfolio", "other", "desk-a", a.EntryID, f.FillId, f.InstrumentId, day(1), 1},
		{"other account", "fund", "desk-b", a.EntryID, f.FillId, f.InstrumentId, day(1), 1},
		{"foreign instrument", "fund", "desk-a", a.EntryID, f.FillId, "ETH-USD", day(1), 1},
		{"other ref", "fund", "desk-a", a.EntryID, "raw-other", f.InstrumentId, day(1), 1},
		{"other day", "fund", "desk-a", a.EntryID, f.FillId, f.InstrumentId, day(2), 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := ReadCustodyExecutionEconomics(ctx, tx, c.portfolio, c.account, c.entry, c.ref, c.instrument, c.day, c.pos)
			if err != nil || got != nil {
				t.Fatalf("wanted unknown, got %+v (%v)", got, err)
			}
		})
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	// A distinct native identity with the same raw reference in the same account
	// makes the source ambiguous only from its own commit position forwards.
	collision := proto.Clone(f).(*orderpb.Fill)
	collision.VenueExecutionId = "different"
	collision.OrderId = "different-order"
	appendFill(collision)
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		for pos, wantKnown := range map[int64]bool{5: true, 6: false} {
			got, err := ReadCustodyExecutionEconomics(ctx, tx, "fund", "desk-a", a.EntryID, f.FillId, f.InstrumentId, day(1), pos)
			if err != nil {
				return err
			}
			if (got != nil) != wantKnown {
				t.Fatalf("ambiguous reference at %d: %+v", pos, got)
			}
		}
		if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id','other-tenant',true)"); err != nil {
			return err
		}
		hidden, err := ReadCustodyExecutionEconomics(ctx, tx, "fund", "desk-a", a.EntryID, f.FillId, f.InstrumentId, day(1), 1)
		if err != nil || hidden != nil {
			t.Fatalf("foreign tenant could discover an execution: %+v %v", hidden, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCustodyExecutionEconomicsRejectsMissingSelector(t *testing.T) {
	got, err := ReadCustodyExecutionEconomics(context.Background(), nil, "fund", "", "id", "ref", "instrument", day(1), 1)
	if err != nil || got != nil {
		t.Fatal("empty account must not open a query")
	}
}
