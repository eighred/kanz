package identity

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/eighred/kanz/pkg/auth"
	"github.com/jackc/pgx/v5"
)

// InvitationSummary is the only projection used by listings and audit. Neither
// token material nor its hash can be serialized from this type.
type InvitationSummary struct {
	ID         string     `json:"invite_id"`
	Subject    string     `json:"subject"`
	Tenant     string     `json:"tenant"`
	Roles      []string   `json:"roles"`
	Portfolios []string   `json:"portfolios"`
	CreatedBy  string     `json:"created_by"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	RedeemedAt *time.Time `json:"redeemed_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	RevokedBy  string     `json:"revoked_by,omitempty"`
	ReissuedAs *string    `json:"reissued_as,omitempty"`
	Revision   int64      `json:"revision"`
	State      string     `json:"state"`
	Redeemable bool       `json:"redeemable"`
}

func (i *Invite) Summary(now time.Time) InvitationSummary {
	state := "pending"
	switch {
	case i.RedeemedAt != nil:
		state = "accepted"
	case i.RevokedAt != nil:
		state = "revoked"
	case !now.Before(i.ExpiresAt):
		state = "expired"
	}
	return InvitationSummary{ID: i.ID, Subject: i.Subject, Tenant: i.Tenant, Roles: copyOf(i.Roles), Portfolios: copyOf(i.Portfolios), CreatedBy: i.CreatedBy, CreatedAt: i.CreatedAt, ExpiresAt: i.ExpiresAt, RedeemedAt: i.RedeemedAt, RevokedAt: i.RevokedAt, RevokedBy: i.RevokedBy, ReissuedAs: i.ReissuedAs, Revision: i.Revision, State: state, Redeemable: state == "pending"}
}

func lockInvitation(ctx context.Context, tx pgx.Tx, tenant, id string) (*Invite, error) {
	var i Invite
	err := tx.QueryRow(ctx, `SELECT id,subject,tenant_id,roles,portfolios,created_by,created_at,expires_at,redeemed_at,revoked_at,revoked_by,revision,reissued_as FROM identity_invites WHERE id=$1 AND tenant_id=$2 FOR UPDATE`, id, tenant).Scan(&i.ID, &i.Subject, &i.Tenant, &i.Roles, &i.Portfolios, &i.CreatedBy, &i.CreatedAt, &i.ExpiresAt, &i.RedeemedAt, &i.RevokedAt, &i.RevokedBy, &i.Revision, &i.ReissuedAs)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrInviteNotFound
	}
	return &i, err
}

// RevokeInvite shares the invitation row lock with redemption. A successful
// revoke therefore means no subsequent redemption of that token can commit.
func (p *Postgres) RevokeInvite(ctx context.Context, actor Administration, id string, revision int64, now time.Time) (*Invite, error) {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	u, err := lockAdministrator(ctx, tx, actor, now)
	if err != nil {
		return nil, err
	}
	i, err := lockInvitation(ctx, tx, actor.Tenant, id)
	if err != nil {
		return nil, err
	}
	if i.Revision != revision || i.RedeemedAt != nil || i.ReissuedAs != nil {
		return nil, ErrInviteConflict
	}
	if i.RevokedAt != nil {
		return i, tx.Commit(ctx)
	}
	before := i.Summary(now)
	stamp := now.UTC()
	i.RevokedAt = &stamp
	i.RevokedBy = actor.Subject
	i.Revision++
	if _, err = tx.Exec(ctx, `UPDATE identity_invites SET revoked_at=$2,revoked_by=$3,revision=revision+1 WHERE id=$1`, id, stamp, actor.Subject); err != nil {
		return nil, err
	}
	if err = recordInvitation(ctx, tx, actor, u, "revoke", before, i.Summary(now), nil, now); err != nil {
		return nil, err
	}
	return i, tx.Commit(ctx)
}

// ReissueInvite consumes one reviewed source revision and creates a new token
// lineage atomically. Claims are copied from the locked record, never the body.
// An already reissued or accepted invitation is terminal. Explicitly revoked
// and expired invitations can be reissued after a fresh administrator review.
func (p *Postgres) ReissueInvite(ctx context.Context, actor Administration, id string, revision int64, newID, tokenHash string, policy InviteDomainPolicy, now time.Time, ttl time.Duration) (*Invite, error) {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	u, err := lockAdministrator(ctx, tx, actor, now)
	if err != nil {
		return nil, err
	}
	i, err := lockInvitation(ctx, tx, actor.Tenant, id)
	if err != nil {
		return nil, err
	}
	if i.Revision != revision || i.RedeemedAt != nil || i.ReissuedAs != nil {
		return nil, ErrInviteConflict
	}
	if err = policy.Check(i.Subject); err != nil {
		return nil, ErrInvitePolicy
	}
	if err = ValidateAccess(i.Roles, i.Portfolios); err != nil {
		return nil, err
	}
	replacement, err := NewInvite(newID, tokenHash, i.Subject, i.Tenant, i.Roles, i.Portfolios, actor.Subject, now, ttl)
	if err != nil {
		return nil, err
	}
	before := i.Summary(now)
	stamp := now.UTC()
	if i.RevokedAt == nil {
		i.RevokedAt = &stamp
		i.RevokedBy = actor.Subject
	}
	i.Revision++
	i.ReissuedAs = &replacement.ID
	// The deferred self-reference is satisfied by insertInvite before commit.
	if _, err = tx.Exec(ctx, `UPDATE identity_invites SET revoked_at=$2,revoked_by=$3,revision=revision+1,reissued_as=$4 WHERE id=$1`, id, i.RevokedAt, i.RevokedBy, replacement.ID); err != nil {
		return nil, err
	}
	if err = insertInvite(ctx, tx, replacement); err != nil {
		return nil, err
	}
	if err = recordInvitation(ctx, tx, actor, u, "reissue", before, i.Summary(now), replacement, now); err != nil {
		return nil, err
	}
	return replacement, tx.Commit(ctx)
}

func recordInvitation(ctx context.Context, tx pgx.Tx, actor Administration, u *User, action string, before, after InvitationSummary, replacement *Invite, now time.Time) error {
	entry := auth.BuildDecisionLog("operator:"+actor.Subject, auth.Request{Principal: &auth.Principal{MFA: actor.MFA, Subject: actor.Subject, Tenant: actor.Tenant, Roles: u.Roles, Portfolios: u.Portfolios, SessionEpoch: actor.SessionEpoch, IssuedAt: actor.IssuedAt}, Action: auth.Action("identity.invitation." + action), Resource: auth.Resource{Type: "invitation", ID: after.ID, Tenant: after.Tenant}}, auth.Decision{Allow: true, Reason: "current administrator authority and invitation revision verified under lock"})
	oldJSON, err := json.Marshal(before)
	if err != nil {
		return err
	}
	newJSON, err := json.Marshal(after)
	if err != nil {
		return err
	}
	entry.Attributes["invitation.before"] = string(oldJSON)
	entry.Attributes["invitation.after"] = string(newJSON)
	entry.Attributes["occurred_at"] = now.UTC().Format(time.RFC3339Nano)
	if replacement != nil {
		payload, err := json.Marshal(replacement.Summary(now))
		if err != nil {
			return err
		}
		entry.Attributes["invitation.replacement"] = string(payload)
	}
	return persistIdentityDecision(ctx, tx, after.Tenant, after.Subject, now, entry)
}
