// Command custody_proof imports one independently signed historical venue identity
// or approved custody assignment using its scope-restricted database login.
package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/eighred/kanz/internal/lifecycle"
	"github.com/eighred/kanz/internal/pg"
	"github.com/eighred/kanz/pkg/secret"
	"github.com/eighred/kanz/services/accounting/internal/custody"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() { os.Exit(run()) }

func run() int {
	kind := flag.String("kind", "", "venue_identity or custody_assignment")
	evidence := flag.String("evidence", "", "signed JSON file")
	signature := flag.String("signature", "", "detached raw Ed25519 signature file")
	flag.Parse()
	if flag.NArg() != 0 {
		flag.Usage()
		return 2
	}
	signals, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	fatal := lifecycle.NewFatal(stop)
	ctx, cancel := context.WithTimeout(signals, 30*time.Second)
	defer cancel()
	if err := importSignedEvidence(ctx, *kind, *evidence, *signature); err != nil {
		fmt.Fprintln(os.Stderr, err)
		fatal.Raise(err)
	}
	return fatal.Code()
}

func readBounded(path string, limit int64) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("custody proof: bounded input file required")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("custody proof: input file unavailable")
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(b)) > limit {
		return nil, errors.New("custody proof: input file unavailable or over limit")
	}
	return b, nil
}

func importSignedEvidence(ctx context.Context, kind, evidenceFile, signatureFile string) error {
	raw, err := readBounded(evidenceFile, custody.AccountEvidenceLimit)
	if err != nil {
		return err
	}
	sig, err := readBounded(signatureFile, ed25519.SignatureSize)
	if err != nil {
		return err
	}
	e, err := custody.ParseAccountEvidence(raw)
	tenant := os.Getenv("ACCOUNTING_TENANT")
	if err != nil || tenant == "" || e.TenantID != tenant || e.Kind != kind {
		return errors.New("custody proof: signed evidence type or tenant mismatch")
	}
	dsn, err := secret.Read("ACCOUNTING_DATABASE_URL")
	if err != nil || dsn == "" {
		return errors.New("custody proof: explicit issuer database connection required")
	}
	pool, err := pg.NewTenantPool(ctx, dsn, tenant)
	if err != nil {
		return errors.New("custody proof: issuer connection unavailable")
	}
	defer pool.Close()
	return importEvidence(ctx, pool, e, raw, sig)
}

// Resolve Accounting independently of the issuer's connection search_path.
// The scalar subquery requires a single ledger even when the issuer cannot
// access a second schema. Never select the first visible matching ledger.
const accountingNamespace = `SELECT n.oid, n.nspname FROM pg_catalog.pg_namespace n
    WHERE n.oid=(
        SELECT t.relnamespace FROM pg_catalog.pg_class t
        JOIN pg_catalog.pg_namespace ns ON ns.oid=t.relnamespace
        WHERE t.relname='ledger_entries' AND t.relkind IN ('r','p')
          AND ns.nspname !~ '^pg_' AND ns.nspname <> 'information_schema'
    )
      AND pg_catalog.has_schema_privilege(session_user,n.oid,'USAGE')
      AND NOT pg_catalog.has_schema_privilege(session_user,n.oid,'CREATE')
      AND NOT pg_catalog.pg_has_role(session_user,n.nspowner,'USAGE')`

const authorizedIssuer = `SELECT i.public_key
    FROM %[1]s.custody_proof_issuers i
    JOIN %[1]s.custody_proof_scopes s ON s.tenant_id=i.tenant_id AND s.login_role=i.login_role
    JOIN pg_catalog.pg_roles r ON r.rolname=i.login_role
    WHERE i.login_role=session_user AND current_user=session_user
      AND NOT (r.rolsuper OR r.rolbypassrls OR r.rolcreaterole OR r.rolcreatedb OR r.rolreplication)
      AND i.tenant_id=$1 AND i.kind=$2 AND i.authority=$3
      AND s.account_id=$4 AND s.venue=$5 AND s.exchange_account_id=$6 AND s.portfolio_id=$7 AND s.custodian_id=$8
      AND NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_class t
        WHERE t.relnamespace=$9::oid AND t.relkind IN ('r','p') AND (
          has_table_privilege(session_user,t.oid,'UPDATE,DELETE,TRUNCATE,TRIGGER')
          OR has_any_column_privilege(session_user,t.oid,'UPDATE')
          OR (t.relname<>CASE WHEN i.kind='venue_identity' THEN 'custody_account_proofs' ELSE 'custody_account_assignments' END
              AND has_any_column_privilege(session_user,t.oid,'INSERT'))
        )
      )`

func pinAccountingNamespace(ctx context.Context, tx pgx.Tx) (uint32, string, error) {
	if _, err := tx.Exec(ctx, "SET LOCAL search_path = pg_catalog, pg_temp"); err != nil {
		return 0, "", errors.New("custody proof: unable to secure transaction scope")
	}
	var namespace uint32
	var name string
	if err := tx.QueryRow(ctx, accountingNamespace).Scan(&namespace, &name); err != nil {
		return 0, "", errors.New("custody proof: accounting schema unknown or issuer schema privileges refused")
	}
	schema := pgx.Identifier{name}.Sanitize()
	if _, err := tx.Exec(ctx, "SELECT pg_catalog.set_config('search_path', $1, true)", "pg_catalog,"+schema+",pg_temp"); err != nil {
		return 0, "", errors.New("custody proof: unable to secure transaction scope")
	}
	return namespace, schema, nil
}

func importEvidence(ctx context.Context, pool *pgxpool.Pool, e custody.AccountEvidence, raw, sig []byte) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return errors.New("custody proof: database unavailable")
	}
	defer func() { _ = tx.Rollback(ctx) }()
	namespace, schema, err := pinAccountingNamespace(ctx, tx)
	if err != nil {
		return err
	}
	var key []byte
	if err = tx.QueryRow(ctx, fmt.Sprintf(authorizedIssuer, schema), e.TenantID, e.Kind, e.Authority, e.AccountID, e.Venue, e.ExchangeAccountID, e.PortfolioID, e.CustodianID, namespace).Scan(&key); err != nil {
		return errors.New("custody proof: issuer role or scope refused")
	}
	if !custody.VerifyAccountEvidence(e, raw, sig, key) {
		return errors.New("custody proof: signature refused")
	}
	digest := sha256.Sum256(raw)
	if err = appendEvidence(ctx, tx, schema, e, raw, sig, digest[:]); err != nil {
		return errors.New("custody proof: evidence conflicts with its identity, assignment history or database grant")
	}
	if err = tx.Commit(ctx); err != nil {
		return errors.New("custody proof: commit unconfirmed; replay identical input")
	}
	return nil
}

// Replay accepts only the identical signed payload on the same immutable row.
func appendEvidence(ctx context.Context, tx pgx.Tx, schema string, e custody.AccountEvidence, raw, sig, digest []byte) error {
	var insert, lookup string
	args := []any{e.TenantID, e.AccountID, e.ProofID, e.Authority, e.EffectiveFrom, e.EffectiveUntil, digest, raw, sig}
	switch e.Kind {
	case "venue_identity":
		args = append(args, e.Venue, e.ExchangeAccountID)
		insert = `INSERT INTO ` + schema + `.custody_account_proofs(tenant_id,account_id,proof_id,authority,effective_from,effective_until,evidence_sha256,evidence_payload,signature,venue,exchange_account_id)
            VALUES ($1,$2,$3,$4,$5::text::date,$6::text::date,$7,$8,$9,$10,$11) ON CONFLICT DO NOTHING`
		lookup = `SELECT evidence_payload FROM ` + schema + `.custody_account_proofs
            WHERE tenant_id=$1 AND account_id=$2 AND proof_id=$3 AND authority=$4 AND effective_from=$5::text::date AND effective_until=$6::text::date
            AND evidence_sha256=$7 AND evidence_payload=$8 AND signature=$9 AND venue=$10 AND exchange_account_id=$11 AND issuer_role=session_user`
	case "custody_assignment":
		args = append(args, e.PortfolioID, e.CustodianID, e.Version, e.Revision, e.Withdrawn)
		insert = `INSERT INTO ` + schema + `.custody_account_assignments(tenant_id,account_id,proof_id,authority,effective_from,effective_until,evidence_sha256,evidence_payload,signature,portfolio_id,custodian_id,version,revision,withdrawn)
            VALUES ($1,$2,$3,$4,$5::text::date,$6::text::date,$7,$8,$9,$10,$11,$12,$13,$14) ON CONFLICT DO NOTHING`
		lookup = `SELECT evidence_payload FROM ` + schema + `.custody_account_assignments
            WHERE tenant_id=$1 AND account_id=$2 AND proof_id=$3 AND authority=$4 AND effective_from=$5::text::date AND effective_until=$6::text::date
            AND evidence_sha256=$7 AND evidence_payload=$8 AND signature=$9 AND portfolio_id=$10 AND custodian_id=$11 AND version=$12 AND revision=$13 AND withdrawn=$14 AND issuer_role=session_user`
	default:
		return errors.New("unknown account evidence")
	}
	// A BEFORE INSERT trigger may properly reject an already published revision.
	// Check replay first, then serialize publication and read the eventual winner.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1), hashtext($2))`, e.TenantID, "custody-evidence:"+e.AccountID); err != nil {
		return err
	}
	var retained []byte
	err := tx.QueryRow(ctx, lookup, args...).Scan(&retained)
	if err == nil && bytes.Equal(retained, raw) {
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return errors.New("immutable evidence differs")
	}
	if _, err = tx.Exec(ctx, insert, args...); err != nil {
		return err
	}
	return tx.QueryRow(ctx, lookup, args...).Scan(&retained)
}
