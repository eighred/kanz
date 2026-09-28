package identity

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// RotateCredential commits the verified hash replacement, revocation fence and
// credential-free audit together. Expensive verification/derivation happens
// before acquiring locks; the exact verified hash is rechecked under the lock.
func (p *Postgres) RotateCredential(ctx context.Context, actor Administration, previous, replacement Hash, now time.Time) error {
	if actor.Subject == "" || actor.Tenant == "" || previous == "" || replacement == "" || previous == replacement {
		return ErrCredentialMismatch
	}
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Match administration's lock order, including when the subject is an admin.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('identity-administration:' || $1, 0))`, actor.Tenant); err != nil {
		return err
	}
	var u User
	err = tx.QueryRow(ctx, `SELECT subject,tenant_id,roles,portfolios,status,credential_hash,session_epoch,tokens_invalid_before FROM identity_users WHERE subject=$1 AND tenant_id=$2 FOR UPDATE`, actor.Subject, actor.Tenant).Scan(&u.Subject, &u.Tenant, &u.Roles, &u.Portfolios, &u.Status, &u.Credential, &u.SessionEpoch, &u.TokensInvalidBefore)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrCredentialMismatch
	}
	if err != nil {
		return err
	}
	if !actor.Current(&u) || u.Credential != previous {
		return ErrCredentialMismatch
	}
	before, err := lockAccess(ctx, tx, actor.Subject, actor.Tenant)
	if err != nil {
		return err
	}
	after, err := scanAccess(tx.QueryRow(ctx, `UPDATE identity_users u SET credential_hash=$3,session_epoch=session_epoch+1,access_revision=access_revision+1,updated_at=$4,tokens_invalid_before=GREATEST(tokens_invalid_before,$4::timestamptz) WHERE subject=$1 AND tenant_id=$2 RETURNING `+accessColumns, actor.Subject, actor.Tenant, string(replacement), now.UTC()))
	if err != nil {
		return err
	}
	if err = recordAccess(ctx, tx, actor, &u, "credential.rotate", before, after, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
