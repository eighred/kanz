-- New and old writers share the balance/membership queue. Only explicit signed
-- measurements enter this append-only history; the legacy absence sweep cannot clear it.
CREATE TABLE custody_execution_quantity_observations (
    tenant_id text NOT NULL DEFAULT app_current_tenant(),
    observation bigint GENERATED ALWAYS AS IDENTITY,
    break_id text NOT NULL,
    run_id text NOT NULL,
    statement_id text NOT NULL,
    entry_id text NOT NULL,
    portfolio_id text NOT NULL,
    custodian_id text NOT NULL,
    business_date date NOT NULL,
    account_id text NOT NULL,
    instrument_id text NOT NULL,
    external_ref text NOT NULL,
    source_revision bigint NOT NULL CHECK (source_revision > 0),
    ibor text NOT NULL CHECK (length(ibor) <= 256 AND ibor ~ '^(0|-?[1-9][0-9]*|-?(0|[1-9][0-9]*)\.[0-9]*[1-9])$'),
    custodian text NOT NULL CHECK (length(custodian) <= 256 AND custodian ~ '^(0|-?[1-9][0-9]*|-?(0|[1-9][0-9]*)\.[0-9]*[1-9])$'),
    difference text NOT NULL CHECK (length(difference) <= 256 AND difference ~ '^(0|-?[1-9][0-9]*|-?(0|[1-9][0-9]*)\.[0-9]*[1-9])$'),
    first_seen_at timestamptz NOT NULL,
    measured_at timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, run_id, break_id),
    FOREIGN KEY (tenant_id, run_id) REFERENCES custody_runs(tenant_id, run_id),
    FOREIGN KEY (tenant_id, statement_id) REFERENCES custody_batch_receipts(tenant_id, statement_id),
    FOREIGN KEY (tenant_id, entry_id) REFERENCES ledger_entries(tenant_id, entry_id)
);
CREATE INDEX custody_quantity_item_latest ON custody_execution_quantity_observations
    (tenant_id, portfolio_id, custodian_id, business_date, break_id, source_revision DESC, observation DESC);
ALTER TABLE custody_execution_quantity_observations ENABLE ROW LEVEL SECURITY;
ALTER TABLE custody_execution_quantity_observations FORCE ROW LEVEL SECURITY;
CREATE POLICY custody_quantity_tenant ON custody_execution_quantity_observations
    USING (tenant_id = app_current_tenant()) WITH CHECK (tenant_id = app_current_tenant());
-- A generated working identity includes both independent authorities' signed
-- versions and the statement signer. JSON array framing preserves opaque UTF-8 refs.
CREATE OR REPLACE FUNCTION custody_quantity_key(text[]) RETURNS text
LANGUAGE sql IMMUTABLE STRICT SET search_path=pg_catalog AS $$
    SELECT 'q1:' || encode(sha256(convert_to(array_to_json($1)::text,'UTF8')),'hex')
$$;

-- Verify each statement's exact signed line and the actual journal entry before
-- accepting working observations. Parse a batch once for its set of matched rows.
DO $install$
DECLARE owning_schema name := pg_catalog.current_schema();
BEGIN
 EXECUTE pg_catalog.format($ddl$CREATE OR REPLACE FUNCTION %1$I.custody_quantity_observation_origin() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,%1$I,pg_temp AS %2$L$ddl$,
     owning_schema, $body$
BEGIN
    IF EXISTS (
        WITH signed_lines AS MATERIALIZED (
            SELECT s.tenant_id,s.statement_id,coalesce(line->>'externalRef',line->>'external_ref') ref,
                   coalesce(line->>'instrumentId',line->>'instrument_id') instrument,
                   ((coalesce(line->'quantity'->>'coefficient','0')::numeric::bigint)::text || 'e' ||
                    (coalesce(line->'quantity'->>'exponent','0')::numeric::integer)::text)::numeric amount
            FROM (SELECT DISTINCT tenant_id,statement_id FROM new_observations) n
            JOIN custody_batch_receipts s USING(tenant_id,statement_id)
            CROSS JOIN LATERAL jsonb_array_elements(convert_from(s.signed_batch,'UTF8')::jsonb #> '{statement,transactions}') line
            WHERE coalesce(line->'quantityInInstrumentUnits',line->'quantity_in_instrument_units')='true'::jsonb
              AND jsonb_typeof(line->'quantity')='object'
        )
        SELECT FROM new_observations n
        WHERE NOT EXISTS (
            SELECT FROM custody_runs r
            JOIN custody_batch_receipts s USING(tenant_id,statement_id,portfolio_id,custodian_id,business_date)
            JOIN ledger_entries e ON e.tenant_id=r.tenant_id AND e.entry_id=n.entry_id AND e.portfolio_id=r.portfolio_id
            JOIN ledger_append_positions pos ON (pos.tenant_id,pos.portfolio_id,pos.entry_id)=(e.tenant_id,e.portfolio_id,e.entry_id)
            JOIN signed_lines l ON l.tenant_id=r.tenant_id AND l.statement_id=r.statement_id AND l.ref=n.external_ref
            CROSS JOIN LATERAL jsonb_array_elements(r.book_basis->'assignments') a
            WHERE r.tenant_id=n.tenant_id AND r.run_id=n.run_id AND r.statement_id=n.statement_id
              AND r.portfolio_id=n.portfolio_id AND r.custodian_id=n.custodian_id AND r.business_date=n.business_date
              AND r.outcome IN ('clean','breaks') AND r.signed_statement_sha256=decode(s.sha256,'hex')
              AND r.completed_at=n.measured_at
              AND n.first_seen_at=coalesce((
                  SELECT CASE WHEN prev.difference<>'0' THEN prev.first_seen_at ELSE n.measured_at END
                  FROM custody_execution_quantity_observations prev
                  WHERE prev.tenant_id=n.tenant_id AND prev.portfolio_id=n.portfolio_id AND prev.custodian_id=n.custodian_id
                    AND prev.business_date=n.business_date AND prev.break_id=n.break_id AND prev.observation<n.observation
                  ORDER BY prev.source_revision DESC,prev.observation DESC LIMIT 1
              ),n.measured_at)
              AND r.manual_receipt IS NOT NULL AND r.book_basis->>'mode'='declared'
              AND r.book_basis->'transactions'->>'pass'='ran'
              AND r.book_basis->'measured_execution_entries'->>e.entry_id=n.external_ref
              AND r.book_basis->'accounts'->r.custodian_id=jsonb_build_array(s.account_id)
              AND a->>'account_id'=s.account_id AND a->>'custodian_id'=s.custodian_id AND a->>'portfolio_id'=s.portfolio_id
              AND n.break_id=custody_quantity_key(ARRAY[n.tenant_id,n.portfolio_id,n.custodian_id,to_char(n.business_date,'YYYY-MM-DD'),
                   s.account_id,a->>'proof_sha256',a->>'assignment_sha256',s.source_id,encode(s.public_key,'hex'),n.external_ref,n.instrument_id])
              AND s.account_id=n.account_id AND s.revision=n.source_revision
              AND pos.position<=(r.book_basis->>'journal_position')::bigint
              AND NOT EXISTS (
                  SELECT FROM ledger_entries other JOIN ledger_append_positions op USING(tenant_id,portfolio_id,entry_id)
                  WHERE other.tenant_id=e.tenant_id AND other.portfolio_id=e.portfolio_id AND other.venue_account_id=e.venue_account_id
                    AND other.entry_type=1 AND other.entry_id<>e.entry_id AND other.source_ref=e.source_ref
                    AND (coalesce(other.settlement_date,other.effective_time) AT TIME ZONE 'UTC')::date=n.business_date
                    AND op.position<=(r.book_basis->>'journal_position')::bigint
              )
              AND e.entry_type=1 AND e.venue_account_id=n.account_id AND e.source_ref=n.external_ref AND e.instrument_id=n.instrument_id
              AND (coalesce(e.settlement_date,e.effective_time) AT TIME ZONE 'UTC')::date=n.business_date
              AND e.quantity IS NOT NULL AND l.instrument=n.instrument_id AND l.amount=n.custodian::numeric
              AND split_part(e.quantity,'/',1)::numeric =
                  n.ibor::numeric * coalesce(nullif(split_part(e.quantity,'/',2),''),'1')::numeric
              AND n.difference::numeric=n.ibor::numeric-n.custodian::numeric
        )
    ) THEN
        RAISE EXCEPTION 'custody quantity observation lacks a corresponding signed measurement and booked trade';
    END IF;
    RETURN NULL;
END;
$body$);
END;
$install$;
CREATE TRIGGER custody_quantity_origin AFTER INSERT ON custody_execution_quantity_observations
    REFERENCING NEW TABLE AS new_observations FOR EACH STATEMENT EXECUTE FUNCTION custody_quantity_observation_origin();
CREATE TRIGGER custody_quantity_observation_immutable BEFORE UPDATE OR DELETE ON custody_execution_quantity_observations
    FOR EACH ROW EXECUTE FUNCTION ledger_entries_worm();
CREATE TRIGGER custody_quantity_observation_no_truncate BEFORE TRUNCATE ON custody_execution_quantity_observations
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_entries_worm();
