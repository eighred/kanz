-- Preserve migration 0022's locking, membership rule and check-violation
-- SQLSTATE. Clients can now distinguish this trigger from unrelated checks
-- without interpreting localized database text. Existing rows are untouched.
DO $migration$
DECLARE
    guard regprocedure := 'require_funded_order_after_activation()'::regprocedure;
    definer boolean;
    settings text[];
    setting record;
    previous_value text;
BEGIN
    -- CREATE OR REPLACE preserves ownership and grants, but resets security
    -- mode and function-local settings. Preserve a restricted trigger owner's
    -- search path when upgrading an existing estate.
    SELECT prosecdef, proconfig INTO STRICT definer, settings FROM pg_proc WHERE oid=guard;

    CREATE OR REPLACE FUNCTION require_funded_order_after_activation() RETURNS trigger LANGUAGE plpgsql AS $$
    DECLARE armed boolean;
    BEGIN
        INSERT INTO capital_activation(tenant_id) VALUES(NEW.tenant_id) ON CONFLICT DO NOTHING;
        SELECT enabled INTO armed FROM capital_activation WHERE tenant_id=NEW.tenant_id FOR SHARE;
        IF armed AND NOT EXISTS (
            SELECT 1 FROM capital_members m
            WHERE m.tenant_id=NEW.tenant_id AND m.order_id=NEW.order_id
        ) THEN
            RAISE EXCEPTION 'capital activation refuses an unfunded order'
                USING ERRCODE='23514', CONSTRAINT='funded_order_activation',
                      TABLE=TG_TABLE_NAME, SCHEMA=TG_TABLE_SCHEMA;
        END IF;
        RETURN NEW;
    END $$;

    FOR setting IN SELECT * FROM pg_options_to_table(settings) LOOP
        previous_value := current_setting(setting.option_name,true);
        PERFORM set_config(setting.option_name,setting.option_value,true);
        EXECUTE format('ALTER FUNCTION %s SET %I FROM CURRENT', guard, setting.option_name);
        PERFORM set_config(setting.option_name,previous_value,true);
    END LOOP;
    IF definer THEN
        EXECUTE format('ALTER FUNCTION %s SECURITY DEFINER', guard);
    END IF;
END $migration$;
