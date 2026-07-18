-- WORM enforcement for the ledger (0001_ledger.sql calls it an "append-only
-- journal"; until this migration nothing made that true).
--
-- RLS restricts WHICH ROWS a role may touch. It does not restrict the COMMAND:
-- a session correctly scoped to its own tenant could UPDATE or DELETE its own
-- ledger history and RLS would permit every row. The application cannot do this
-- today -- ledger.Store exposes only Append/Journal/snapshots, and no UPDATE or
-- DELETE SQL exists in the accounting tree -- but that is a convention held by
-- the current shape of the Go code, and conventions decay. A compromised app
-- role, a migration written in a hurry, or one psql session is all it takes.
--
-- audit_log has been protected this way since AUDIT-01 (see
-- services/audit/migrations/0001_audit_log.sql). This is the same pattern,
-- applied to the table that records where the money went.
--
-- Deliberately BEFORE UPDATE OR DELETE with no exemption: there is no
-- "correction" path. A bitemporal ledger corrects by APPENDING a new entry at a
-- later knowledge_time, never by rewriting what was believed earlier -- that is
-- the whole point of storing knowledge_time. An UPDATE here would destroy the
-- audit trail the bitemporal model exists to preserve.

CREATE OR REPLACE FUNCTION ledger_entries_worm() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'ledger_entries is append-only (WORM): % is not permitted — correct by appending at a later knowledge_time, never by rewriting history', TG_OP;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS ledger_entries_no_mutate ON ledger_entries;
CREATE TRIGGER ledger_entries_no_mutate
    BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_entries_worm();
