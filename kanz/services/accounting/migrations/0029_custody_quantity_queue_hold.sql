-- Reserve the execution-quantity queue kind until resolution is scoped to the
-- original signed account, business date, reference, instrument and measured unit.
-- Older reconcilers resolve all otherwise absent breaks for a portfolio/custodian.
CREATE TRIGGER custody_quantity_queue_insert_hold BEFORE INSERT ON custody_breaks
    FOR EACH ROW WHEN (NEW.kind = 'execution_quantity') EXECUTE FUNCTION ledger_entries_worm();
CREATE TRIGGER custody_quantity_queue_update_hold BEFORE UPDATE ON custody_breaks
    FOR EACH ROW WHEN (OLD.kind = 'execution_quantity' OR NEW.kind = 'execution_quantity') EXECUTE FUNCTION ledger_entries_worm();
CREATE TRIGGER custody_quantity_queue_delete_hold BEFORE DELETE ON custody_breaks
    FOR EACH ROW WHEN (OLD.kind = 'execution_quantity') EXECUTE FUNCTION ledger_entries_worm();
