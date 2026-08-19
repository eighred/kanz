package identity

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/eighred/kanz/internal/revocation"
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
// IT ALSO REVOKES THE TOKEN THE ACCOUNT ALREADY HOLDS (#532), which it did not
// used to. A disable stamps tokens_invalid_before in the SAME statement, and
// Revocations below publishes that mark to the gateway, which refuses every
// token for the subject minted before it. Two properties come from stamping it
// here rather than in a second write:
//
//   - The mark cannot lag the status. A disable that committed and then failed
//     to record its revocation instant would report success while leaving the
//     session in flight untouched — the precise gap this closes.
//   - GREATEST() rather than an assignment, so the mark only ever moves FORWARD.
//     A clock that steps backwards, or a replayed request, cannot narrow a
//     revocation that has already been published. GREATEST ignores NULLs, which
//     is what makes the FIRST disable — where the column is still NULL — land on
//     $3 without a COALESCE. The explicit ::timestamptz is because $3 appears in
//     two positions and the planner should not have to infer it from either.
//
// AN ENABLE DOES NOT CLEAR IT. Re-enabling an account must not resurrect the
// token the disable killed; the account's next login mints a newer one, which
// the gateway admits with no operator action. The condition below is what makes
// enable leave the mark alone.
func (p *Postgres) SetStatus(ctx context.Context, subject string, status Status, now time.Time) error {
	if err := status.Validate(); err != nil {
		return err
	}
	tag, err := p.pool.Exec(ctx, `
		UPDATE identity_users
		   SET status = $2,
		       updated_at = $3,
		       tokens_invalid_before = CASE WHEN $2 = 'disabled'
		           THEN GREATEST(tokens_invalid_before, $3::timestamptz)
		           ELSE tokens_invalid_before END
		 WHERE subject = $1`,
		subject, string(status), now.UTC())
	if err != nil {
		return fmt.Errorf("identity: set status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrUserNotFound
	}
	return nil
}

// Revocations is the feed the api-gateway enforces (#532): every account with a
// revocation mark, subject hashed.
//
// UNFILTERED BY status, AND THAT IS NOT AN OVERSIGHT. A re-enabled account keeps
// its mark, because the token issued before the disable must stay dead even
// though the account is active again. Filtering on status = 'disabled' would
// drop exactly the entries that are doing work.
//
// UNFILTERED BY AGE EITHER. Pruning marks older than the token lifetime is
// tempting and is only correct if you know the longest TTL any live token was
// minted with — which is unknowable after IDENTITY_TOKEN_TTL changes, and
// getting it wrong readmits a revoked token. The list is bounded by the number
// of accounts ever disabled, which is bounded by the number of accounts; that is
// a bound worth having over one that depends on config history.
func (p *Postgres) Revocations(ctx context.Context) ([]revocation.Entry, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT subject, tokens_invalid_before
		  FROM identity_users
		 WHERE tokens_invalid_before IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("identity: list revocations: %w", err)
	}
	defer rows.Close()

	// NON-NIL, so an empty result marshals as `[]` and not `null`. "Nobody is
	// revoked" and "this field is missing" must not look the same on a wire
	// contract whose whole job is to be believed.
	out := []revocation.Entry{}
	for rows.Next() {
		var subject string
		var notBefore time.Time
		if err := rows.Scan(&subject, &notBefore); err != nil {
			return nil, fmt.Errorf("identity: scan revocation: %w", err)
		}
		out = append(out, revocation.Entry{
			SubjectHash: revocation.HashSubject(subject),
			NotBefore:   notBefore.UTC().Unix(),
		})
	}
	if err := rows.Err(); err != nil {
		// A PARTIAL DENYLIST IS WORSE THAN NONE: it reads as complete and admits
		// whoever the failed read did not reach. The caller serves an error and
		// the gateway keeps its previous snapshot until this recovers.
		return nil, fmt.Errorf("identity: read revocations: %w", err)
	}
	return out, nil
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
