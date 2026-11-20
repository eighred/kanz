-- Allocate publication identity with the row, including retained rows laid
-- out before this migration. Relay attempts never assign or change it.
ALTER TABLE outbox ADD COLUMN event_id UUID NOT NULL UNIQUE DEFAULT (
    (lpad(to_hex(floor(extract(epoch FROM clock_timestamp()) * 1000)::bigint),12,'0') ||
     '7' || substr(replace(gen_random_uuid()::text,'-',''),14))::uuid
);
ALTER TABLE outbox ADD CONSTRAINT outbox_event_id_uuidv7 CHECK (
    get_byte(uuid_send(event_id), 6) >> 4 = 7 AND get_byte(uuid_send(event_id), 8) >> 6 = 2
);

CREATE FUNCTION keep_outbox_event_id() RETURNS trigger LANGUAGE plpgsql
    SET search_path=pg_catalog,pg_temp AS $$
BEGIN
    IF NEW.event_id IS DISTINCT FROM OLD.event_id THEN
        RAISE EXCEPTION 'outbox event identity is immutable';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER keep_outbox_event_id BEFORE UPDATE OF event_id ON outbox
    FOR EACH ROW EXECUTE FUNCTION keep_outbox_event_id();
