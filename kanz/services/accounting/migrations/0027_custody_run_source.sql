-- A signed verdict attests the original source bytes actually compared.
-- Historical runs retain their original evidence; statement IDs alone cannot
-- prove which values a reconciler observed before a replacement or repair.
ALTER TABLE custody_runs ADD COLUMN signed_statement_sha256 BYTEA
    CHECK (signed_statement_sha256 IS NULL OR
           (octet_length(signed_statement_sha256) = 32 AND outcome IN ('clean', 'breaks')));
CREATE TRIGGER custody_run_signed_source_immutable BEFORE UPDATE OF signed_statement_sha256 ON custody_runs
    FOR EACH ROW EXECUTE FUNCTION ledger_entries_worm();
