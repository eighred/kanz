-- #1292: queue insertion and authority evidence commit atomically. Migration
-- runner executes this entire file in one transaction; the temporary SELECT
-- policy exists only inside that transaction and only for its migration role.
LOCK TABLE bff_session_audit IN ACCESS EXCLUSIVE MODE;
CREATE TABLE bff_session_audit_tenants (
 tenant_id TEXT PRIMARY KEY
);
CREATE TRIGGER immutable_tenant_directory BEFORE UPDATE OR DELETE OR TRUNCATE ON bff_session_audit_tenants
 FOR EACH STATEMENT EXECUTE FUNCTION bff_session_audit_immutable();
-- Directory contains routing metadata only, never subjects or decision payloads.
CREATE UNIQUE INDEX bff_session_audit_tenant_sequence ON bff_session_audit(tenant_id,sequence);
CREATE TABLE bff_session_audit_pending (
 sequence BIGINT PRIMARY KEY,
 tenant_id TEXT NOT NULL,
 FOREIGN KEY (tenant_id,sequence) REFERENCES bff_session_audit(tenant_id,sequence),
 event_id UUID NOT NULL UNIQUE DEFAULT (
   (lpad(to_hex((extract(epoch FROM clock_timestamp())*1000)::bigint),12,'0') ||
   '7' || substr(replace(gen_random_uuid()::text,'-',''),14))::uuid
 )
);
CREATE TRIGGER immutable_delivery_identity BEFORE UPDATE ON bff_session_audit_pending
 FOR EACH STATEMENT EXECUTE FUNCTION bff_session_audit_immutable();
CREATE INDEX bff_session_audit_pending_tenant ON bff_session_audit_pending(tenant_id,sequence);
ALTER TABLE bff_session_audit_pending ENABLE ROW LEVEL SECURITY;
ALTER TABLE bff_session_audit_pending FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON bff_session_audit_pending
 USING (tenant_id=app_current_tenant()) WITH CHECK (tenant_id=app_current_tenant());
CREATE FUNCTION bff_session_audit_enqueue() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 INSERT INTO bff_session_audit_tenants VALUES(NEW.tenant_id) ON CONFLICT DO NOTHING;
 INSERT INTO bff_session_audit_pending(sequence,tenant_id) VALUES(NEW.sequence,NEW.tenant_id);
 PERFORM pg_notify('bff_session_audit', '');
 RETURN NEW;
END $$;
CREATE TRIGGER bff_session_audit_enqueue AFTER INSERT ON bff_session_audit
 FOR EACH ROW EXECUTE FUNCTION bff_session_audit_enqueue();
CREATE POLICY delivery_migration_only ON bff_session_audit FOR SELECT TO CURRENT_USER USING(true);
INSERT INTO bff_session_audit_tenants SELECT DISTINCT tenant_id FROM bff_session_audit;
DO $$ DECLARE t text; BEGIN
 FOR t IN SELECT tenant_id FROM bff_session_audit_tenants LOOP
   PERFORM set_config('app.tenant_id',t,true);
   INSERT INTO bff_session_audit_pending(sequence,tenant_id)
   SELECT sequence,tenant_id FROM bff_session_audit WHERE tenant_id=t;
 END LOOP;
END $$;
DROP POLICY delivery_migration_only ON bff_session_audit;
