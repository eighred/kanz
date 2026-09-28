package session

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"time"

	"github.com/eighred/kanz/pkg/auth"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/encoding/protojson"
)

type postgres struct {
	pool   *pgxpool.Pool
	aead   cipher.AEAD
	limits Limits
}

// NewPostgres verifies role, schema, encryption-key agreement and quota policy.
// Pool ownership stays with the composition root; no credentials enter errors.
func NewPostgres(ctx context.Context, pool *pgxpool.Pool, key []byte, ttl time.Duration, l Limits) (*Manager, error) {
	if pool == nil || len(key) != 32 || l.Validate() != nil || ttl <= 0 || ttl > 24*time.Hour {
		return nil, ErrInvalid
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, ErrInvalid
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, ErrInvalid
	}
	var bypass bool
	if err = pool.QueryRow(ctx, `SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&bypass); err != nil || bypass {
		return nil, ErrUnavailable
	}
	var secured bool
	var tableCount int
	if err = pool.QueryRow(ctx, `SELECT count(*),COALESCE(bool_and(relrowsecurity AND relforcerowsecurity),false) FROM pg_class WHERE oid IN (to_regclass('bff_sessions'),to_regclass('bff_session_audit'))`).Scan(&tableCount, &secured); err != nil || tableCount != 2 || !secured {
		return nil, ErrUnavailable
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('bff-session-capacity',0))`); err != nil {
		return nil, ErrUnavailable
	}
	fingerprint := digest(string(key))
	if _, err = tx.Exec(ctx, `INSERT INTO bff_session_policy VALUES(TRUE,$1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, l.Sessions, l.Pending, l.PerSubject, fingerprint, int64(ttl)); err != nil {
		return nil, ErrUnavailable
	}
	var actual Limits
	var fp string
	var actualTTL int64
	if err = tx.QueryRow(ctx, `SELECT max_sessions,max_pending,per_subject,key_fingerprint,ttl_ns FROM bff_session_policy WHERE singleton`).Scan(&actual.Sessions, &actual.Pending, &actual.PerSubject, &fp, &actualTTL); err != nil || actual != l || fp != fingerprint || actualTTL != int64(ttl) {
		return nil, ErrInvalid
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, ErrUnavailable
	}
	return &Manager{store: &postgres{pool, aead, l}, ttl: ttl, now: time.Now}, nil
}
func (p *postgres) seal(v any, aad string) ([]byte, error) {
	b, e := json.Marshal(v)
	if e != nil || len(b) > MaxPayload {
		return nil, ErrInvalid
	}
	nonce := make([]byte, p.aead.NonceSize())
	if _, e = rand.Read(nonce); e != nil {
		return nil, ErrUnavailable
	}
	return p.aead.Seal(nonce, nonce, b, []byte(aad)), nil
}
func (p *postgres) open(b []byte, aad string, v any) error {
	n := p.aead.NonceSize()
	if len(b) < n || len(b) > MaxPayload+128 {
		return ErrUnavailable
	}
	raw, e := p.aead.Open(nil, b[:n], b[n:], []byte(aad))
	if e != nil || json.Unmarshal(raw, v) != nil {
		return ErrUnavailable
	}
	return nil
}
func aad(hash string, s Session) string {
	b, _ := json.Marshal([]string{"bff-session-v1", hash, s.Tenant, s.Subject, s.Authority})
	return string(b)
}
func (p *postgres) begin(ctx context.Context, write bool) (pgx.Tx, error) {
	tx, e := p.pool.Begin(ctx)
	if e != nil {
		return nil, ErrUnavailable
	}
	if write {
		_, e = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('bff-session-capacity',0))`)
	}
	if e == nil {
		_, e = tx.Exec(ctx, `SELECT set_config('app.tenant_id','__bff_lookup__',true),set_config('app.bff_hash','',true),set_config('app.bff_subject','',true),set_config('app.bff_authority','',true)`)
	}
	if e == nil && write {
		_, e = tx.Exec(ctx, `DELETE FROM bff_session_slots WHERE expires_at<=clock_timestamp()`)
	}
	if e != nil {
		_ = tx.Rollback(ctx)
		return nil, ErrUnavailable
	}
	return tx, nil
}
func scope(ctx context.Context, tx pgx.Tx, s Session) error {
	_, e := tx.Exec(ctx, `SELECT set_config('app.tenant_id',$1,true),set_config('app.bff_subject',$2,true),set_config('app.bff_authority',$3,true),set_config('app.bff_hash','',true)`, s.Tenant, s.Subject, s.Authority)
	return e
}
func (p *postgres) load(ctx context.Context, tx pgx.Tx, hash string) (Session, bool, error) {
	if _, e := tx.Exec(ctx, `SELECT set_config('app.bff_hash',$1,true)`, hash); e != nil {
		return Session{}, false, ErrUnavailable
	}
	var meta Session
	var encrypted []byte
	e := tx.QueryRow(ctx, `SELECT s.tenant_id,s.subject,s.authority,s.payload,d.expires_at FROM bff_sessions s JOIN bff_session_slots d USING(hash) WHERE s.hash=$1 AND d.kind='session' AND d.expires_at>clock_timestamp()`, hash).Scan(&meta.Tenant, &meta.Subject, &meta.Authority, &encrypted, &meta.Expiry)
	if errors.Is(e, pgx.ErrNoRows) {
		return Session{}, false, nil
	}
	if e != nil {
		return Session{}, false, ErrUnavailable
	}
	var s Session
	if p.open(encrypted, aad(hash, meta), &s) != nil || !sameOwner(meta, s) || !meta.Expiry.Equal(s.Expiry) {
		return Session{}, false, ErrUnavailable
	}
	s.Expiry = meta.Expiry
	return s, true, nil
}
func audit(ctx context.Context, tx pgx.Tx, s Session, action, target string) error {
	if e := scope(ctx, tx, s); e != nil {
		return e
	}
	entry := auth.BuildDecisionLog("web-bff", auth.Request{Principal: &auth.Principal{Subject: s.Subject, Tenant: s.Tenant}, Action: auth.Action("identity.session." + action), Resource: auth.Resource{Type: "browser-session", ID: target, Tenant: s.Tenant}}, auth.Decision{Allow: true, Reason: "session authority transaction"})
	entry.Attributes["session.authority"] = s.Authority
	raw, e := protojson.Marshal(entry)
	if e != nil {
		return e
	}
	_, e = tx.Exec(ctx, `INSERT INTO bff_session_audit(tenant_id,subject,decision) VALUES($1,$2,$3::jsonb)`, s.Tenant, s.Subject, string(raw))
	return e
}
func (p *postgres) put(ctx context.Context, id string, s Session, old string, strict bool) error {
	tx, e := p.begin(ctx, true)
	if e != nil {
		return e
	}
	defer func() { _ = tx.Rollback(ctx) }()
	previous, exists, e := p.load(ctx, tx, digest(old))
	if e != nil {
		return e
	}
	if strict && (!exists || !sameOwner(previous, s)) {
		return ErrMissing
	}
	if exists {
		if _, e = tx.Exec(ctx, `DELETE FROM bff_session_slots WHERE hash=$1 AND kind='session'`, digest(old)); e != nil {
			return ErrUnavailable
		}
		if e = audit(ctx, tx, previous, "replace", digest(old)); e != nil {
			return ErrUnavailable
		}
	}
	if e = scope(ctx, tx, s); e != nil {
		return ErrUnavailable
	}
	var total, count int
	if e = tx.QueryRow(ctx, `SELECT count(*) FROM bff_session_slots WHERE kind='session'`).Scan(&total); e != nil {
		return ErrUnavailable
	}
	if e = tx.QueryRow(ctx, `SELECT count(*) FROM bff_sessions WHERE tenant_id=$1 AND subject=$2 AND authority=$3`, s.Tenant, s.Subject, s.Authority).Scan(&count); e != nil {
		return ErrUnavailable
	}
	if total >= p.limits.Sessions || count >= p.limits.PerSubject {
		return ErrCapacity
	}
	hash := digest(id)
	payload, e := p.seal(s, aad(hash, s))
	if e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, `INSERT INTO bff_session_slots(hash,kind,expires_at) VALUES($1,'session',$2)`, hash, s.Expiry); e != nil {
		return ErrUnavailable
	}
	if _, e = tx.Exec(ctx, `INSERT INTO bff_sessions(hash,tenant_id,subject,authority,created_at,payload) VALUES($1,$2,$3,$4,clock_timestamp(),$5)`, hash, s.Tenant, s.Subject, s.Authority, payload); e != nil {
		return ErrUnavailable
	}
	if e = audit(ctx, tx, s, "create", hash); e != nil {
		return ErrUnavailable
	}
	if tx.Commit(ctx) != nil {
		return ErrUnavailable
	}
	return nil
}
func (p *postgres) get(ctx context.Context, id string) (Session, bool, error) {
	tx, e := p.begin(ctx, false)
	if e != nil {
		return Session{}, false, e
	}
	defer func() { _ = tx.Rollback(ctx) }()
	s, ok, e := p.load(ctx, tx, digest(id))
	if e != nil {
		return Session{}, false, e
	}
	if tx.Commit(ctx) != nil {
		return Session{}, false, ErrUnavailable
	}
	return s, ok, nil
}
func (p *postgres) del(ctx context.Context, id string) error {
	tx, e := p.begin(ctx, true)
	if e != nil {
		return e
	}
	defer func() { _ = tx.Rollback(ctx) }()
	s, ok, e := p.load(ctx, tx, digest(id))
	if e != nil {
		return e
	}
	if ok {
		if _, e = tx.Exec(ctx, `DELETE FROM bff_session_slots WHERE hash=$1 AND kind='session'`, digest(id)); e != nil {
			return ErrUnavailable
		}
		if audit(ctx, tx, s, "logout", digest(id)) != nil {
			return ErrUnavailable
		}
	}
	if tx.Commit(ctx) != nil {
		return ErrUnavailable
	}
	return nil
}
func (p *postgres) list(ctx context.Context, id string) ([]Summary, error) {
	tx, e := p.begin(ctx, false)
	if e != nil {
		return nil, e
	}
	defer func() { _ = tx.Rollback(ctx) }()
	s, ok, e := p.load(ctx, tx, digest(id))
	if e != nil {
		return nil, e
	}
	if !ok {
		return nil, ErrMissing
	}
	if scope(ctx, tx, s) != nil {
		return nil, ErrUnavailable
	}
	rows, e := tx.Query(ctx, `SELECT s.hash,s.created_at,d.expires_at FROM bff_sessions s JOIN bff_session_slots d USING(hash) WHERE s.tenant_id=$1 AND s.subject=$2 AND s.authority=$3 AND d.expires_at>clock_timestamp() ORDER BY s.hash LIMIT $4`, s.Tenant, s.Subject, s.Authority, p.limits.PerSubject)
	if e != nil {
		return nil, ErrUnavailable
	}
	out := []Summary{}
	for rows.Next() {
		var v Summary
		if rows.Scan(&v.ID, &v.CreatedAt, &v.ExpiresAt) != nil {
			rows.Close()
			return nil, ErrUnavailable
		}
		v.Current = v.ID == digest(id)
		out = append(out, v)
	}
	rows.Close()
	if rows.Err() != nil || tx.Commit(ctx) != nil {
		return nil, ErrUnavailable
	}
	return out, nil
}
func (p *postgres) revoke(ctx context.Context, id, target string) error {
	tx, e := p.begin(ctx, true)
	if e != nil {
		return e
	}
	defer func() { _ = tx.Rollback(ctx) }()
	s, ok, e := p.load(ctx, tx, digest(id))
	if e != nil {
		return e
	}
	if !ok {
		return ErrMissing
	}
	if scope(ctx, tx, s) != nil {
		return ErrUnavailable
	}
	var own bool
	if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM bff_sessions WHERE hash=$1 AND tenant_id=$2 AND subject=$3 AND authority=$4)`, target, s.Tenant, s.Subject, s.Authority).Scan(&own); e != nil {
		return ErrUnavailable
	}
	if !own {
		return ErrNotFound
	}
	if _, e = tx.Exec(ctx, `DELETE FROM bff_session_slots WHERE hash=$1 AND kind='session'`, target); e != nil {
		return ErrUnavailable
	}
	if audit(ctx, tx, s, "revoke", target) != nil || tx.Commit(ctx) != nil {
		return ErrUnavailable
	}
	return nil
}
func (p *postgres) pendingPut(ctx context.Context, state, value string) error {
	tx, e := p.begin(ctx, true)
	if e != nil {
		return e
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var count int
	if tx.QueryRow(ctx, `SELECT count(*) FROM bff_session_slots WHERE kind='pending'`).Scan(&count) != nil {
		return ErrUnavailable
	}
	if count >= p.limits.Pending {
		return ErrCapacity
	}
	key := digest(state)
	payload, e := p.seal(value, "bff-pending-v1:"+key)
	if e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, `INSERT INTO bff_session_slots(hash,kind,expires_at,pending_payload) VALUES($1,'pending',clock_timestamp()+interval '10 minutes',$2)`, key, payload); e != nil {
		return ErrUnavailable
	}
	if tx.Commit(ctx) != nil {
		return ErrUnavailable
	}
	return nil
}
func (p *postgres) pendingTake(ctx context.Context, state string) (string, bool, error) {
	tx, e := p.begin(ctx, true)
	if e != nil {
		return "", false, e
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var payload []byte
	key := digest(state)
	e = tx.QueryRow(ctx, `DELETE FROM bff_session_slots WHERE hash=$1 AND kind='pending' AND expires_at>clock_timestamp() RETURNING pending_payload`, key).Scan(&payload)
	if errors.Is(e, pgx.ErrNoRows) {
		return "", false, nil
	}
	if e != nil {
		return "", false, ErrUnavailable
	}
	var value string
	if p.open(payload, "bff-pending-v1:"+key, &value) != nil || tx.Commit(ctx) != nil {
		return "", false, ErrUnavailable
	}
	return value, true, nil
}
func (p *postgres) ping(ctx context.Context) error {
	_, _, err := p.get(ctx, "readiness-probe")
	return err
}
func (p *postgres) sweep(ctx context.Context) error {
	tx, e := p.begin(ctx, true)
	if e != nil {
		return e
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if tx.Commit(ctx) != nil {
		return ErrUnavailable
	}
	return nil
}
