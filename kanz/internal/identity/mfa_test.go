package identity_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/identity"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/fxamacker/cbor/v2"
	"github.com/go-webauthn/webauthn/webauthn"
)

// This authenticator signs the actual WebAuthn wire protocol. Only test private
// keys live in memory; the production verifier and PostgreSQL remain real.
type testPasskey struct {
	key   *ecdsa.PrivateKey
	id    []byte
	count uint32
}

func newPasskey(t *testing.T) *testPasskey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := make([]byte, 32)
	if _, err = rand.Read(id); err != nil {
		t.Fatal(err)
	}
	return &testPasskey{key: key, id: id}
}
func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
func jsonBytes(t *testing.T, v any) []byte {
	t.Helper()
	b, e := json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func mfaProvider(t *testing.T) *webauthn.WebAuthn {
	t.Helper()
	w, e := webauthn.New(&webauthn.Config{RPID: "kanz.example.test", RPDisplayName: "Kanz test", RPOrigins: []string{"https://kanz.example.test"}})
	if e != nil {
		t.Fatal(e)
	}
	return w
}
func ceremonyChallenge(t *testing.T, c *identity.MFACeremony) string {
	t.Helper()
	var data struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
		} `json:"publicKey"`
	}
	if e := json.Unmarshal(jsonBytes(t, c.Options), &data); e != nil {
		t.Fatal(e)
	}
	return data.PublicKey.Challenge
}
func (k *testPasskey) response(t *testing.T, c *identity.MFACeremony, registration bool, origin string, uv bool) []byte {
	t.Helper()
	typ := "webauthn.get"
	if registration {
		typ = "webauthn.create"
	}
	client := jsonBytes(t, map[string]any{"type": typ, "challenge": ceremonyChallenge(t, c), "origin": origin})
	rp := sha256.Sum256([]byte("kanz.example.test"))
	data := append([]byte{}, rp[:]...)
	flags := byte(1)
	if uv {
		flags |= 4
	}
	if registration {
		flags |= 64
	}
	data = append(data, flags)
	k.count++
	counter := make([]byte, 4)
	binary.BigEndian.PutUint32(counter, k.count)
	data = append(data, counter...)
	response := map[string]any{"clientDataJSON": b64(client)}
	if registration {
		pub, err := cbor.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: k.key.X.FillBytes(make([]byte, 32)), -3: k.key.Y.FillBytes(make([]byte, 32))})
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, make([]byte, 16)...)
		size := make([]byte, 2)
		binary.BigEndian.PutUint16(size, uint16(len(k.id)))
		data = append(data, size...)
		data = append(data, k.id...)
		data = append(data, pub...)
		att, err := cbor.Marshal(map[string]any{"fmt": "none", "attStmt": map[string]any{}, "authData": data})
		if err != nil {
			t.Fatal(err)
		}
		response["attestationObject"] = b64(att)
	} else {
		h := sha256.Sum256(client)
		message := append(append([]byte{}, data...), h[:]...)
		digest := sha256.Sum256(message)
		signature, err := ecdsa.SignASN1(rand.Reader, k.key, digest[:])
		if err != nil {
			t.Fatal(err)
		}
		response["authenticatorData"] = b64(data)
		response["signature"] = b64(signature)
		response["userHandle"] = nil
	}
	return jsonBytes(t, map[string]any{"id": b64(k.id), "rawId": b64(k.id), "type": "public-key", "response": response, "clientExtensionResults": map[string]any{}})
}
func mfaActor(u *identity.User, now time.Time) identity.Administration {
	return identity.Administration{Subject: u.Subject, Tenant: u.Tenant, SessionEpoch: u.SessionEpoch, IssuedAt: now, MFA: u.MFA}
}
func enrollKey(t *testing.T, st *identity.Postgres, w *webauthn.WebAuthn, u *identity.User, now time.Time) (*identity.User, *testPasskey) {
	t.Helper()
	a := mfaActor(u, now)
	c, e := st.BeginMFA(context.Background(), a, u.Credential, "register", "test key", now, w)
	if e != nil {
		t.Fatal(e)
	}
	key := newPasskey(t)
	fresh, e := st.FinishMFA(context.Background(), &a, c.ID, "register", key.response(t, c, true, "https://kanz.example.test", true), now, w)
	if e != nil {
		t.Fatal(e)
	}
	return fresh, key
}
func TestMFAConcurrentProofConsumptionRevokesAndPreservesRecovery(t *testing.T) {
	st, pool := newStorePool(t)
	ctx := context.Background()
	u, old := verifiedMailbox(t, st)
	now := time.Now().UTC()
	wa := mfaProvider(t)
	fresh, key := enrollKey(t, st, wa, u, now)
	if fresh.SessionEpoch != 1 || !fresh.MFA.Recent(now) {
		t.Fatal("enrollment failed to fence sessions or record assurance")
	}
	a := mfaActor(fresh, now)
	c, err := st.BeginMFA(ctx, a, old, "login", "", now, wa)
	if err != nil {
		t.Fatal(err)
	}
	proof := key.response(t, c, false, "https://kanz.example.test", true)
	var wg sync.WaitGroup
	results := make(chan error, 8)
	// Independent store instances and pooled database connections share no Go lock.
	other := identity.NewPostgres(pool)
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			store := st
			if i%2 == 1 {
				store = other
			}
			_, e := store.FinishMFA(ctx, nil, c.ID, "login", proof, now, wa)
			results <- e
		}()
	}
	wg.Wait()
	close(results)
	wins := 0
	for e := range results {
		if e == nil {
			wins++
		} else if !errors.Is(e, identity.ErrMFA) {
			t.Fatal(e)
		}
	}
	if wins != 1 {
		t.Fatalf("consumption commits=%d", wins)
	}
	stale, err := st.BeginMFA(ctx, a, "", "stepup", "", now, wa)
	if err != nil {
		t.Fatal(err)
	}
	if err = st.RequestRecovery(ctx, u.Subject, now); err != nil {
		t.Fatal(err)
	}
	mail, err := st.ClaimMail(ctx, now)
	if err != nil || mail == nil {
		t.Fatal("missing recovery mail", err)
	}
	replacement, _ := identity.HashCredential("synthetic-mfa-recovered-password")
	if err = st.ConsumeChallenge(ctx, mail.Token, "recover", replacement, now); err != nil {
		t.Fatal(err)
	}
	recovered, err := st.UserBySubject(ctx, u.Subject)
	if err != nil {
		t.Fatal(err)
	}
	if !recovered.MFA.Required || !recovered.MFA.VerifiedAt.IsZero() || recovered.SessionEpoch != 2 {
		t.Fatal("recovery removed MFA or fabricated assurance")
	}
	if _, err = st.FinishMFA(ctx, &a, stale.ID, "stepup", key.response(t, stale, false, "https://kanz.example.test", true), now, wa); !errors.Is(err, identity.ErrMFA) {
		t.Fatal("recovery resurrected challenge", err)
	}
	signingKey, _ := identity.GenerateKey()
	signer, _ := identity.NewSigner(signingKey, "https://issuer.test", "kanz", time.Hour, identity.WithClock(func() time.Time { return now }))
	if _, _, err = signer.Mint(recovered); err == nil {
		t.Fatal("password-only issuance bypassed enrolled MFA")
	}
	login, err := st.BeginMFA(ctx, mfaActor(recovered, now), replacement, "login", "", now, wa)
	if err != nil {
		t.Fatal(err)
	}
	authenticated, err := st.FinishMFA(ctx, nil, login.ID, "login", key.response(t, login, false, "https://kanz.example.test", true), now, wa)
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := signer.Mint(authenticated)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := signer.Verify(token, now)
	if err != nil || !claims.MFA.Recent(now) {
		t.Fatal("verified login did not carry assurance", err)
	}
}
func TestMFARejectsOriginUVPurposeExpiryAndSupersededCeremonies(t *testing.T) {
	st, _ := newStorePool(t)
	ctx := context.Background()
	u, _ := verifiedMailbox(t, st)
	now := time.Now().UTC()
	wa := mfaProvider(t)
	a := mfaActor(u, now)
	key := newPasskey(t)
	for _, tc := range []struct {
		name, origin, purpose string
		uv                    bool
		at                    time.Time
	}{{"origin", "https://evil.test", "register", true, now}, {"UV", "https://kanz.example.test", "register", false, now}, {"purpose", "https://kanz.example.test", "stepup", true, now}, {"expired", "https://kanz.example.test", "register", true, now.Add(identity.MFACeremonyTTL)}} {
		t.Run(tc.name, func(t *testing.T) {
			c, e := st.BeginMFA(ctx, a, u.Credential, "register", "test", now, wa)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = st.FinishMFA(ctx, &a, c.ID, tc.purpose, key.response(t, c, true, tc.origin, tc.uv), tc.at, wa); !errors.Is(e, identity.ErrMFA) {
				t.Fatal("bad proof accepted", e)
			}
		})
	}
	first, e := st.BeginMFA(ctx, a, u.Credential, "register", "test", now, wa)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = st.BeginMFA(ctx, a, u.Credential, "register", "replacement", now, wa); e != nil {
		t.Fatal(e)
	}
	if _, e = st.FinishMFA(ctx, &a, first.ID, "register", key.response(t, first, true, "https://kanz.example.test", true), now, wa); !errors.Is(e, identity.ErrMFA) {
		t.Fatal("superseded ceremony accepted", e)
	}
}
func TestMFAAuditRollbackAndFactorRemovalFence(t *testing.T) {
	st, pool := newStorePool(t)
	ctx := context.Background()
	u, _ := verifiedMailbox(t, st)
	now := time.Now().UTC()
	wa := mfaProvider(t)
	a := mfaActor(u, now)
	c, e := st.BeginMFA(ctx, a, u.Credential, "register", "primary", now, wa)
	if e != nil {
		t.Fatal(e)
	}
	key := newPasskey(t)
	proof := key.response(t, c, true, "https://kanz.example.test", true)
	if _, e = pool.Exec(ctx, `CREATE FUNCTION refuse_mfa_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'audit unavailable'; END $$; CREATE TRIGGER refuse_mfa_audit BEFORE INSERT ON identity_access_audit FOR EACH STATEMENT EXECUTE FUNCTION refuse_mfa_audit()`); e != nil {
		t.Fatal(e)
	}
	if _, e = st.FinishMFA(ctx, &a, c.ID, "register", proof, now, wa); e == nil {
		t.Fatal("audit failure committed")
	}
	loaded, e := st.UserBySubject(ctx, u.Subject)
	if e != nil || loaded.MFA.Required || loaded.SessionEpoch != 0 {
		t.Fatal("audit failure changed authority", e)
	}
	if _, e = pool.Exec(ctx, `DROP TRIGGER refuse_mfa_audit ON identity_access_audit`); e != nil {
		t.Fatal(e)
	}
	fresh, e := st.FinishMFA(ctx, &a, c.ID, "register", proof, now, wa)
	if e != nil {
		t.Fatal("rollback burnt ceremony", e)
	}
	a = mfaActor(fresh, now)
	if _, e = st.RemoveMFA(ctx, a, b64(key.id), now); !errors.Is(e, identity.ErrMFALastFactor) {
		t.Fatal("last factor removed", e)
	}
	fresh, spare := enrollKey(t, st, wa, fresh, now)
	a = mfaActor(fresh, now)
	pending, e := st.BeginMFA(ctx, a, "", "stepup", "", now, wa)
	if e != nil {
		t.Fatal(e)
	}
	removed, e := st.RemoveMFA(ctx, a, b64(key.id), now)
	if e != nil {
		t.Fatal(e)
	}
	if removed.SessionEpoch != 3 || !removed.MFA.Required {
		t.Fatal("factor removal failed to revoke")
	}
	if _, e = st.FinishMFA(ctx, &a, pending.ID, "stepup", spare.response(t, pending, false, "https://kanz.example.test", true), now, wa); !errors.Is(e, identity.ErrMFA) {
		t.Fatal("removed-factor epoch accepted", e)
	}
	expired := mfaActor(removed, now)
	expired.MFA.VerifiedAt = now.Add(-auth.StepUpTTL)
	if _, e = st.BeginMFA(ctx, expired, removed.Credential, "register", "bad", now, wa); !errors.Is(e, identity.ErrMFAStepUp) {
		t.Fatal("stale assurance added factor", e)
	}
}
func TestMFACounterRegressionAndTamperedSignatureRefuse(t *testing.T) {
	st, _ := newStorePool(t)
	ctx := context.Background()
	u, _ := verifiedMailbox(t, st)
	now := time.Now().UTC()
	wa := mfaProvider(t)
	fresh, key := enrollKey(t, st, wa, u, now)
	a := mfaActor(fresh, now)
	c, err := st.BeginMFA(ctx, a, fresh.Credential, "login", "", now, wa)
	if err != nil {
		t.Fatal(err)
	}
	key.count = 0 // Registration stored 1; repeating 1 is a clone warning.
	if _, err = st.FinishMFA(ctx, nil, c.ID, "login", key.response(t, c, false, "https://kanz.example.test", true), now, wa); !errors.Is(err, identity.ErrMFA) {
		t.Fatal("counter regression accepted", err)
	}
	proof := key.response(t, c, false, "https://kanz.example.test", true)
	var wire map[string]any
	if err = json.Unmarshal(proof, &wire); err != nil {
		t.Fatal(err)
	}
	wire["response"].(map[string]any)["signature"] = b64([]byte("not a signature"))
	if _, err = st.FinishMFA(ctx, nil, c.ID, "login", jsonBytes(t, wire), now, wa); !errors.Is(err, identity.ErrMFA) {
		t.Fatal("forged signature accepted", err)
	}
	if _, err = st.FinishMFA(ctx, nil, c.ID, "login", proof, now, wa); err != nil {
		t.Fatal("invalid proof consumed the legitimate challenge", err)
	}
}

func TestMFAScopeBoundsAndSensitiveCredentialChanges(t *testing.T) {
	st, pool := newStorePool(t)
	ctx := context.Background()
	u, _ := verifiedMailbox(t, st)
	now := time.Now().UTC()
	wa := mfaProvider(t)
	fresh, key := enrollKey(t, st, wa, u, now)
	a := mfaActor(fresh, now)
	ceremony, err := st.BeginMFA(ctx, a, "", "stepup", "", now, wa)
	if err != nil {
		t.Fatal(err)
	}
	proof := key.response(t, ceremony, false, "https://kanz.example.test", true)
	for _, foreign := range []identity.Administration{
		{Subject: a.Subject, Tenant: "another-tenant", SessionEpoch: a.SessionEpoch, IssuedAt: now, MFA: a.MFA},
		{Subject: "another-subject", Tenant: a.Tenant, SessionEpoch: a.SessionEpoch, IssuedAt: now, MFA: a.MFA},
	} {
		if _, err = st.MFAStatus(ctx, foreign, now); err == nil {
			t.Fatal("foreign status disclosed")
		}
		if _, err = st.FinishMFA(ctx, &foreign, ceremony.ID, "stepup", proof, now, wa); !errors.Is(err, identity.ErrMFA) {
			t.Fatal("foreign actor consumed proof", err)
		}
		if _, err = st.RemoveMFA(ctx, foreign, b64(key.id), now); err == nil {
			t.Fatal("foreign factor removed")
		}
	}
	if _, err = st.FinishMFA(ctx, &a, ceremony.ID, "stepup", proof, now, wa); err != nil {
		t.Fatal("foreign attempt burned owner proof", err)
	}
	stale := a
	stale.MFA.VerifiedAt = now.Add(-auth.StepUpTTL)
	if err = st.RotateCredential(ctx, stale, fresh.Credential, fresh.Credential, now); !errors.Is(err, identity.ErrCredentialMismatch) {
		t.Fatal("stale proof rotated password", err)
	}
	if err = st.EnrollMailbox(ctx, stale, fresh.Credential, "replacement@example.test", now); !errors.Is(err, identity.ErrCredentialMismatch) {
		t.Fatal("stale proof replaced mailbox", err)
	}
	for i := 1; i < identity.MaxMFAFactors; i++ {
		fresh, _ = enrollKey(t, st, wa, fresh, now)
	}
	a = mfaActor(fresh, now)
	if _, err = st.BeginMFA(ctx, a, fresh.Credential, "register", "overflow", now, wa); !errors.Is(err, identity.ErrMFA) {
		t.Fatal("factor limit ignored", err)
	}
	status, err := st.MFAStatus(ctx, a, now)
	if err != nil || len(status.Factors) != identity.MaxMFAFactors {
		t.Fatal("factor inventory disagrees", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE identity_users SET status='disabled' WHERE subject=$1`, fresh.Subject); err != nil {
		t.Fatal(err)
	}
	if _, err = st.BeginMFA(ctx, a, fresh.Credential, "login", "", now, wa); !errors.Is(err, identity.ErrMFA) {
		t.Fatal("disabled account began login", err)
	}
}
