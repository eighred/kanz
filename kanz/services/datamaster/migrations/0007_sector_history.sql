-- Dated source editions are append-only. Current golden records cannot prove a
-- historical effective day or when the book acquired that observation.
CREATE TABLE sector_classification_history (
    tenant_id TEXT NOT NULL DEFAULT public.app_current_tenant(),
    instrument_id TEXT NOT NULL,
    effective_date DATE NOT NULL CHECK (isfinite(effective_date)),
    source_as_of TIMESTAMPTZ NOT NULL CHECK (isfinite(source_as_of)),
    known_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp() CHECK (isfinite(known_at)),
    taxonomy TEXT NOT NULL,
    code TEXT NOT NULL,
    name TEXT NOT NULL,
    vendor TEXT NOT NULL,
    CHECK (source_as_of <= known_at),
    CHECK ((taxonomy = '') = (code = '')),
    PRIMARY KEY (tenant_id, instrument_id, effective_date, source_as_of)
);
ALTER TABLE sector_classification_history ENABLE ROW LEVEL SECURITY;
ALTER TABLE sector_classification_history FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON sector_classification_history
    USING (tenant_id=public.app_current_tenant()) WITH CHECK (tenant_id=public.app_current_tenant());

CREATE FUNCTION public.refuse_sector_history_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'sector classification history is immutable'; END $$;
CREATE TRIGGER sector_history_immutable BEFORE UPDATE OR DELETE ON sector_classification_history
    FOR EACH ROW EXECUTE FUNCTION public.refuse_sector_history_mutation();
CREATE TRIGGER sector_history_no_truncate BEFORE TRUNCATE ON sector_classification_history
    FOR EACH STATEMENT EXECUTE FUNCTION public.refuse_sector_history_mutation();
