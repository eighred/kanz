package consume_test

import (
	"context"
	"errors"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/outbox"
	"github.com/eighred/kanz/internal/pg"
	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/consume"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

type cutoffPublisher struct{}

func (cutoffPublisher) Publish(context.Context, bus.Event) error {
	return errors.New("announcement must be enqueued, not published inside Append")
}

func TestPostgresCashAnnouncementSharesTheEffectiveCutoffAndCommit(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("set TEST_POSTGRES_URL to run the cash announcement transaction and tenant RLS proof")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := tenantPool(t, ctx, dsn, "cutoff-acme")
	var super, bypass bool
	if err := pool.QueryRow(ctx, `SELECT rolsuper,rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&super, &bypass); err != nil || super || bypass {
		t.Fatalf("restricted role required: super=%v bypass=%v err=%v", super, bypass, err)
	}
	st := ledger.NewPostgres(pool)
	at := time.Unix(1_700_000_000, 0).UTC()
	entry := func(id, currency string, amount *big.Rat, effective time.Time) *ledger.Event {
		return &ledger.Event{EntryID: id, PortfolioID: "PF-cutoff", VenueAccountID: "ACC1", Type: ledger.EntryCash,
			Cash: amount, CashCurrency: currency, Effective: effective, Knowledge: at}
	}
	ctx = bus.WithTraceContext(bus.WithCausationID(bus.WithCorrelationID(bus.WithTenantID(ctx, "cutoff-acme"), "cutoff-corr"), "cutoff-cause"), "cutoff-trace")
	for _, e := range []*ledger.Event{
		entry("today", "USD", big.NewRat(100, 1), at),
		entry("tomorrow", "USD", big.NewRat(25, 1), at.Add(24*time.Hour)),
		entry("future-foreign", "EUR", big.NewRat(-25, 1), at.Add(time.Microsecond)),
	} {
		if err := st.Append(ctx, e, nil); err != nil {
			t.Fatal(err)
		}
	}
	clockReads := 0
	ann := consume.NewAnnouncer(nil, cutoffPublisher{}, "USD", consume.EntrySourcePosture{Produced: []string{"cash"}}, nil, func() time.Time {
		clockReads++
		return at.Add(time.Duration(clockReads-1) * 24 * time.Hour)
	})
	announce := func(ctx context.Context, view ledger.Store) ([]outbox.Record, error) {
		return ann.Records(ctx, view, "PF-cutoff")
	}
	// A newly learned, backdated correction must be visible before its INSERT
	// commits. The independent cash oracle is 100 - 5 = 95, never 120.
	correction := entry("late-correction", "USD", big.NewRat(-5, 1), at.Add(-time.Hour))
	if err := st.Append(ctx, correction, announce); err != nil {
		t.Fatalf("append with actual announcer: %v", err)
	}
	if clockReads != 1 {
		t.Fatalf("clock sampled %d times, want one", clockReads)
	}
	pending, err := st.Outbox().Pending(ctx, "PF-cutoff", 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending=%v err=%v, want one committed FACT", pending, err)
	}
	rec := pending[0].Record
	if rec.TenantID != "cutoff-acme" || rec.CorrelationID != "cutoff-corr" || rec.CausationID != "cutoff-cause" || rec.TraceContext != "cutoff-trace" || !rec.EventTime.Equal(at) {
		t.Fatalf("persisted tenant, lineage or event time changed: %+v", rec)
	}
	event, err := rec.Event()
	if err != nil {
		t.Fatal(err)
	}
	msg := event.Payload.(*accountingpb.PortfolioCashBalance)
	if dec.FromProto(msg.GetTotal()).RatString() != "95" || len(msg.GetByVenueAccount()) != 1 || dec.FromProto(msg.GetByVenueAccount()[0].GetAmount()).RatString() != "95" || msg.GetByVenueAccount()[0].GetAsset() != "USD" {
		t.Fatalf("cash fields disagree with the 95 USD effective oracle: %v", msg)
	}
	if !msg.GetAsOf().AsTime().Equal(at) || !msg.GetKnowledgeTime().AsTime().Equal(at) || msg.GetCurrencyCoverage().GetContributed() != 1 || msg.GetCurrencyCoverage().GetExcludedCount() != 0 || len(msg.GetExcludedCurrencies()) != 0 {
		t.Fatalf("metadata or coverage used a different cutoff: %v", msg)
	}

	// Both readers must be tenant-scoped; a second real tenant cannot discover
	// either the journal or its derived outbox FACT under the same portfolio id.
	otherPool, err := pg.NewTenantPool(ctx, dsn, "cutoff-other")
	if err != nil {
		t.Fatal(err)
	}
	defer otherPool.Close()
	other := ledger.NewPostgres(otherPool)
	otherCtx := bus.WithTenantID(ctx, "cutoff-other")
	if journal, err := other.Journal(otherCtx, "PF-cutoff"); err != nil || len(journal) != 0 {
		t.Fatalf("foreign journal visible: entries=%v err=%v", journal, err)
	}
	if rows, err := other.Outbox().Pending(otherCtx, "PF-cutoff", 10); err != nil || len(rows) != 0 {
		t.Fatalf("foreign FACT visible: rows=%v err=%v", rows, err)
	}

	for _, tc := range []struct{ name, currency, account, want string }{
		{"total refusal", "USD", "", "cash balance is not representable"},
		{"account refusal", "EUR", "ACC1", "EUR balance in ACC1 is not representable"},
		{"missing tenant", "USD", "ACC1", "tenant"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := entry("refused-"+tc.name, tc.currency, big.NewRat(1, 3), at)
			bad.VenueAccountID = tc.account
			callCtx := ctx
			if tc.name == "missing tenant" {
				callCtx = bus.WithTenantID(ctx, "")
				bad.Cash = big.NewRat(25, 1)
			}
			// Keep this callback's clock fixed; advancing it would legitimately
			// include the next day's entries and test a different valuation.
			refuser := consume.NewAnnouncer(nil, cutoffPublisher{}, "USD", consume.EntrySourcePosture{Produced: []string{"cash"}}, nil, func() time.Time { return at })
			err := st.Append(callCtx, bad, func(ctx context.Context, view ledger.Store) ([]outbox.Record, error) {
				return refuser.Records(ctx, view, "PF-cutoff")
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("append=%v, want %q refusal", err, tc.want)
			}
			journal, err := st.Journal(ctx, "PF-cutoff")
			if err != nil || len(journal) != 4 {
				t.Fatalf("refused INSERT committed: journal=%v err=%v", journal, err)
			}
			rows, err := st.Outbox().Pending(ctx, "PF-cutoff", 10)
			if err != nil || len(rows) != 1 || rows[0].ID != pending[0].ID {
				t.Fatalf("refused FACT committed: rows=%v err=%v", rows, err)
			}
		})
	}
}
