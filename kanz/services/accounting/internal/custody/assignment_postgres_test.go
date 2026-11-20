package custody

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The administrative fixture owns only this disposable DB. Reads and issuance
// use separate NOSUPERUSER/NOBYPASSRLS logins and the actual pre-signed inputs.
func TestDeclaredAccountIsProvenByDatedIndependentAuthorities(t *testing.T) {
	const tenant = "synthetic-custody-pilot"
	admin, reader, venue, governance := assignmentTestPools(t, tenant)
	ctx := bus.WithTenantID(t.Context(), tenant)
	write := signedAccountFixture(t, admin, ctx)
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
	// Seed another tenant's actual registry rows. Both issuer logins and the
	// reader must keep their own authority when selecting a different tenant.
	if _, err := admin.Exec(ctx, `INSERT INTO custody_assignment_readers VALUES('foreign_reader','other');
 INSERT INTO custody_proof_issuers VALUES('foreign_issuer','venue_identity','other','foreign-authority',sha256('foreign'::bytea));
 INSERT INTO custody_proof_scopes(login_role,tenant_id,venue,account_id,exchange_account_id)
 VALUES('foreign_issuer','other','XSIM','foreign-account','foreign-exchange')`); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		pool    *pgxpool.Pool
		issuers []string
		readers []string
	}{
		{"reader", reader, []string{"pilot_custody_assignment", "pilot_venue_proof"}, []string{"pilot_assignment_reader"}},
		{"venue issuer", venue, []string{"pilot_venue_proof"}, nil},
		{"assignment issuer", governance, []string{"pilot_custody_assignment"}, nil},
	} {
		t.Run("registry/"+tc.name, func(t *testing.T) {
			tx, err := tc.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			for _, selected := range []string{tenant, "other"} {
				if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id',$1,true)", selected); err != nil {
					t.Fatal(err)
				}
				var issuers, scopes, readers []string
				var enrolled, canProvision bool
				err := tx.QueryRow(ctx, `SELECT
 ARRAY(SELECT login_role::text FROM custody_proof_issuers ORDER BY 1),
 ARRAY(SELECT login_role::text FROM custody_proof_scopes ORDER BY 1),
 ARRAY(SELECT login_role::text FROM custody_assignment_readers ORDER BY 1),
 custody_assignment_reader_enrolled(),
 EXISTS(SELECT FROM unnest(ARRAY['custody_proof_issuers','custody_proof_scopes','custody_assignment_readers']) AS r(name)
 WHERE has_table_privilege(current_user,r.name,'INSERT,UPDATE,DELETE,TRUNCATE'))`).Scan(&issuers, &scopes, &readers, &enrolled, &canProvision)
				if err != nil {
					t.Fatal(err)
				}
				wantIssuers := tc.issuers
				if selected != tenant {
					wantIssuers = nil
				}
				if !slices.Equal(issuers, wantIssuers) || !slices.Equal(scopes, wantIssuers) || !slices.Equal(readers, tc.readers) ||
					enrolled != (tc.pool == reader && selected == tenant) || canProvision {
					t.Fatalf("tenant %s: issuers %v scopes %v readers %v enrolled %v provision %v", selected, issuers, scopes, readers, enrolled, canProvision)
				}
				if tc.pool == reader && selected != tenant {
					var proofs, assignments int
					if err := tx.QueryRow(ctx, "SELECT (SELECT count(*) FROM custody_account_proofs),(SELECT count(*) FROM custody_account_assignments)").Scan(&proofs, &assignments); err != nil || proofs != 0 || assignments != 0 {
						t.Fatal("foreign custody evidence", proofs, assignments, err)
					}
				}
			}
		})
	}
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
	// Materialize an independent, signed zero-inventory statement for the real
	// scoped book. Enroll an older receipt and then run the proven comparator.
	feed := signedStatementFeedForAccount(t, ctx, NewPostgres(admin), ids[0])
	statementID := feed(json.RawMessage(fmt.Sprintf(`{"portfolioId":"PF1","custodianId":"CUST-A","businessDate":"%s","positions":[],"cash":[],"transactions":[],"grain":"STATEMENT_GRAIN_TRANSACTIONS"}`, sub.BusinessDate.Format(time.RFC3339))))
	scope, err := NewBookScope([]Subject{sub}, map[string]map[string][]string{sub.PortfolioID: basis.Accounts})
	if err != nil {
		t.Fatal(err)
	}
	base := LedgerBookLoader(ledger.NewPostgres(admin), scope, testLogger())
	now := time.Now().UTC()
	comparator := func(store *Postgres, loader BookLoader) *Reconciler {
		t.Helper()
		r, err := NewReconciler(store, loader, &capturePublisher{}, nil, nil, testLogger(), func() time.Time { return now })
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	legacy, err := comparator(NewPostgres(admin), base).Reconcile(ctx, sub)
	if err != nil || legacy.Outcome != OutcomeClean {
		t.Fatal("legacy source", legacy, err)
	}
	checkCurrent := func(state, reason string, verified bool) {
		t.Helper()
		res, err := st.DatedResult(ctx, sub)
		if err != nil || res.State != state || res.Reason != reason || res.StatementID != statementID || res.AccountAssignmentVerified != verified {
			t.Fatal("current recorded assignment", res, err)
		}
	}
	checkCurrent("pending", "assignment_unproven", false)
	saved, err := st.HistoricalReceipt(ctx, sub, ids[0], legacy.RunID)
	if err != nil || saved.State != "recorded" || saved.AccountAssignmentVerified {
		t.Fatal("legacy history", saved, err)
	}
	proven := func(ctx context.Context, s Subject) (*ledger.Book, []ledger.Execution, *RunBasis, error) {
		book, executions, b, err := base(ctx, s)
		if err == nil {
			b.Assignments, err = st.ProvenPortfolioAssignments(ctx, s, b.Accounts)
		}
		return book, executions, b, err
	}
	now = now.Add(time.Second)
	completed, err := comparator(st, proven).Reconcile(ctx, sub)
	if err != nil || completed.Outcome != OutcomeClean {
		t.Fatal("proven zero-holdings comparison", completed, err)
	}
	checkCurrent("recorded", "", true)
	checkSaved := func() {
		t.Helper()
		got, err := st.HistoricalReceipt(ctx, sub, ids[0], completed.RunID)
		if err != nil || got.State != "recorded" || !got.AccountAssignmentVerified || !slices.Equal(got.Run.Basis.Assignments, original) {
			t.Fatal("historical effective assignment", got, err)
		}
	}
	checkSaved()
	historic(true)
	if err := write(governance, "custody_assignment_withdrawn", nil); err != nil {
		t.Fatal(err)
	}
	expectDenied(sub, ids)
	checkCurrent("pending", "assignment_changed", false)
	checkSaved()
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
	checkCurrent("pending", "assignment_changed", false)
	checkSaved()
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

func signedAccountFixture(t *testing.T, admin *pgxpool.Pool, ctx context.Context) func(*pgxpool.Pool, string, func(*AccountEvidence)) error {
	t.Helper()
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
	return func(p *pgxpool.Pool, name string, alter func(*AccountEvidence)) error {
		raw, sig := fixture(name+".json"), fixture(name+".sig")
		e, err := ParseAccountEvidence(raw)
		if err != nil {
			t.Fatal(err)
		}
		if alter != nil {
			alter(&e)
		}
		return insertFixtureEvidence(ctx, p, e, raw, sig)
	}
}

func assignmentTestPools(t *testing.T, tenant string) (*pgxpool.Pool, *pgxpool.Pool, *pgxpool.Pool, *pgxpool.Pool) {
	t.Helper()
	if os.Getenv("TEST_CUSTODY_ASSIGNMENT_FIXTURE") != "1" {
		t.Skip("requires disposable PostgreSQL fixture with three distinct restricted logins")
	}
	admin := newCustodyPool(t, tenant)
	requireRestrictedCustodyRole(t, admin)
	applyCustodySchema(t, admin)
	reader, venue, governance := assignmentRolePools(t, admin, tenant)
	return admin, reader, venue, governance
}

func assignmentRolePools(t *testing.T, admin *pgxpool.Pool, tenant string) (*pgxpool.Pool, *pgxpool.Pool, *pgxpool.Pool) {
	t.Helper()
	ctx := bus.WithTenantID(t.Context(), tenant)
	exec := func(pool *pgxpool.Pool, q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(admin, `INSERT INTO custody_assignment_readers VALUES('pilot_assignment_reader',$1)`, tenant)
	exec(admin, `GRANT SELECT ON custody_assignment_readers,custody_proof_scopes,custody_proof_issuers TO pilot_assignment_reader,pilot_venue_proof,pilot_custody_assignment;
 GRANT SELECT ON custody_account_proofs,custody_account_assignments TO pilot_assignment_reader;
 GRANT SELECT,INSERT ON custody_account_proofs TO pilot_venue_proof;
 GRANT SELECT,INSERT ON custody_account_assignments TO pilot_custody_assignment;
 GRANT SELECT,INSERT ON custody_runs TO pilot_assignment_reader;
 GRANT SELECT ON custody_statements,custody_batch_receipts,ledger_heads,ledger_entries,ledger_append_positions,custody_break_detections TO pilot_assignment_reader;
 GRANT SELECT,INSERT,UPDATE ON custody_breaks,outbox TO pilot_assignment_reader;
 GRANT SELECT,INSERT ON custody_execution_quantity_observations TO pilot_assignment_reader;
 GRANT USAGE ON SEQUENCE custody_execution_quantity_observations_observation_seq TO pilot_assignment_reader;
 GRANT USAGE,SELECT ON SEQUENCE outbox_id_seq TO pilot_assignment_reader`)
	role := func(name string) *pgxpool.Pool {
		t.Helper()
		cfg := admin.Config().Copy()
		cfg.ConnConfig.User = name
		if password, ok := os.LookupEnv(map[string]string{"pilot_assignment_reader": "TEST_CUSTODY_ASSIGNMENT_READER_PASSWORD", "pilot_venue_proof": "TEST_CUSTODY_VENUE_PROOF_PASSWORD", "pilot_custody_assignment": "TEST_CUSTODY_ASSIGNMENT_ISSUER_PASSWORD"}[name]); ok {
			cfg.ConnConfig.Password = password
		}
		if name == "pilot_assignment_reader" {
			cfg.ConnConfig.RuntimeParams["default_transaction_isolation"] = "repeatable read"
		}
		p, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(p.Close)
		requireRestrictedCustodyRole(t, p)
		return p
	}
	return role("pilot_assignment_reader"), role("pilot_venue_proof"), role("pilot_custody_assignment")
}

type fixtureEvidenceWriter interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

func insertFixtureEvidence(ctx context.Context, p fixtureEvidenceWriter, e AccountEvidence, raw, sig []byte) error {
	var err error
	digest := sha256.Sum256(raw)
	args := []any{e.TenantID, e.AccountID, e.ProofID, e.Authority, e.EffectiveFrom, e.EffectiveUntil, raw, sig, digest[:]}
	if e.Kind == "venue_identity" {
		_, err = p.Exec(ctx, `INSERT INTO custody_account_proofs(tenant_id,account_id,proof_id,authority,effective_from,effective_until,evidence_payload,signature,evidence_sha256,venue,exchange_account_id) VALUES($1,$2,$3,$4,$5::text::date,$6::text::date,$7,$8,$9,$10,$11)`, append(args, e.Venue, e.ExchangeAccountID)...)
	} else {
		_, err = p.Exec(ctx, `INSERT INTO custody_account_assignments(tenant_id,account_id,proof_id,authority,effective_from,effective_until,evidence_payload,signature,evidence_sha256,portfolio_id,custodian_id,version,revision,withdrawn) VALUES($1,$2,$3,$4,$5::text::date,$6::text::date,$7,$8,$9,$10,$11,$12,$13,$14)`, append(args, e.PortfolioID, e.CustodianID, e.Version, e.Revision, e.Withdrawn)...)
	}
	return err
}
func TestDeclaredAccountIsProvenAgainAfterConcurrentAccountEnrollment(t *testing.T) {
	const tenant = "concurrent-custody"
	admin, reader, venue, governance := assignmentTestPools(t, tenant)
	ctx := bus.WithTenantID(t.Context(), tenant)
	// Both synthetic signers have two named grants. A complete declaration must
	// stop being sufficient when another authorized account is committed.
	venueKey, venueSecret, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	approvalKey, approvalSecret, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = admin.Exec(ctx, `INSERT INTO custody_proof_issuers VALUES('pilot_venue_proof','venue_identity',$1,'venue-observer',$2),('pilot_custody_assignment','custody_assignment',$1,'custody-governance',$3)`, tenant, []byte(venueKey), []byte(approvalKey))
	if err != nil {
		t.Fatal(err)
	}
	issue := func(w fixtureEvidenceWriter, e AccountEvidence, k ed25519.PrivateKey) {
		t.Helper()
		raw, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		if err := insertFixtureEvidence(ctx, w, e, raw, ed25519.Sign(k, raw)); err != nil {
			t.Fatal(err)
		}
	}
	approval := func(id string) AccountEvidence {
		return AccountEvidence{Kind: "custody_assignment", TenantID: tenant, Authority: "custody-governance", PortfolioID: "PF1", CustodianID: "CUST-A", AccountID: id, ProofID: "proof-" + id, Version: "assignment-" + id, Revision: 1, EffectiveFrom: "2020-01-01", EffectiveUntil: "2030-01-01"}
	}
	for _, id := range []string{"A", "B"} {
		_, err = admin.Exec(ctx, `INSERT INTO custody_proof_scopes(login_role,tenant_id,venue,account_id,exchange_account_id,portfolio_id,custodian_id) VALUES('pilot_venue_proof',$1,'XSIM',$2,$3,'',''),('pilot_custody_assignment',$1,'',$2,'','PF1','CUST-A')`, tenant, id, "exchange-"+id)
		if err != nil {
			t.Fatal(err)
		}
		issue(venue, AccountEvidence{Kind: "venue_identity", TenantID: tenant, Authority: "venue-observer", Venue: "XSIM", AccountID: id, ExchangeAccountID: "exchange-" + id, ProofID: "proof-" + id, EffectiveFrom: "2020-01-01", EffectiveUntil: "2030-01-01"}, venueSecret)
	}
	issue(governance, approval("A"), approvalSecret)
	sub := subject()
	sub.BusinessDate = BusinessDay(t0)
	st := NewPostgres(reader)
	accounts := map[string][]string{"CUST-A": {"A"}}
	current, err := st.ProvenPortfolioAssignments(ctx, sub, accounts)
	if err != nil || len(current) != 1 {
		t.Fatal(current, err)
	}
	var isolation string
	if err := reader.QueryRow(ctx, "SHOW default_transaction_isolation").Scan(&isolation); err != nil || isolation != "repeatable read" {
		t.Fatal(isolation, err)
	}
	tx, err := governance.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	issue(tx, approval("B"), approvalSecret)
	// The enrollment BEFORE INSERT now holds the production tenant lock.
	var holder int
	if err := tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&holder); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := st.CommitRun(ctx, Run{Subject: sub, RunID: "prior-fold", Outcome: OutcomeClean, CompletedAt: time.Now().UTC(), Basis: &RunBasis{Mode: BasisDeclared, Accounts: accounts, Assignments: current}})
		result <- err
	}()
	deadline := time.Now().Add(4 * time.Second)
	for {
		var waiting bool
		err = admin.QueryRow(ctx, `SELECT EXISTS(SELECT FROM pg_locks owner JOIN pg_locks waiter USING(locktype,database,classid,objid,objsubid)
   WHERE owner.pid=$1 AND owner.granted AND NOT waiter.granted AND owner.locktype='advisory')`, holder).Scan(&waiting)
		if err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("comparison did not wait on authority enrollment")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, ErrUnprovenAssignment) {
		t.Fatal("committed stale complete-set observation", err)
	}
	if current, err := st.ProvenPortfolioAssignments(ctx, sub, map[string][]string{"CUST-A": {"A", "B"}}); err != nil || len(current) != 2 {
		t.Fatal("new complete set", current, err)
	}
	var count int
	if err := reader.QueryRow(ctx, "SELECT count(*) FROM custody_runs WHERE run_id='prior-fold'").Scan(&count); err != nil || count != 0 {
		t.Fatal("stale run left durable state", count, err)
	}
}
