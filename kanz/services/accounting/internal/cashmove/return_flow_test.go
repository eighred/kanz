package cashmove_test

import (
	"math/big"
	"testing"
	"time"

	"github.com/eighred/kanz/services/accounting/internal/cashmove"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/jackc/pgx/v5"
)

func TestRecordedExternalFlowsMatchesCompleteOrderedJournal(t *testing.T) {
	pool := database(t)
	ctx := t.Context()
	commands, journal := cashmove.NewCommands(pool), ledger.NewPostgres(pool)
	start := time.Date(2020, 2, 3, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	now := func() time.Time {
		t.Helper()
		var horizon time.Time
		if err := pool.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&horizon); err != nil {
			t.Fatal(err)
		}
		return horizon
	}
	book := func(id, currency, account string, effective time.Time, amount *big.Rat) cashmove.ExternalFlowProof {
		t.Helper()
		cmd := command()
		m := &cmd.Movement
		m.MovementID, m.Currency, m.VenueAccountID, m.Effective = id, currency, account, effective
		m.Amount = new(big.Rat).Abs(amount)
		if amount.Sign() < 0 {
			m.Kind = cashmove.Redemption
		}
		receipt, err := commands.Accept(ctx, cmd, digest(t, cmd))
		if err != nil {
			t.Fatal(err)
		}
		err = journal.Append(ctx, &ledger.Event{EntryID: "cash:" + id, PortfolioID: m.PortfolioID, Type: ledger.EntryCash,
			VenueAccountID: account, Effective: effective, Knowledge: receipt.AcceptedAt, SourceRef: m.SourceRef,
			Cash: amount, CashCurrency: currency}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return cashmove.ExternalFlowProof{MovementID: id, CommandDigest: receipt.Digest, Effective: effective, Amount: amount}
	}
	check := func(label, tenant string, horizon time.Time, want bool, proofs ...cashmove.ExternalFlowProof) {
		t.Helper()
		tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		got, err := cashmove.RecordedExternalFlows(ctx, tx, tenant, "PF", "USD", start, end, horizon, proofs)
		if err != nil || got != want {
			t.Fatalf("%s: matched=%v want=%v err=%v", label, got, want, err)
		}
	}
	const tenant = "tenant-A"
	// The opening instant is outside the window, the closing instant is inside.
	book("opening", "USD", "", start, big.NewRat(7, 1))
	check("empty proof cannot attest a booked flow", tenant, now(), false)
	second := book("redemption", "USD", "", end, big.NewRat(-9, 4))
	first := book("subscription", "USD", "", start.Add(time.Hour), big.NewRat(17, 8))
	horizon := now()
	check("order by effective time, not booking order", tenant, horizon, true, first, second)
	check("wrong tenant", "tenant-B", horizon, false, first, second)
	check("reversed", tenant, horizon, false, second, first)
	check("missing movement", tenant, horizon, false, first)
	check("extra signed movement", tenant, horizon, false, first, second, second)
	wrong := second
	wrong.CommandDigest = first.CommandDigest
	check("another accepted command", tenant, horizon, false, first, wrong)
	wrong = second
	wrong.Amount = big.NewRat(9, 4)
	check("signed redemption magnitude", tenant, horizon, false, first, wrong)

	tie := book("same-instant", "USD", "", first.Effective, big.NewRat(1, 1))
	check("ambiguous instant", tenant, now(), false, first, tie, second)
	check("prior recorded horizon", tenant, horizon, true, first, second)
	// A later window isolates each unexpected leg from the equal-instant case.
	start = end
	end = start.Add(24 * time.Hour)
	base := book("second-day", "USD", "", start.Add(time.Hour), big.NewRat(3, 1))
	foreign := book("foreign", "EUR", "", end, big.NewRat(1, 1))
	check("foreign cash must not disappear", tenant, now(), false, base)
	check("foreign cash as USD", tenant, now(), false, base, foreign)
	start = end
	end = start.Add(24 * time.Hour)
	base = book("third-day", "USD", "", start.Add(time.Hour), big.NewRat(4, 1))
	venue := book("venue", "USD", "account", end, big.NewRat(1, 1))
	check("venue cash must not disappear", tenant, now(), false, base)
	check("venue cash as off-venue", tenant, now(), false, base, venue)
}
