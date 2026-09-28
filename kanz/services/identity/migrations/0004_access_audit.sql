-- Authority edits and their evidence commit together. The canonical DecisionLog
-- payload can subsequently be delivered to the central audit projection.
ALTER TABLE identity_users ADD COLUMN IF NOT EXISTS access_revision BIGINT NOT NULL DEFAULT 0 CHECK (access_revision >= 0);
CREATE INDEX IF NOT EXISTS identity_users_tenant_subject ON identity_users (tenant_id, subject);
CREATE INDEX IF NOT EXISTS identity_invites_redeemed_creator ON identity_invites (subject, tenant_id, redeemed_at) WHERE redeemed_at IS NOT NULL;
CREATE TABLE IF NOT EXISTS identity_access_audit (
    sequence BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id TEXT NOT NULL,
    subject TEXT NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL,
    decision JSONB NOT NULL
);
CREATE INDEX IF NOT EXISTS identity_access_audit_subject ON identity_access_audit (tenant_id, subject, sequence);
-- Match the estate's raising tenant guard: missing scope is an error, not an
-- empty audit history that could be mistaken for "no access changes occurred".
CREATE OR REPLACE FUNCTION app_current_tenant() RETURNS text
LANGUAGE plpgsql STABLE AS $fn$
DECLARE t text;
BEGIN
    t := current_setting('app.tenant_id', true);
    IF t IS NULL OR t = '' THEN
        RAISE EXCEPTION 'kanz: tenant scope missing — app.tenant_id is not set on this session'
            USING ERRCODE = '42501';
    END IF;
    RETURN t;
END $fn$;
ALTER TABLE identity_access_audit ENABLE ROW LEVEL SECURITY;
ALTER TABLE identity_access_audit FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS identity_access_audit_tenant ON identity_access_audit;
CREATE POLICY identity_access_audit_tenant ON identity_access_audit
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());
CREATE OR REPLACE FUNCTION identity_access_audit_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$ BEGIN
    RAISE EXCEPTION 'identity access audit is append-only';
END $$;
DROP TRIGGER IF EXISTS identity_access_audit_immutable ON identity_access_audit;
CREATE TRIGGER identity_access_audit_immutable
    BEFORE UPDATE OR DELETE OR TRUNCATE ON identity_access_audit
    FOR EACH STATEMENT EXECUTE FUNCTION identity_access_audit_immutable();
