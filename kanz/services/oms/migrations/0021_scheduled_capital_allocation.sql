-- The parent row is the cancellation barrier and the quantity allocation
-- mutex. A child increments this exact total before its order and outbox commit;
-- rollback of any later write restores the total with the child admission.
ALTER TABLE orders ADD COLUMN allocated_quantity numeric NOT NULL DEFAULT 0
    CHECK (allocated_quantity >= 0);

CREATE FUNCTION reject_scheduled_allocation_decrease() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.allocated_quantity < OLD.allocated_quantity THEN
        RAISE EXCEPTION 'scheduled capital allocation is monotonic' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER scheduled_allocation_monotonic BEFORE UPDATE ON orders
    FOR EACH ROW EXECUTE FUNCTION reject_scheduled_allocation_decrease();
