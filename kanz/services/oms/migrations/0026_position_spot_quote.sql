-- NULL is an older writer's unclassified lot. Empty is a verified legacy
-- fold. A native spot lot records its actual quote currency.
ALTER TABLE positions ADD COLUMN spot_quote TEXT;

-- Latest observed mark across venues for one instrument and tenant.
CREATE INDEX position_history_fund_mark ON position_execution_history
    (tenant_id, portfolio_id, instrument_id, execution_time DESC, execution_key DESC);

-- Older writers also take the instrument lock but would fold gross size,
-- or acknowledge an existing claim and republish it in the configured currency.
CREATE FUNCTION require_spot_position_writer() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    native_lot BOOLEAN;
BEGIN
    IF current_setting('app.position_native_fees_v1', true) = 'on' THEN
        RETURN NEW;
    END IF;
    IF TG_TABLE_NAME = 'position_fills' THEN
        SELECT EXISTS (
            SELECT 1 FROM position_execution_history h JOIN positions p
              USING (tenant_id,portfolio_id,venue,instrument_id)
            WHERE h.tenant_id=NEW.tenant_id AND h.execution_key=NEW.fill_id AND p.spot_quote<>''
        ) INTO native_lot;
    ELSE
        SELECT COALESCE(NEW.spot_quote<>'',false) OR EXISTS (
            SELECT 1 FROM positions p WHERE p.tenant_id=NEW.tenant_id
              AND p.portfolio_id=NEW.portfolio_id AND p.instrument_id=NEW.instrument_id
              AND p.spot_quote<>''
        ) INTO native_lot;
    END IF;
    IF native_lot THEN
        RAISE EXCEPTION 'verified spot inventory requires the native fee position writer';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER spot_position_claim BEFORE INSERT ON position_fills
    FOR EACH ROW EXECUTE FUNCTION require_spot_position_writer();
CREATE TRIGGER spot_position_write BEFORE INSERT OR UPDATE ON positions
    FOR EACH ROW EXECUTE FUNCTION require_spot_position_writer();
