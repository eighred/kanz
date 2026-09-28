package identity

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/eighred/kanz/pkg/auth"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/jackc/pgx/v5"
)

var ErrMFA = errors.New("identity: MFA ceremony refused")
var ErrMFAStepUp = errors.New("identity: recent MFA required")
var ErrMFALastFactor = errors.New("identity: retain at least one verified factor")

const MFACeremonyTTL = 3 * time.Minute
const MaxMFAFactors = 5

type MFACeremony struct {
	ID      string `json:"id"`
	Options any    `json:"options"`
}
type MFAFactor struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
}
type MFAStatus struct {
	Enabled       bool        `json:"enabled"`
	Recent        bool        `json:"recent"`
	VerifiedUntil *time.Time  `json:"verified_until,omitempty"`
	Factors       []MFAFactor `json:"factors"`
}

type webAuthnUser struct {
	u           *User
	credentials []webauthn.Credential
}

func (u webAuthnUser) WebAuthnID() []byte {
	sum := sha256.Sum256([]byte(u.u.Tenant + "\x00" + u.u.Subject))
	return sum[:]
}
func (u webAuthnUser) WebAuthnName() string                       { return u.u.Subject }
func (u webAuthnUser) WebAuthnDisplayName() string                { return u.u.Subject }
func (u webAuthnUser) WebAuthnCredentials() []webauthn.Credential { return u.credentials }

func mfaCredentials(ctx context.Context, tx pgx.Tx, u *User) (webAuthnUser, error) {
	out := webAuthnUser{u: u}
	rows, err := tx.Query(ctx, `SELECT credential FROM identity_mfa_credentials WHERE subject=$1 ORDER BY credential_id LIMIT 6`, u.Subject)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			return out, err
		}
		var c webauthn.Credential
		if err = json.Unmarshal(raw, &c); err != nil {
			return out, err
		}
		out.credentials = append(out.credentials, c)
	}
	if len(out.credentials) > MaxMFAFactors {
		return out, ErrMFA
	}
	return out, rows.Err()
}

func (p *Postgres) MFAStatus(ctx context.Context, actor Administration, now time.Time) (*MFAStatus, error) {
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
		return nil, ErrMFA
	}
	out := &MFAStatus{Enabled: u.MFA.Required, Recent: actor.MFA.Recent(now), Factors: []MFAFactor{}}
	if out.Recent {
		until := actor.MFA.VerifiedAt.Add(auth.StepUpTTL)
		out.VerifiedUntil = &until
	}
	rows, err := tx.Query(ctx, `SELECT credential_id,name,created_at,last_used_at FROM identity_mfa_credentials WHERE subject=$1 ORDER BY created_at LIMIT 6`, u.Subject)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var f MFAFactor
		if err = rows.Scan(&f.ID, &f.Name, &f.CreatedAt, &f.LastUsedAt); err != nil {
			rows.Close()
			return nil, err
		}
		out.Factors = append(out.Factors, f)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return out, tx.Commit(ctx)
}

// BeginMFA verifies the password snapshot and current session under the same
// account lock used by recovery and access changes. A challenge grants nothing.
func (p *Postgres) BeginMFA(ctx context.Context, actor Administration, previous Hash, purpose, name string, now time.Time, wa *webauthn.WebAuthn) (*MFACeremony, error) {
	if wa == nil || (purpose != "register" && purpose != "login" && purpose != "stepup") {
		return nil, ErrMFA
	}
	name = strings.TrimSpace(name)
	if purpose == "register" && (len(name) < 1 || len(name) > 80 || strings.ContainsAny(name, "\r\n\x00")) {
		return nil, ErrMFA
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	u, err := lockRecoveryUser(ctx, tx, actor.Subject, actor.Tenant)
	if err != nil {
		return nil, err
	}
	if !actor.Current(u) || ((purpose == "login" || purpose == "register") && (previous == "" || previous != u.Credential)) {
		return nil, ErrMFA
	}
	if purpose == "register" && u.MFA.Required && !actor.MFA.Recent(now) {
		return nil, ErrMFAStepUp
	}
	if purpose != "register" && !u.MFA.Required {
		return nil, ErrMFA
	}
	wu, err := mfaCredentials(ctx, tx, u)
	if err != nil {
		return nil, err
	}
	if (u.MFA.Required && len(wu.credentials) == 0) || (purpose == "register" && len(wu.credentials) >= MaxMFAFactors) {
		return nil, ErrMFA
	}
	var options any
	var session *webauthn.SessionData
	if purpose == "register" {
		exclusions := make([]protocol.CredentialDescriptor, 0, len(wu.credentials))
		for _, c := range wu.credentials {
			exclusions = append(exclusions, c.Descriptor())
		}
		options, session, err = wa.BeginRegistration(wu, webauthn.WithExclusions(exclusions), webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{UserVerification: protocol.VerificationRequired}))
	} else {
		options, session, err = wa.BeginLogin(wu, webauthn.WithUserVerification(protocol.VerificationRequired))
	}
	if err != nil {
		return nil, ErrMFA
	}
	session.Expires = now.Add(MFACeremonyTTL)
	raw, err := json.Marshal(session)
	if err != nil {
		return nil, err
	}
	id, hash, err := NewInviteToken()
	if err != nil {
		return nil, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO identity_mfa_ceremonies(subject,purpose,token_hash,session_epoch,session_data,name,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(subject,purpose) DO UPDATE SET token_hash=EXCLUDED.token_hash,session_epoch=EXCLUDED.session_epoch,session_data=EXCLUDED.session_data,name=EXCLUDED.name,expires_at=EXCLUDED.expires_at,consumed=FALSE`, u.Subject, purpose, hash, u.SessionEpoch, raw, name, session.Expires)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &MFACeremony{ID: id, Options: options}, nil
}

// FinishMFA serializes signature/counter verification and proof consumption.
// An audit failure rolls back every state change, including challenge burning.
func (p *Postgres) FinishMFA(ctx context.Context, actor *Administration, id, purpose string, response []byte, now time.Time, wa *webauthn.WebAuthn) (*User, error) {
	if wa == nil || len(id) != 43 || len(response) == 0 || len(response) > 64<<10 {
		return nil, ErrMFA
	}
	if purpose != "login" && purpose != "register" && purpose != "stepup" {
		return nil, ErrMFA
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var subject, tenant string
	hash := InviteTokenHash(id)
	err = tx.QueryRow(ctx, `SELECT c.subject,u.tenant_id FROM identity_mfa_ceremonies c JOIN identity_users u ON u.subject=c.subject WHERE token_hash=$1 AND purpose=$2`, hash, purpose).Scan(&subject, &tenant)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrMFA
	}
	if err != nil {
		return nil, err
	}
	u, err := lockRecoveryUser(ctx, tx, subject, tenant)
	if err != nil {
		return nil, err
	}
	if !u.Active() || (purpose != "login" && (actor == nil || !actor.Current(u))) {
		return nil, ErrMFA
	}
	if purpose == "register" && u.MFA.Required && !actor.MFA.Recent(now) {
		return nil, ErrMFAStepUp
	}
	var epoch int64
	var data []byte
	var name string
	var expires time.Time
	var consumed bool
	err = tx.QueryRow(ctx, `SELECT session_epoch,session_data,name,expires_at,consumed FROM identity_mfa_ceremonies WHERE token_hash=$1 AND purpose=$2`, hash, purpose).Scan(&epoch, &data, &name, &expires, &consumed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrMFA
	}
	if err != nil {
		return nil, err
	}
	if consumed || epoch != u.SessionEpoch || !now.Before(expires) {
		return nil, ErrMFA
	}
	var session webauthn.SessionData
	if err = json.Unmarshal(data, &session); err != nil {
		return nil, err
	}
	wu, err := mfaCredentials(ctx, tx, u)
	if err != nil {
		return nil, err
	}
	var credential *webauthn.Credential
	if purpose == "register" {
		if len(wu.credentials) >= MaxMFAFactors {
			return nil, ErrMFA
		}
		parsed, e := protocol.ParseCredentialCreationResponseBytes(response)
		if e != nil {
			return nil, ErrMFA
		}
		credential, err = wa.CreateCredential(wu, session, parsed)
	} else {
		if !u.MFA.Required {
			return nil, ErrMFA
		}
		parsed, e := protocol.ParseCredentialRequestResponseBytes(response)
		if e != nil {
			return nil, ErrMFA
		}
		credential, err = wa.ValidateLogin(wu, session, parsed)
	}
	if err != nil || credential == nil || !credential.Flags.UserVerified || credential.Authenticator.CloneWarning {
		return nil, ErrMFA
	}
	raw, err := json.Marshal(credential)
	if err != nil {
		return nil, err
	}
	key := base64.RawURLEncoding.EncodeToString(credential.ID)
	if purpose == "register" {
		_, err = tx.Exec(ctx, `INSERT INTO identity_mfa_credentials(credential_id,subject,name,credential,created_at) VALUES($1,$2,$3,$4,$5)`, key, u.Subject, name, raw, now.UTC())
		if err != nil {
			return nil, err
		}
		if err = mfaFence(ctx, tx, u, now); err != nil {
			return nil, err
		}
	} else {
		_, err = tx.Exec(ctx, `UPDATE identity_mfa_credentials SET credential=$1,last_used_at=$2 WHERE credential_id=$3 AND subject=$4`, raw, now.UTC(), key, u.Subject)
		if err != nil {
			return nil, err
		}
	}
	tag, err := tx.Exec(ctx, `UPDATE identity_mfa_ceremonies SET consumed=TRUE WHERE token_hash=$1 AND consumed=FALSE`, hash)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() != 1 {
		return nil, ErrMFA
	}
	u.MFA = auth.MFA{Required: true, VerifiedAt: now.UTC()}
	if err = recordMFA(ctx, tx, u, purpose, key, now); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return u, nil
}

func mfaFence(ctx context.Context, tx pgx.Tx, u *User, now time.Time) error {
	err := tx.QueryRow(ctx, `UPDATE identity_users SET mfa_enabled=TRUE,session_epoch=session_epoch+1,access_revision=access_revision+1,updated_at=$1,tokens_invalid_before=GREATEST(COALESCE(tokens_invalid_before,$1),$1) WHERE subject=$2 RETURNING session_epoch`, now.UTC(), u.Subject).Scan(&u.SessionEpoch)
	if err == nil {
		u.MFA.Required = true
		u.TokensInvalidBefore = &now
	}
	return err
}

func recordMFA(ctx context.Context, tx pgx.Tx, u *User, action, factorID string, now time.Time) error {
	entry := auth.BuildDecisionLog("operator:"+u.Subject, auth.Request{Principal: &auth.Principal{MFA: u.MFA, Subject: u.Subject, Tenant: u.Tenant, Roles: u.Roles, Portfolios: u.Portfolios, SessionEpoch: u.SessionEpoch}, Action: auth.Action("identity.account.mfa." + action), Resource: auth.Resource{Type: "account", ID: u.Subject, Tenant: u.Tenant}}, auth.Decision{Allow: true, Reason: "verified WebAuthn user verification and account epoch under account lock"})
	fingerprint := sha256.Sum256([]byte(factorID))
	entry.Attributes["mfa.factor_sha256"] = hex.EncodeToString(fingerprint[:])
	entry.Attributes["session_epoch"] = strconv.FormatInt(u.SessionEpoch, 10)
	return persistIdentityDecision(ctx, tx, u.Tenant, u.Subject, now, entry)
}

func (p *Postgres) RemoveMFA(ctx context.Context, actor Administration, id string, now time.Time) (*User, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	u, err := lockRecoveryUser(ctx, tx, actor.Subject, actor.Tenant)
	if err != nil {
		return nil, err
	}
	if !actor.Current(u) || !u.MFA.Required {
		return nil, ErrMFA
	}
	if !actor.MFA.Recent(now) {
		return nil, ErrMFAStepUp
	}
	var count int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM identity_mfa_credentials WHERE subject=$1`, u.Subject).Scan(&count); err != nil {
		return nil, err
	}
	if count <= 1 {
		return nil, ErrMFALastFactor
	}
	tag, err := tx.Exec(ctx, `DELETE FROM identity_mfa_credentials WHERE subject=$1 AND credential_id=$2`, u.Subject, id)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() != 1 {
		return nil, ErrMFA
	}
	if err = mfaFence(ctx, tx, u, now); err != nil {
		return nil, err
	}
	// Removing a factor does not renew the prior ceremony's freshness.
	u.MFA = actor.MFA
	if err = recordMFA(ctx, tx, u, "remove", id, now); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return u, nil
}
