-- Custody identity and governance are independent, operator-provisioned principals.
-- No runtime role may modify the authority registries. Exact scopes can be added
-- for an existing signer without sharing its key or giving it another duty.
CREATE TABLE custody_proof_issuers (
 login_role name PRIMARY KEY, kind text NOT NULL CHECK(kind IN ('venue_identity','custody_assignment')),
 tenant_id text NOT NULL CHECK(tenant_id<>''), authority text NOT NULL UNIQUE CHECK(authority<>''),
 public_key bytea NOT NULL UNIQUE CHECK(octet_length(public_key)=32),
 UNIQUE(login_role,tenant_id), UNIQUE(login_role,tenant_id,authority)
);
CREATE TABLE custody_proof_scopes (
 login_role name NOT NULL, tenant_id text NOT NULL, venue text NOT NULL DEFAULT '',
 account_id text NOT NULL CHECK(account_id<>''), exchange_account_id text NOT NULL DEFAULT '',
 portfolio_id text NOT NULL DEFAULT '', custodian_id text NOT NULL DEFAULT '',
 PRIMARY KEY(login_role,tenant_id,venue,account_id,exchange_account_id,portfolio_id,custodian_id),
 FOREIGN KEY(login_role,tenant_id) REFERENCES custody_proof_issuers(login_role,tenant_id),
 CHECK ((venue<>'' AND exchange_account_id<>'' AND portfolio_id='' AND custodian_id='')
    OR (venue='' AND exchange_account_id='' AND portfolio_id<>'' AND custodian_id<>''))
);
CREATE TABLE custody_assignment_readers (
 login_role name NOT NULL, tenant_id text NOT NULL CHECK(tenant_id<>''), PRIMARY KEY(login_role,tenant_id)
);
ALTER TABLE custody_assignment_readers ENABLE ROW LEVEL SECURITY;
CREATE POLICY assignment_reader_self ON custody_assignment_readers FOR SELECT USING(login_role=session_user);
ALTER TABLE custody_proof_issuers ENABLE ROW LEVEL SECURITY;
CREATE POLICY assignment_issuer_read ON custody_proof_issuers FOR SELECT USING(
 tenant_id=app_current_tenant() AND (login_role=session_user OR EXISTS(
 SELECT FROM custody_assignment_readers r WHERE r.login_role=session_user AND r.tenant_id=custody_proof_issuers.tenant_id)));
ALTER TABLE custody_proof_scopes ENABLE ROW LEVEL SECURITY;
CREATE POLICY assignment_scope_read ON custody_proof_scopes FOR SELECT USING(
 tenant_id=app_current_tenant() AND (login_role=session_user OR EXISTS(
 SELECT FROM custody_assignment_readers r WHERE r.login_role=session_user AND r.tenant_id=custody_proof_scopes.tenant_id)));
CREATE TABLE custody_account_proofs (
 tenant_id text NOT NULL DEFAULT app_current_tenant(), proof_id text NOT NULL CHECK(proof_id<>''), venue text NOT NULL,
 account_id text NOT NULL, exchange_account_id text NOT NULL, authority text NOT NULL,
 issuer_role name NOT NULL DEFAULT session_user,
 effective_from date NOT NULL, effective_until date NOT NULL CHECK(effective_from<effective_until),
 evidence_sha256 bytea NOT NULL CHECK(octet_length(evidence_sha256)=32),
 evidence_payload bytea NOT NULL CHECK(octet_length(evidence_payload) BETWEEN 1 AND 65536 AND sha256(evidence_payload)=evidence_sha256),
 signature bytea NOT NULL CHECK(octet_length(signature)=64),recorded_at timestamptz NOT NULL,
 PRIMARY KEY(tenant_id,proof_id), UNIQUE(tenant_id,account_id,proof_id),
 FOREIGN KEY(issuer_role,tenant_id,authority) REFERENCES custody_proof_issuers(login_role,tenant_id,authority)
);
CREATE TABLE custody_account_assignments (
 tenant_id text NOT NULL DEFAULT app_current_tenant(), version text NOT NULL CHECK(version<>''), revision bigint NOT NULL CHECK(revision>0),
 withdrawn boolean NOT NULL DEFAULT false CHECK(NOT withdrawn OR revision>1),
 preceding_revision bigint GENERATED ALWAYS AS (nullif(revision-1,0)) STORED,
 portfolio_id text NOT NULL, custodian_id text NOT NULL, account_id text NOT NULL, proof_id text NOT NULL,authority text NOT NULL,
 issuer_role name NOT NULL DEFAULT session_user,
 effective_from date NOT NULL,effective_until date NOT NULL CHECK(effective_from<effective_until),
 evidence_sha256 bytea NOT NULL CHECK(octet_length(evidence_sha256)=32),
 evidence_payload bytea NOT NULL CHECK(octet_length(evidence_payload) BETWEEN 1 AND 65536 AND sha256(evidence_payload)=evidence_sha256),
 signature bytea NOT NULL CHECK(octet_length(signature)=64),recorded_at timestamptz NOT NULL,
 PRIMARY KEY(tenant_id,version), UNIQUE(tenant_id,account_id,revision),
 FOREIGN KEY(tenant_id,account_id,preceding_revision) REFERENCES custody_account_assignments(tenant_id,account_id,revision),
 FOREIGN KEY(tenant_id,account_id,proof_id) REFERENCES custody_account_proofs(tenant_id,account_id,proof_id),
 FOREIGN KEY(issuer_role,tenant_id,authority) REFERENCES custody_proof_issuers(login_role,tenant_id,authority)
);
CREATE INDEX custody_assignment_by_date ON custody_account_assignments(tenant_id,account_id,effective_from DESC,revision DESC);
ALTER TABLE custody_account_proofs ENABLE ROW LEVEL SECURITY;
ALTER TABLE custody_account_proofs FORCE ROW LEVEL SECURITY;
CREATE POLICY custody_proof_view ON custody_account_proofs FOR SELECT USING(tenant_id=app_current_tenant() AND
 (issuer_role=session_user OR EXISTS(SELECT FROM custody_assignment_readers r WHERE r.login_role=session_user AND r.tenant_id=custody_account_proofs.tenant_id)));
CREATE POLICY custody_proof_issue ON custody_account_proofs FOR INSERT WITH CHECK(tenant_id=app_current_tenant() AND issuer_role=session_user AND
 EXISTS(SELECT FROM custody_proof_issuers i JOIN custody_proof_scopes s USING(login_role,tenant_id)
 WHERE i.login_role=session_user AND i.kind='venue_identity' AND i.tenant_id=custody_account_proofs.tenant_id
 AND i.authority=custody_account_proofs.authority AND (s.venue,s.account_id,s.exchange_account_id) =
 (custody_account_proofs.venue,custody_account_proofs.account_id,custody_account_proofs.exchange_account_id)));
ALTER TABLE custody_account_assignments ENABLE ROW LEVEL SECURITY;
ALTER TABLE custody_account_assignments FORCE ROW LEVEL SECURITY;
CREATE POLICY custody_assignment_view ON custody_account_assignments FOR SELECT USING(tenant_id=app_current_tenant() AND
 (issuer_role=session_user OR EXISTS(SELECT FROM custody_assignment_readers r WHERE r.login_role=session_user AND r.tenant_id=custody_account_assignments.tenant_id)));
CREATE POLICY custody_assignment_issue ON custody_account_assignments FOR INSERT WITH CHECK(tenant_id=app_current_tenant() AND issuer_role=session_user AND
 EXISTS(SELECT FROM custody_proof_issuers i JOIN custody_proof_scopes s USING(login_role,tenant_id)
 WHERE i.login_role=session_user AND i.kind='custody_assignment' AND i.tenant_id=custody_account_assignments.tenant_id
 AND i.authority=custody_account_assignments.authority AND (s.portfolio_id,s.custodian_id,s.account_id) =
 (custody_account_assignments.portfolio_id,custody_account_assignments.custodian_id,custody_account_assignments.account_id)));
CREATE OR REPLACE FUNCTION custody_record_assignment_knowledge() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 -- Comparing a complete portfolio set must serialize with newly added accounts too.
 PERFORM pg_advisory_xact_lock(hashtextextended('custody-assignment-tenant:' || NEW.tenant_id,0));
 IF TG_TABLE_NAME='custody_account_assignments' THEN
   IF NEW.withdrawn AND NOT EXISTS (
      SELECT FROM custody_account_assignments a WHERE a.tenant_id=NEW.tenant_id AND a.account_id=NEW.account_id
      AND a.revision=NEW.revision-1 AND NOT a.withdrawn AND a.portfolio_id=NEW.portfolio_id AND a.custodian_id=NEW.custodian_id
      AND a.effective_from <= NEW.effective_from AND NEW.effective_from < a.effective_until
   ) THEN RAISE EXCEPTION 'withdrawal requires its effective predecessor'; END IF;
 END IF;
 NEW.recorded_at := clock_timestamp(); RETURN NEW;
END;
$$;
CREATE TRIGGER custody_proof_knowledge BEFORE INSERT ON custody_account_proofs FOR EACH ROW EXECUTE FUNCTION custody_record_assignment_knowledge();
CREATE TRIGGER custody_assignment_knowledge BEFORE INSERT ON custody_account_assignments FOR EACH ROW EXECUTE FUNCTION custody_record_assignment_knowledge();
DO $$
DECLARE t text;
BEGIN
 FOREACH t IN ARRAY ARRAY['custody_proof_issuers','custody_proof_scopes','custody_account_proofs','custody_account_assignments'] LOOP
   EXECUTE format('CREATE TRIGGER immutable_custody_authority BEFORE UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION ledger_entries_worm()',t);
   EXECUTE format('CREATE TRIGGER immutable_custody_authority_truncate BEFORE TRUNCATE ON %I FOR EACH STATEMENT EXECUTE FUNCTION ledger_entries_worm()',t);
 END LOOP;
END;
$$;
-- During a rolling upgrade, previously deployed accounting binaries must fail
-- closed for a newly enrolled login. Legacy deployments have no enrollment.
CREATE OR REPLACE FUNCTION custody_require_proven_run() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,pg_temp AS $$
BEGIN
 IF EXISTS(SELECT FROM public.custody_assignment_readers r WHERE r.login_role=session_user AND r.tenant_id=NEW.tenant_id)
 AND NEW.outcome IN ('clean','breaks') AND
 CASE WHEN jsonb_typeof(NEW.book_basis->'assignments')='array' THEN jsonb_array_length(NEW.book_basis->'assignments')=0 ELSE true END
 THEN RAISE EXCEPTION 'enrolled accounting requires effective custody assignment evidence'; END IF;
 RETURN NEW;
END;
$$;
REVOKE ALL ON FUNCTION custody_require_proven_run() FROM PUBLIC;
CREATE TRIGGER custody_run_requires_assignment BEFORE INSERT ON custody_runs FOR EACH ROW EXECUTE FUNCTION custody_require_proven_run();
