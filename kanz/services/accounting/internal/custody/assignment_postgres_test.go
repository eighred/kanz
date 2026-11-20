package custody

import (
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The administrative fixture owns only this disposable DB. Reads and issuance
// use separate NOSUPERUSER/NOBYPASSRLS logins and the actual pre-signed inputs.
func TestDeclaredAccountIsProvenByDatedIndependentAuthorities(t *testing.T) {
	if os.Getenv("TEST_CUSTODY_ASSIGNMENT_FIXTURE") != "1" {
		t.Skip("requires disposable PostgreSQL fixture with three distinct restricted logins")
	}
	const tenant = "synthetic-custody-pilot"
	admin := newCustodyPool(t, tenant)
	requireRestrictedCustodyRole(t, admin)
	applyCustodySchema(t, admin)
	ctx := bus.WithTenantID(t.Context(), tenant)
	reader, venue, governance, write := assignmentIssuers(t, admin, tenant)
	st := NewPostgres(reader)
	sub := subject()
	sub.BusinessDate = BusinessDay(t0)
	ids := []string{"sandbox-account"}
	expectDenied := func(s Subject, accounts []string) {
		t.Helper()
		if _, err := st.ProvenAssignments(ctx, s, accounts); !errors.Is(err, ErrUnprovenAssignment) {
			t.Fatal("unexpected admission", s, err)
		}
	}
	expectDenied(sub, ids)
	deniedSQL := func(err error) {
		t.Helper()
		var db *pgconn.PgError
		if !errors.As(err, &db) || db.Code != "42501" {
			t.Fatalf("expected role/RLS refusal: %v", err)
		}
	}
	deniedSQL(write(reader, "venue_identity", nil))
	deniedSQL(write(venue, "venue_identity", func(e *AccountEvidence) { e.ExchangeAccountID = "another" }))
	deniedSQL(write(governance, "venue_identity", nil))
	if err := write(venue, "venue_identity", nil); err != nil {
		t.Fatal(err)
	}
	expectDenied(sub, ids)
	deniedSQL(write(governance, "custody_assignment", func(e *AccountEvidence) { e.PortfolioID = "other" }))
	if err := write(governance, "custody_assignment", nil); err != nil {
		t.Fatal(err)
	}
	valid := func(s Subject) []AccountAssignment {
		t.Helper()
		a, err := st.ProvenAssignments(ctx, s, ids)
		if err != nil || len(a) != 1 {
			t.Fatalf("no active approval: %+v %v", a, err)
		}
		return a
	}
	original := valid(sub)
	if original[0].Revision != 1 || original[0].Venue != "XSIM" || original[0].ExchangeAccountID != "xs-account-1" || original[0].RecordedAt == "" || original[0].ProofRecordedAt == "" {
		t.Fatal(original)
	}
	expectDenied(sub, []string{"mistyped"})
	other := sub
	other.CustodianID = "CUST-B"
	expectDenied(other, ids)
	other = sub
	other.PortfolioID = "PF2"
	expectDenied(other, ids)
	other = sub
	other.BusinessDate = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	expectDenied(other, ids)
	if _, err := st.ProvenAssignments(bus.WithTenantID(t.Context(), "other"), sub, ids); !errors.Is(err, ErrUnprovenAssignment) {
		t.Fatal("tenant", err)
	}
	// Changing the session tenant alone cannot grant a different issuer scope.
	conn, err := reader.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	func() {
		defer conn.Release()
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err = tx.Exec(ctx, "SELECT set_config('app.tenant_id','other',true)"); err != nil {
			t.Fatal(err)
		}
		var n int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM custody_account_proofs").Scan(&n); err != nil || n != 0 {
			t.Fatal("foreign rows", n, err)
		}
	}()
	basis := &RunBasis{Mode: BasisDeclared, Accounts: map[string][]string{"CUST-A": ids}, Assignments: original}
	historic := func(wantCurrent bool) {
		t.Helper()
		tx, err := reader.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		good, err := confirmRunAssignments(ctx, tx, sub, basis, true)
		if err != nil || !good {
			t.Fatal("original immutable receipt", good, err)
		}
		good, err = confirmRunAssignments(ctx, tx, sub, basis, false)
		if good != wantCurrent || (wantCurrent && err != nil) || (!wantCurrent && !errors.Is(err, ErrUnprovenAssignment)) {
			t.Fatal("current/readback disagreement", good, err)
		}
	}
	historic(true)
	if err := write(governance, "custody_assignment_withdrawn", nil); err != nil {
		t.Fatal(err)
	}
	expectDenied(sub, ids)
	historic(false)
	earlier := sub
	earlier.BusinessDate = time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC)
	if !slices.Equal(valid(earlier), original) {
		t.Fatal("withdrawal rewrote earlier date")
	}
	if err := write(governance, "custody_assignment_reinstated", nil); err != nil {
		t.Fatal(err)
	}
	if got := valid(sub); got[0].Revision != 3 || got[0].AssignmentDigest == original[0].AssignmentDigest {
		t.Fatal("renewal", got)
	}
	historic(false)
	// A scoped writer bypassing the importer still cannot invent a signed approval.
	if err := write(governance, "custody_assignment_reinstated", func(e *AccountEvidence) { e.Revision = 4; e.Version = "forged-v4" }); err != nil {
		t.Fatal(err)
	}
	expectDenied(sub, ids)
	historic(false)
	_, err = reader.Exec(ctx, `INSERT INTO custody_runs(run_id,portfolio_id,custodian_id,business_date,outcome,completed_at) VALUES('old-writer','PF1','CUST-A',$1,'clean',now())`, sub.BusinessDate)
	var db *pgconn.PgError
	if !errors.As(err, &db) || db.Code != "P0001" || db.Message != "enrolled accounting requires effective custody assignment evidence" {
		t.Fatal("old writer bypass", err)
	}
	var rows int
	if err := admin.QueryRow(ctx, "SELECT count(*) FROM ledger_entries").Scan(&rows); err != nil || rows != 0 {
		t.Fatal("fixture required journal entries", rows, err)
	}
}

func TestDeclaredAccountIsProvenOnlyOnIntersectionOfEffectiveWindows(t *testing.T) {
	day := BusinessDay(t0)
	for offset := -2; offset <= 2; offset++ {
		got := coveredAssignmentDay(day.AddDate(0, 0, offset), day, day.AddDate(0, 0, 1), day.AddDate(-1, 0, 0), day.AddDate(1, 0, 0))
		if got != (offset == 0) {
			t.Fatal("one-day venue identity within annual custody approval", offset, got)
		}
		got = coveredAssignmentDay(day.AddDate(0, 0, offset), day.AddDate(-1, 0, 0), day.AddDate(1, 0, 0), day, day.AddDate(0, 0, 1))
		if got != (offset == 0) {
			t.Fatal("one-day approval within annual venue identity", offset, got)
		}
	}
}

func assignmentIssuers(t *testing.T, admin *pgxpool.Pool, tenant string) (reader, venue, governance *pgxpool.Pool, write func(*pgxpool.Pool, string, func(*AccountEvidence)) error) {
	t.Helper()
	ctx := bus.WithTenantID(t.Context(), tenant)
	fixture := func(name string) []byte {
		t.Helper()
		b, err := os.ReadFile(filepath.Join("../../cmd/custody_proof/testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	exec := func(pool *pgxpool.Pool, q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(admin, `INSERT INTO custody_proof_issuers SELECT (r->>'login_role')::name,r->>'kind',r->>'tenant_id',r->>'authority',decode(r->>'public_key','base64') FROM jsonb_array_elements($1::jsonb) r`, string(fixture("issuers.json")))
	exec(admin, `INSERT INTO custody_proof_scopes(login_role,tenant_id,venue,account_id,exchange_account_id,portfolio_id,custodian_id) SELECT (r->>'login_role')::name,r->>'tenant_id',r->>'venue',r->>'account_id',r->>'exchange_account_id',r->>'portfolio_id',r->>'custodian_id' FROM jsonb_array_elements($1::jsonb) r`, string(fixture("scopes.json")))
	exec(admin, `INSERT INTO custody_assignment_readers VALUES('pilot_assignment_reader',$1)`, tenant)
	exec(admin, `GRANT SELECT ON custody_assignment_readers,custody_proof_scopes,custody_proof_issuers TO pilot_assignment_reader,pilot_venue_proof,pilot_custody_assignment;
 GRANT SELECT ON custody_account_proofs,custody_account_assignments TO pilot_assignment_reader;
 GRANT SELECT,INSERT ON custody_account_proofs TO pilot_venue_proof;
 GRANT SELECT,INSERT ON custody_account_assignments TO pilot_custody_assignment;
 GRANT INSERT ON custody_runs TO pilot_assignment_reader`)
	role := func(name string) *pgxpool.Pool {
		t.Helper()
		cfg := admin.Config().Copy()
		cfg.ConnConfig.User = name
		p, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(p.Close)
		requireRestrictedCustodyRole(t, p)
		return p
	}
	reader, venue, governance = role("pilot_assignment_reader"), role("pilot_venue_proof"), role("pilot_custody_assignment")
	write = func(p *pgxpool.Pool, name string, alter func(*AccountEvidence)) error {
		raw, sig := fixture(name+".json"), fixture(name+".sig")
		e, err := ParseAccountEvidence(raw)
		if err != nil {
			t.Fatal(err)
		}
		if alter != nil {
			alter(&e)
		}
		digest := sha256.Sum256(raw)
		args := []any{e.TenantID, e.AccountID, e.ProofID, e.Authority, e.EffectiveFrom, e.EffectiveUntil, raw, sig, digest[:]}
		if e.Kind == "venue_identity" {
			_, err = p.Exec(ctx, `INSERT INTO custody_account_proofs(tenant_id,account_id,proof_id,authority,effective_from,effective_until,evidence_payload,signature,evidence_sha256,venue,exchange_account_id) VALUES($1,$2,$3,$4,$5::text::date,$6::text::date,$7,$8,$9,$10,$11)`, append(args, e.Venue, e.ExchangeAccountID)...)
		} else {
			_, err = p.Exec(ctx, `INSERT INTO custody_account_assignments(tenant_id,account_id,proof_id,authority,effective_from,effective_until,evidence_payload,signature,evidence_sha256,portfolio_id,custodian_id,version,revision,withdrawn) VALUES($1,$2,$3,$4,$5::text::date,$6::text::date,$7,$8,$9,$10,$11,$12,$13,$14)`, append(args, e.PortfolioID, e.CustodianID, e.Version, e.Revision, e.Withdrawn)...)
		}
		return err
	}
	return
}
