-- Slots contain only hashed capabilities, expiry and encrypted pre-auth PKCE.
-- They are the global capacity/expiry directory before a tenant is known (#1287).
CREATE TABLE IF NOT EXISTS bff_session_policy (
 singleton BOOLEAN PRIMARY KEY CHECK(singleton),
 max_sessions INTEGER NOT NULL CHECK(max_sessions BETWEEN 1 AND 100000), max_pending INTEGER NOT NULL CHECK(max_pending BETWEEN 1 AND 10000), per_subject INTEGER NOT NULL CHECK(per_subject BETWEEN 1 AND 100 AND per_subject<=max_sessions),
 key_fingerprint TEXT NOT NULL, ttl_ns BIGINT NOT NULL CHECK(ttl_ns>0 AND ttl_ns<=86400000000000)
);
CREATE TABLE IF NOT EXISTS bff_session_slots (
 hash TEXT PRIMARY KEY CHECK(length(hash)=64),
 kind TEXT NOT NULL CHECK(kind IN ('session','pending')),
 expires_at TIMESTAMPTZ NOT NULL,
 pending_payload BYTEA,
 CHECK ((kind='pending')=(pending_payload IS NOT NULL))
);
CREATE INDEX IF NOT EXISTS bff_session_slots_expiry ON bff_session_slots(expires_at);
CREATE TABLE IF NOT EXISTS bff_sessions (
 hash TEXT PRIMARY KEY REFERENCES bff_session_slots(hash) ON DELETE CASCADE,
 tenant_id TEXT NOT NULL, subject TEXT NOT NULL, authority TEXT NOT NULL,
 created_at TIMESTAMPTZ NOT NULL, payload BYTEA NOT NULL
);
CREATE INDEX IF NOT EXISTS bff_sessions_owner ON bff_sessions(tenant_id,subject,authority);
CREATE OR REPLACE FUNCTION app_current_tenant() RETURNS text
LANGUAGE plpgsql STABLE AS $fn$
DECLARE t text;
BEGIN
 t:=current_setting('app.tenant_id',true);
 IF t IS NULL OR t='' THEN RAISE EXCEPTION 'kanz: tenant scope missing' USING ERRCODE='42501'; END IF;
 RETURN t;
END $fn$;
ALTER TABLE bff_sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE bff_sessions FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS bff_sessions_scope ON bff_sessions;
CREATE POLICY bff_sessions_scope ON bff_sessions
 USING (hash=current_setting('app.bff_hash',true) OR
 (tenant_id=app_current_tenant() AND subject=current_setting('app.bff_subject',true) AND authority=current_setting('app.bff_authority',true)))
 WITH CHECK (tenant_id=app_current_tenant() AND subject=current_setting('app.bff_subject',true) AND authority=current_setting('app.bff_authority',true));
CREATE TABLE IF NOT EXISTS bff_session_audit (
 sequence BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 tenant_id TEXT NOT NULL, subject TEXT NOT NULL, occurred_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(), decision JSONB NOT NULL
);
ALTER TABLE bff_session_audit ENABLE ROW LEVEL SECURITY;
ALTER TABLE bff_session_audit FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS bff_session_audit_scope ON bff_session_audit;
CREATE POLICY bff_session_audit_scope ON bff_session_audit
 USING (tenant_id=app_current_tenant()) WITH CHECK (tenant_id=app_current_tenant());
CREATE OR REPLACE FUNCTION bff_session_audit_immutable() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 RAISE EXCEPTION 'session audit is append-only'; END $$;
DROP TRIGGER IF EXISTS bff_session_audit_immutable ON bff_session_audit;
CREATE TRIGGER bff_session_audit_immutable BEFORE UPDATE OR DELETE OR TRUNCATE ON bff_session_audit FOR EACH STATEMENT EXECUTE FUNCTION bff_session_audit_immutable();
