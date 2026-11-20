-- Dated manual comparisons retain their own signed completion. Only ordinary
-- clean/breaks runs and the existing quantity-correction path own working items.
ALTER TABLE custody_runs DROP CONSTRAINT custody_run_signed_source_scope,
    ADD CONSTRAINT custody_run_signed_source_scope CHECK (
        (signed_statement_sha256 IS NULL OR (octet_length(signed_statement_sha256)=32
          AND outcome IN ('clean','breaks','historical_quantity_correction','historical_clean','historical_breaks')))
        AND (outcome<>'historical_quantity_correction' OR (
            signed_statement_sha256 IS NOT NULL AND manual_receipt IS NOT NULL
            AND COALESCE(jsonb_typeof(book_basis->'historical_quantity_detections')='object'
               AND book_basis->'historical_quantity_detections'<>'{}'::jsonb,false)
        ))
        AND (outcome NOT IN ('historical_clean','historical_breaks') OR COALESCE(
            values_verified AND failure_reason='' AND signed_statement_sha256 IS NOT NULL
            AND octet_length(manual_receipt) BETWEEN 1 AND 1048576
            AND run_id ~ '^manual:[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'
            AND book_basis->>'mode'='declared'
            AND jsonb_typeof(book_basis->'journal_position')='number'
            AND book_basis->>'journal_position' ~ '^(0|[1-9][0-9]*)$'
            AND book_basis->'transactions'->>'pass' IN ('balances_only','ran')
            AND jsonb_typeof(book_basis->'assignments')='array'
            AND NOT (book_basis ? 'historical_quantity_detections'),false))
    );

-- The accounting writer verifies both independent signatures and the
-- canonical completion bytes. Bind the relational provenance too, including
-- the precise version of the single account on the signed source for the day.
CREATE OR REPLACE FUNCTION custody_historical_comparison_origin() RETURNS trigger LANGUAGE plpgsql
SET search_path=pg_catalog,public,pg_temp AS $$
BEGIN
    IF NOT EXISTS (
        SELECT FROM custody_batch_receipts s
        JOIN custody_account_assignments a ON (a.tenant_id,a.portfolio_id,a.custodian_id,a.account_id)=
            (s.tenant_id,s.portfolio_id,s.custodian_id,s.account_id)
        JOIN custody_account_proofs p ON (p.tenant_id,p.account_id,p.proof_id)=(a.tenant_id,a.account_id,a.proof_id)
        WHERE (s.tenant_id,s.portfolio_id,s.custodian_id,s.business_date,s.statement_id)=
              (NEW.tenant_id,NEW.portfolio_id,NEW.custodian_id,NEW.business_date,NEW.statement_id)
          AND s.sha256=encode(NEW.signed_statement_sha256,'hex')
          AND NEW.book_basis->'accounts'->NEW.custodian_id=jsonb_build_array(s.account_id)
          AND NEW.book_basis->'assignments' @> jsonb_build_array(jsonb_build_object(
              'tenant_id',a.tenant_id,'portfolio_id',a.portfolio_id,'custodian_id',a.custodian_id,'account_id',a.account_id,
              'version',a.version,'proof_id',p.proof_id,'proof_sha256',encode(p.evidence_sha256,'hex'),
              'assignment_sha256',encode(a.evidence_sha256,'hex')))
          AND NOT a.withdrawn
          AND a.effective_from<=s.business_date AND s.business_date<a.effective_until
          AND p.effective_from<=s.business_date AND s.business_date<p.effective_until
          AND NOT EXISTS (SELECT FROM custody_account_assignments newer
              WHERE (newer.tenant_id,newer.account_id)=(a.tenant_id,a.account_id)
              AND newer.effective_from<=s.business_date AND newer.revision>a.revision)
          AND NOT EXISTS (SELECT FROM custody_batch_receipts newer
              WHERE (newer.tenant_id,newer.portfolio_id,newer.custodian_id,newer.business_date)=
                    (s.tenant_id,s.portfolio_id,s.custodian_id,s.business_date) AND newer.revision>s.revision)
          AND EXISTS (SELECT FROM custody_runs later WHERE
              (later.tenant_id,later.portfolio_id,later.custodian_id)=(s.tenant_id,s.portfolio_id,s.custodian_id)
              AND later.business_date>s.business_date AND later.outcome IN ('clean','breaks'))
    ) THEN
        RAISE EXCEPTION 'historical comparison lacks a later close or its effective signed source and account assignment';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER custody_historical_comparison_origin BEFORE INSERT OR UPDATE ON custody_runs
FOR EACH ROW WHEN (NEW.outcome IN ('historical_clean','historical_breaks'))
EXECUTE FUNCTION custody_historical_comparison_origin();
