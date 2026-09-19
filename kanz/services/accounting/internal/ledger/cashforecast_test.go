package ledger

import (
	"errors"
	"math/big"
	"testing"
	"time"
)

func TestPostgresCashForecastKnownInputsAndReplay(t *testing.T) {
	pool := newPool(t)
	var privileged bool
	if err := pool.QueryRow(t.Context(), `SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&privileged); err != nil || privileged {
		t.Fatalf("non-privileged RLS role required: %v %v", privileged, err)
	}
	p := NewPostgres(pool)
	asOf := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)
	tomorrow := asOf.Add(24 * time.Hour)
	for _, row := range []struct {
		id, ccy    string
		amount     int64
		basis      SettlementBasis
		due, known time.Time
	}{
		{"opening", "USD", 100, SettlementSettled, asOf.Add(-time.Hour), asOf},
		{"receipt", "USD", 80, SettlementPending, tomorrow, asOf},
		{"payment", "USD", -140, SettlementPending, tomorrow, asOf},
		{"other-currency", "EUR", 999, SettlementSettled, asOf, asOf},
		{"late-correction", "USD", 10000, SettlementSettled, asOf, asOf.Add(time.Hour)},
	} {
		err := p.Append(t.Context(), &Event{EntryID: row.id, PortfolioID: "PF", Type: EntryCash, Cash: big.NewRat(row.amount, 1), CashCurrency: row.ccy,
			Effective: asOf.Add(-time.Hour), Knowledge: row.known, SettlementBasis: row.basis, SettlementDate: row.due}, nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	f, err := p.ForecastCash(t.Context(), "PF", "USD", asOf, tomorrow)
	if err != nil {
		t.Fatal(err)
	}
	if f.Opening == nil || *f.Opening != "100" || f.ConservativeBalance == nil || *f.ConservativeBalance != "-40" || len(f.Flows) != 2 || f.Complete {
		t.Fatalf("expected receipts were spent or coverage invented: %+v", f)
	}
	replayed, err := NewPostgres(pool).ForecastCash(t.Context(), "PF", "USD", asOf, tomorrow)
	if err != nil || replayed.SourceVersion != f.SourceVersion {
		t.Fatalf("replay changed: %v %+v", err, replayed)
	}
	newKnowledge, err := p.ForecastCash(t.Context(), "PF", "USD", asOf.Add(time.Hour), tomorrow)
	if err != nil || newKnowledge.Opening == nil || *newKnowledge.Opening != "10100" || newKnowledge.SourceVersion == f.SourceVersion {
		t.Fatalf("late correction lost: %v %+v", err, newKnowledge)
	}
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	if _, err = tx.Exec(t.Context(), `SELECT set_config('app.tenant_id','other-tenant',true)`); err != nil {
		t.Fatal(err)
	}
	other, err := p.withTx(tx).ForecastCash(t.Context(), "PF", "USD", asOf, tomorrow)
	if err != nil || len(other.Flows) != 0 || other.Opening == nil || *other.Opening != "0" || other.Complete {
		t.Fatalf("tenant leak: %v %+v", err, other)
	}
}

func TestRecordedCashUnknownAndOverdueAreNotAvailable(t *testing.T) {
	asOf := time.Now().UTC()
	for _, basis := range []SettlementBasis{SettlementUnknown, SettlementPending, SettlementSettled} {
		due := asOf.Add(-time.Hour)
		if basis == SettlementSettled {
			due = asOf.Add(time.Hour)
		}
		f, err := projectRecordedCash("PF", "USD", asOf, asOf.Add(24*time.Hour), []forecastEntry{{ID: "ambiguous", Cash: "9007199254740993/100", Effective: asOf, Knowledge: asOf, Basis: basis, Settlement: &due}})
		if err != nil || f.Opening != nil || f.ConservativeBalance != nil || len(f.Reasons) != 2 || f.Complete {
			t.Fatalf("unknown became money: %v %+v", err, f)
		}
	}
}

func TestPostgresCashForecastCapacityFailsWithoutPartialResult(t *testing.T) {
	pool := newPool(t)
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	if _, err = tx.Exec(t.Context(), `SELECT set_config('app.venue_account_id','',true)`); err != nil {
		t.Fatal(err)
	}
	asOf := time.Now().UTC().Add(-time.Hour)
	_, err = tx.Exec(t.Context(), `INSERT INTO ledger_entries(entry_id,portfolio_id,entry_type,cash,cash_currency,effective_time,knowledge_time)
		SELECT 'cash-'||n,'PF',2,'1','USD',$1,$1 FROM generate_series(1,$2) n`, asOf, forecastEntryLimit+1)
	if err != nil {
		t.Fatal(err)
	}
	f, err := NewPostgres(pool).withTx(tx).ForecastCash(t.Context(), "PF", "USD", asOf, asOf.Add(time.Hour))
	if !errors.Is(err, ErrForecastCapacity) || f.SourceVersion != "" {
		t.Fatalf("truncated journal presented as forecast: %v %+v", err, f)
	}
}
