package identity

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// WhyNoTenantScope is the reason this store opens an UNSCOPED pool, passed to
// internal/pg.NewGlobalPool by whichever composition root builds it.
//
// It is exported so the reason lives with the store rather than being retyped —
// differently — at each call site. NewGlobalPool refuses to open without one,
// which is what keeps "deliberately unscoped" distinguishable from "forgot to
// scope it".
const WhyNoTenantScope = "identity: login must find an account BEFORE it knows the account's tenant, so a " +
	"tenant-bound pool could only ever find users of the wrong tenant. Tenancy is carried on the issued " +
	"token instead, and every downstream store is RLS-scoped by it."

// Postgres is the durable user + invite store.
type Postgres struct {
	pool *pgxpool.Pool
}

// NewPostgres wraps a pool. The pool must be an UNSCOPED one — see
// WhyNoTenantScope.
func NewPostgres(pool *pgxpool.Pool) *Postgres { return &Postgres{pool: pool} }

// CreateInvite stores an operator's offer of an account.
//
// The unique partial index on (subject) WHERE redeemed_at IS NULL means a second
// live invite for the same person is refused by the DATABASE rather than by a
// check that races. Two live invites are two authorities competing to become one
// account, and the loser is silent.
func (p *Postgres) CreateInvite(ctx context.Context, inv *Invite) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO identity_invites
			(id, token_hash, subject, tenant_id, roles, portfolios, created_by, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		inv.ID, inv.TokenHash, inv.Subject, inv.Tenant, inv.Roles, inv.Portfolios,
		inv.CreatedBy, inv.CreatedAt, inv.ExpiresAt)
	if err != nil {
		return fmt.Errorf("identity: create invite: %w", err)
	}
	return nil
}

// Redeem exchanges a raw invite token for an account, atomically.
//
// THE SINGLE-USE GUARANTEE IS ONE CONDITIONAL UPDATE, NOT A READ-THEN-WRITE.
// `WHERE redeemed_at IS NULL` inside a transaction means two concurrent
// redemptions of the same token cannot both win: the second updates zero rows
// and is refused. A load-check-save in application code would let both pass the
// check before either wrote, and the result is two accounts — or one account
// whose credential is whichever redemption committed last.
//
// The credential is supplied by the redeemer; every claim comes from the invite.
func (p *Postgres) Redeem(ctx context.Context, rawToken string, cred Hash, now time.Time) (*User, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("identity: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var inv Invite
	err = tx.QueryRow(ctx, `
		UPDATE identity_invites
		   SET redeemed_at = $2
		 WHERE token_hash = $1
		   AND redeemed_at IS NULL
		   AND expires_at > $2
		RETURNING id, subject, tenant_id, roles, portfolios, created_by, created_at, expires_at`,
		InviteTokenHash(rawToken), now.UTC(),
	).Scan(&inv.ID, &inv.Subject, &inv.Tenant, &inv.Roles, &inv.Portfolios,
		&inv.CreatedBy, &inv.CreatedAt, &inv.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// ONE ERROR FOR THREE STATES — unknown, expired, already redeemed. The
		// caller here is unauthenticated and holds only a link; telling them
		// "that invite exists but expired" confirms an account was offered to
		// someone. The operator-facing list distinguishes them.
		return nil, ErrInviteNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("identity: redeem: %w", err)
	}

	u := UserFromInvite(&inv, cred, now)
	if _, err := tx.Exec(ctx, `
		INSERT INTO identity_users
			(subject, tenant_id, roles, portfolios, credential_hash, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		u.Subject, u.Tenant, u.Roles, u.Portfolios, string(u.Credential), string(u.Status),
		u.CreatedAt, u.UpdatedAt); err != nil {
		// The rollback undoes the redemption stamp too, so a failure here leaves
		// the invite USABLE rather than burnt with no account to show for it.
		return nil, fmt.Errorf("identity: create user: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("identity: commit: %w", err)
	}
	return u, nil
}

// UserBySubject loads an account for the login path.
func (p *Postgres) UserBySubject(ctx context.Context, subject string) (*User, error) {
	var u User
	var status string
	err := p.pool.QueryRow(ctx, `
		SELECT subject, tenant_id, roles, portfolios, credential_hash, status, created_at, updated_at
		  FROM identity_users WHERE subject = $1`, subject,
	).Scan(&u.Subject, &u.Tenant, &u.Roles, &u.Portfolios, &u.Credential, &status,
		&u.CreatedAt, &u.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("identity: load user: %w", err)
	}
	u.Status = Status(status)
	return &u, nil
}

// UpdateCredential rewrites an account's credential — the rehash-on-login
// upgrade path (see NeedsRehash), and the only write the login path makes.
func (p *Postgres) UpdateCredential(ctx context.Context, subject string, cred Hash, now time.Time) error {
	tag, err := p.pool.Exec(ctx, `
		UPDATE identity_users SET credential_hash = $2, updated_at = $3 WHERE subject = $1`,
		subject, string(cred), now.UTC())
	if err != nil {
		return fmt.Errorf("identity: update credential: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrUserNotFound
	}
	return nil
}

// SetStatus moves an account between active and disabled — the write that makes
// identity.StatusDisabled a reachable state rather than one the platform only
// honours (#525). Until this existed, an offboarded trader or a compromised
// credential could be locked out only by a human running UPDATE against the
// production database by hand.
//
// BOTH REFUSALS ARE THE POINT, and each removes a way this silently does nothing.
//
// An unknown status is refused BEFORE the statement runs — see Status.Validate
// for why the column's CHECK constraint is the backstop and not the check.
//
// ZERO ROWS AFFECTED IS ErrUserNotFound, NEVER SUCCESS. A disable that matched no
// subject is exactly the failure this method exists to remove: the operator is
// told the account is locked out, the audit record says it was, and it was not.
// A typo'd subject must not be indistinguishable from a completed disable.
//
// WHAT THIS DOES NOT DO, stated here because the handler above it says the same
// thing to its caller: a token already issued to this account stays valid until
// it expires (identity.DefaultTokenTTL, 8h). The gateway verifies signatures
// against JWKS and never reads this table, so this stops the NEXT login and not
// the session in flight. Closing that gap is #525 step 4 — a disabled_at column
// set by this same statement, plus a gateway check — and it is deliberately not
// done here.
func (p *Postgres) SetStatus(ctx context.Context, subject string, status Status, now time.Time) error {
	if err := status.Validate(); err != nil {
		return err
	}
	tag, err := p.pool.Exec(ctx, `
		UPDATE identity_users SET status = $2, updated_at = $3 WHERE subject = $1`,
		subject, string(status), now.UTC())
	if err != nil {
		return fmt.Errorf("identity: set status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrUserNotFound
	}
	return nil
}

// InvitesFor lists a tenant's invites for the OPERATOR-facing surface, which is
// authenticated and may see the three states apart. Token hashes are not
// returned: an operator has no use for one, and a surface that hands them out is
// a surface that can leak them.
func (p *Postgres) InvitesFor(ctx context.Context, tenant string) ([]*Invite, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT id, subject, tenant_id, roles, portfolios, created_by, created_at, expires_at, redeemed_at
		  FROM identity_invites WHERE tenant_id = $1 ORDER BY created_at DESC`, tenant)
	if err != nil {
		return nil, fmt.Errorf("identity: list invites: %w", err)
	}
	defer rows.Close()

	var out []*Invite
	for rows.Next() {
		var inv Invite
		if err := rows.Scan(&inv.ID, &inv.Subject, &inv.Tenant, &inv.Roles, &inv.Portfolios,
			&inv.CreatedBy, &inv.CreatedAt, &inv.ExpiresAt, &inv.RedeemedAt); err != nil {
			return nil, fmt.Errorf("identity: scan invite: %w", err)
		}
		out = append(out, &inv)
	}
	return out, rows.Err()
}
