package session

import (
	"bytes"
	"testing"
	"time"
)

func TestSessionQueueFailureRollsBackReplacementAndLogout(t *testing.T) {
	pool := testPool(t)
	ctx := t.Context()
	m, err := NewPostgres(ctx, pool, bytes.Repeat([]byte{9}, 32), time.Hour, Limits{Sessions: 4, Pending: 2, PerSubject: 2})
	if err != nil {
		t.Fatal(err)
	}
	s := Session{Tenant: "fund", Subject: "operator", AccessToken: "synthetic-secret"}
	id, err := m.Create(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `CREATE FUNCTION refuse_pending_evidence() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'queue unavailable'; END $$; CREATE TRIGGER refuse_pending_evidence BEFORE INSERT ON bff_session_audit_pending FOR EACH ROW EXECUTE FUNCTION refuse_pending_evidence()`); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Replace(ctx, id, s, true); err != ErrUnavailable {
		t.Fatal("replacement committed without durable delivery", err)
	}
	if err = m.Delete(ctx, id); err != ErrUnavailable {
		t.Fatal("logout committed without durable delivery", err)
	}
	if actual, ok, e := m.Get(ctx, id); e != nil || !ok || actual.AccessToken != s.AccessToken {
		t.Fatal("failed audit queue changed session authority", e)
	}
}
