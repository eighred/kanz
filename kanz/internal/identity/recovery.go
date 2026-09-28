package identity

import (
	"context"
	"errors"
	"net/mail"
	"strings"
	"time"

	"github.com/eighred/kanz/pkg/auth"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var ErrRecovery = errors.New("identity: challenge unavailable")
var ErrRecoveryCooldown = errors.New("identity: wait before requesting another message")

const ChallengeTTL = 20 * time.Minute

type MailboxStatus struct {
	Address        string     `json:"verified_address"`
	VerifiedAt     *time.Time `json:"verified_at"`
	PendingAddress string     `json:"pending_address"`
	Delivery       string     `json:"verification_delivery"`
}

func (p *Postgres) MailboxStatus(ctx context.Context, actor Administration, now time.Time) (*MailboxStatus, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	u, err := lockRecoveryUser(ctx, tx, actor.Subject, actor.Tenant)
	if err != nil {
		return nil, err
	}
	if !actor.Current(u) {
		return nil, ErrCredentialMismatch
	}
	status := &MailboxStatus{Delivery: "none"}
	err = tx.QueryRow(ctx, `SELECT address,verified_at FROM identity_mailboxes WHERE subject=$1`, actor.Subject).Scan(&status.Address, &status.VerifiedAt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	var epoch int64
	var expiry time.Time
	err = tx.QueryRow(ctx, `SELECT address,state,session_epoch,expires_at FROM identity_mail_challenges WHERE subject=$1 AND purpose='verify'`, actor.Subject).Scan(&status.PendingAddress, &status.Delivery, &epoch, &expiry)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if err == nil && status.Delivery != "consumed" {
		if epoch != u.SessionEpoch {
			status.Delivery = "invalidated"
		} else if !now.Before(expiry) {
			status.Delivery = "expired"
		}
	}
	if status.Delivery == "consumed" {
		status.PendingAddress = ""
	}
	return status, tx.Commit(ctx)
}

func recoveryAddress(address string) bool {
	a, err := mail.ParseAddress(address)
	return err == nil && a.Name == "" && a.Address == address && len(address) <= 254 && strings.Contains(address, "@") && !strings.ContainsAny(address, "\r\n\x00")
}

// lockRecoveryUser observes the same tenant -> account lock order as access,
// status and rotation. No challenge mutation can resurrect a stale epoch.
func lockRecoveryUser(ctx context.Context, tx pgx.Tx, subject, tenant string) (*User, error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('identity-administration:' || $1,0))`, tenant); err != nil {
		return nil, err
	}
	var u User
	err := tx.QueryRow(ctx, `SELECT subject,tenant_id,roles,portfolios,status,credential_hash,session_epoch,tokens_invalid_before FROM identity_users WHERE subject=$1 AND tenant_id=$2 FOR UPDATE`, subject, tenant).Scan(&u.Subject, &u.Tenant, &u.Roles, &u.Portfolios, &u.Status, &u.Credential, &u.SessionEpoch, &u.TokensInvalidBefore)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrRecovery
	}
	return &u, err
}

// EnrollMailbox requires a previously verified credential snapshot AND a current
// session. Knowing a password alone cannot silently replace a recovery address.
// Existing verified recovery remains available until the new proof is consumed.
func (p *Postgres) EnrollMailbox(ctx context.Context, actor Administration, previous Hash, address string, now time.Time) error {
	if !recoveryAddress(address) {
		return ErrRecovery
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	u, err := lockRecoveryUser(ctx, tx, actor.Subject, actor.Tenant)
	if err != nil {
		return err
	}
	if !actor.Current(u) || u.Credential != previous {
		return ErrCredentialMismatch
	}
	if err = queueChallenge(ctx, tx, u, "verify", address, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RequestRecovery accepts only a subject. The destination is exclusively the
// durable, previously verified mailbox. Unknown, disabled and unenrolled
// subjects return the same result without contacting SMTP in the request path.
func (p *Postgres) RequestRecovery(ctx context.Context, subject string, now time.Time) error {
	// One database round trip for every subject; SMTP never runs on the public
	// request path. Concurrent mailbox/epoch changes are rechecked at consumption.
	_, err := p.pool.Exec(ctx, `WITH queued AS (
 INSERT INTO identity_mail_challenges(subject,purpose,request_id,address,session_epoch,requested_at,expires_at,state)
 SELECT u.subject,'recover',$2,m.address,u.session_epoch,$3,$4,'pending'
 FROM identity_users u JOIN identity_mailboxes m ON m.subject=u.subject
 WHERE u.subject=$1 AND u.status='active'
 ON CONFLICT(subject,purpose) DO UPDATE SET request_id=EXCLUDED.request_id,address=EXCLUDED.address,session_epoch=EXCLUDED.session_epoch,requested_at=EXCLUDED.requested_at,expires_at=EXCLUDED.expires_at,state='pending',attempts=0,token_hash=NULL,lease_until=NULL
 WHERE identity_mail_challenges.requested_at <= $3::timestamptz - interval '1 minute'
 RETURNING request_id)
 SELECT pg_notify('identity_mail','') FROM queued`, subject, uuid.NewString(), now.UTC(), now.Add(ChallengeTTL).UTC())
	return err
}

func queueChallenge(ctx context.Context, tx pgx.Tx, u *User, purpose, address string, now time.Time) error {
	// Cooldown is enforced by the database across replicas; repeated requests do
	// not invalidate an in-flight message or amplify outbound mail indefinitely.
	tag, err := tx.Exec(ctx, `INSERT INTO identity_mail_challenges(subject,purpose,request_id,address,session_epoch,requested_at,expires_at,state) VALUES($1,$2,$3,$4,$5,$6,$7,'pending') ON CONFLICT(subject,purpose) DO UPDATE SET request_id=EXCLUDED.request_id,address=EXCLUDED.address,session_epoch=EXCLUDED.session_epoch,requested_at=EXCLUDED.requested_at,expires_at=EXCLUDED.expires_at,state='pending',attempts=0,token_hash=NULL,lease_until=NULL WHERE identity_mail_challenges.requested_at <= $6::timestamptz - interval '1 minute'`, u.Subject, purpose, uuid.NewString(), address, u.SessionEpoch, now.UTC(), now.Add(ChallengeTTL).UTC())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrRecoveryCooldown
	}
	_, err = tx.Exec(ctx, `SELECT pg_notify('identity_mail','')`)
	return err
}

// MailAttempt holds an ephemeral token only for the duration of SMTP delivery.
// Do not serialize it, log it, or expose it through an administrative response.
type MailAttempt struct {
	ID, Address, Purpose string
	Token                string `json:"-"`
	Attempt              int
	ExpiresAt            time.Time
}

func (p *Postgres) ClaimMail(ctx context.Context, now time.Time) (*MailAttempt, error) {
	// Resolve abandoned final attempts in bounded batches. Without this, a
	// process crash on attempt three would leave "sending" forever.
	_, err := p.pool.Exec(ctx, `WITH terminal AS (SELECT subject,purpose FROM identity_mail_challenges WHERE state IN ('pending','sending') AND (expires_at<=$1 OR (attempts=3 AND lease_until<=$1)) LIMIT 100 FOR UPDATE SKIP LOCKED) UPDATE identity_mail_challenges c SET state='failed',token_hash=NULL FROM terminal t WHERE c.subject=t.subject AND c.purpose=t.purpose`, now.UTC())
	if err != nil {
		return nil, err
	}
	raw, hash, err := NewInviteToken()
	if err != nil {
		return nil, err
	}
	var m MailAttempt
	err = p.pool.QueryRow(ctx, `WITH candidate AS (SELECT c.subject,c.purpose FROM identity_mail_challenges c JOIN identity_users u ON u.subject=c.subject WHERE c.state IN ('pending','sending') AND (c.lease_until IS NULL OR c.lease_until <= $1) AND c.attempts<3 AND c.expires_at>$1 AND u.status='active' AND u.session_epoch=c.session_epoch ORDER BY c.requested_at LIMIT 1 FOR UPDATE OF c SKIP LOCKED) UPDATE identity_mail_challenges c SET state='sending',attempts=c.attempts+1,lease_until=$1::timestamptz+interval '30 seconds',token_hash=$2 FROM candidate x WHERE c.subject=x.subject AND c.purpose=x.purpose RETURNING c.request_id,c.address,c.purpose,c.attempts,c.expires_at`, now.UTC(), hash).Scan(&m.ID, &m.Address, &m.Purpose, &m.Attempt, &m.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	m.Token = raw
	return &m, nil
}

// FinishMail is fenced by both request and attempt. An old worker cannot mark a
// replacement delivered, and a lost SMTP acknowledgement never restores a token.
func (p *Postgres) FinishMail(ctx context.Context, m *MailAttempt, accepted bool, now time.Time) error {
	state := "pending"
	if accepted {
		state = "sent"
	} else if m.Attempt >= 3 {
		state = "failed"
	}
	_, err := p.pool.Exec(ctx, `UPDATE identity_mail_challenges SET state=$3,token_hash=CASE WHEN $4 THEN token_hash ELSE NULL END,lease_until=$5::timestamptz+interval '1 minute' WHERE request_id=$1 AND attempts=$2 AND state='sending'`, m.ID, m.Attempt, state, accepted, now.UTC())
	return err
}

// ConsumeChallenge serializes proof consumption with all account mutations.
// Verification cannot reset a password, recovery cannot verify a new address,
// and every successful recovery invalidates all challenges from its old epoch.
func (p *Postgres) ConsumeChallenge(ctx context.Context, raw, purpose string, replacement Hash, now time.Time) error {
	if purpose != "verify" && purpose != "recover" {
		return ErrRecovery
	}
	if purpose == "recover" && replacement == "" {
		return ErrRecovery
	}
	var subject, tenant string
	err := p.pool.QueryRow(ctx, `SELECT c.subject,u.tenant_id FROM identity_mail_challenges c JOIN identity_users u ON u.subject=c.subject WHERE c.token_hash=$1 AND c.purpose=$2`, InviteTokenHash(raw), purpose).Scan(&subject, &tenant)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrRecovery
	}
	if err != nil {
		return err
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	u, err := lockRecoveryUser(ctx, tx, subject, tenant)
	if err != nil {
		return err
	}
	if !u.Active() {
		return ErrRecovery
	}
	var address string
	err = tx.QueryRow(ctx, `UPDATE identity_mail_challenges SET state='consumed',token_hash=NULL WHERE subject=$1 AND purpose=$2 AND token_hash=$3 AND session_epoch=$4 AND expires_at>$5 AND state IN ('sending','sent') RETURNING address`, subject, purpose, InviteTokenHash(raw), u.SessionEpoch, now.UTC()).Scan(&address)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrRecovery
	}
	if err != nil {
		return err
	}
	if purpose == "verify" {
		_, err = tx.Exec(ctx, `INSERT INTO identity_mailboxes(subject,address,verified_at) VALUES($1,$2,$3) ON CONFLICT(subject) DO UPDATE SET address=EXCLUDED.address,verified_at=EXCLUDED.verified_at`, subject, address, now.UTC())
		if err != nil {
			return err
		}
		// Changing the recovery authority retires an already-sent reset link.
		if _, err = tx.Exec(ctx, `UPDATE identity_mail_challenges SET state='consumed',token_hash=NULL WHERE subject=$1 AND purpose='recover'`, subject); err != nil {
			return err
		}
	} else {
		var current string
		if err = tx.QueryRow(ctx, `SELECT address FROM identity_mailboxes WHERE subject=$1`, subject).Scan(&current); err != nil {
			return err
		}
		if current != address {
			return ErrRecovery
		}
		if _, err = tx.Exec(ctx, `UPDATE identity_users SET credential_hash=$2,session_epoch=session_epoch+1,access_revision=access_revision+1,updated_at=$3,tokens_invalid_before=GREATEST(tokens_invalid_before,$3::timestamptz) WHERE subject=$1`, subject, string(replacement), now.UTC()); err != nil {
			return err
		}
	}
	entry := auth.BuildDecisionLog("mailbox-proof:"+subject, auth.Request{Principal: &auth.Principal{Subject: subject, Tenant: tenant, Roles: u.Roles, Portfolios: u.Portfolios, SessionEpoch: u.SessionEpoch}, Action: auth.Action("identity.account.mailbox." + purpose), Resource: auth.Resource{Type: "account", ID: subject, Tenant: tenant}}, auth.Decision{Allow: true, Reason: "single-use mailbox proof and current account epoch checked under tenant lock"})
	entry.Attributes["occurred_at"] = now.UTC().Format(time.RFC3339Nano)
	if err = persistIdentityDecision(ctx, tx, tenant, subject, now, entry); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
