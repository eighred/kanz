-- Apply only after the canonical OMS migrations in a disposable database.
DO $$
BEGIN
  IF current_database() <> 'kanz_synthetic_oms' THEN
    RAISE EXCEPTION 'synthetic OMS requires its isolated database';
  END IF;
END $$;

CREATE TABLE synthetic_oms_evidence (
  tenant_id text NOT NULL DEFAULT app_current_tenant(),
  principal_id text NOT NULL,
  portfolio_id text NOT NULL,
  order_id text NOT NULL,
  instrument_id text NOT NULL,
  command_sha256 bytea NOT NULL CHECK (octet_length(command_sha256)=32),
  kind text NOT NULL CHECK (kind IN ('entitlement','risk','compliance','mandate','market')),
  decision text NOT NULL CHECK (decision IN ('ALLOW','DENY','UNKNOWN')),
  source_id text NOT NULL CHECK (length(source_id) BETWEEN 1 AND 256),
  effective_at timestamptz NOT NULL,
  observed_at timestamptz NOT NULL,
  expires_at timestamptz NOT NULL,
  PRIMARY KEY (tenant_id,principal_id,portfolio_id,order_id,command_sha256,kind)
);

CREATE TABLE synthetic_oms_commands (
  tenant_id text NOT NULL DEFAULT app_current_tenant(),
  order_id text NOT NULL,
  principal_id text NOT NULL,
  portfolio_id text NOT NULL,
  idempotency_key text NOT NULL,
  command_sha256 bytea NOT NULL CHECK (octet_length(command_sha256)=32),
  command bytea NOT NULL CHECK (octet_length(command) BETWEEN 1 AND 65536),
  evidence_ids text[] NOT NULL CHECK (cardinality(evidence_ids)=5),
  phase text NOT NULL DEFAULT 'accepted' CHECK (phase IN
    ('accepted','dispatch_claimed','working','cancel_pending','unknown_execute','unknown_cancel','filled','cancelled')),
  recorded_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
  PRIMARY KEY(tenant_id,order_id),
  UNIQUE(tenant_id,idempotency_key)
);

CREATE TABLE synthetic_oms_observations (
  tenant_id text NOT NULL DEFAULT app_current_tenant(),
  order_id text NOT NULL,
  event_id text NOT NULL,
  operation text NOT NULL,
  observation bytea NOT NULL CHECK (octet_length(observation)<=65536),
  digest bytea NOT NULL CHECK (octet_length(digest)=32),
  effective_seconds bigint NOT NULL,
  effective_nanos integer NOT NULL CHECK(effective_nanos BETWEEN 0 AND 999999999),
  recorded_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
  PRIMARY KEY(tenant_id,order_id,event_id),
  FOREIGN KEY(tenant_id,order_id) REFERENCES synthetic_oms_commands(tenant_id,order_id)
);

DO $$
DECLARE name text;
BEGIN
  FOREACH name IN ARRAY ARRAY['synthetic_oms_evidence','synthetic_oms_commands','synthetic_oms_observations'] LOOP
    EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY',name);
    EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY',name);
    EXECUTE format('CREATE POLICY tenant_isolation ON %I USING (tenant_id=app_current_tenant()) WITH CHECK (tenant_id=app_current_tenant())',name);
  END LOOP;
END $$;
GRANT INSERT ON synthetic_oms_evidence TO kanz_synthetic_oms_control;
GRANT SELECT ON synthetic_oms_evidence,synthetic_oms_commands,synthetic_oms_observations
    TO kanz_synthetic_oms_runtime,kanz_synthetic_oms_adapter;
GRANT INSERT ON synthetic_oms_commands TO kanz_synthetic_oms_runtime;
GRANT UPDATE(phase) ON synthetic_oms_commands TO kanz_synthetic_oms_adapter;
GRANT INSERT ON synthetic_oms_observations TO kanz_synthetic_oms_adapter;
GRANT SELECT (tenant_id,order_id,version,state,status,portfolio_id,parent_order_id), INSERT (tenant_id,order_id,status,state,portfolio_id,parent_order_id)
    ON orders TO kanz_synthetic_oms_runtime,kanz_synthetic_oms_adapter;
GRANT UPDATE (status,state,portfolio_id,parent_order_id,version,updated_at)
    ON orders TO kanz_synthetic_oms_adapter;
GRANT SELECT (published_at,producer_service), INSERT
    (producer_service,envelope_tenant_id,partition_key,subject,event_type,event_class,schema_version,
     domain,payload_schema_ref,event_time,correlation_id,causation_id,trace_context,payload)
    ON outbox TO kanz_synthetic_oms_runtime,kanz_synthetic_oms_adapter;
GRANT USAGE ON SEQUENCE outbox_id_seq TO kanz_synthetic_oms_runtime,kanz_synthetic_oms_adapter;
GRANT INSERT (tenant_id,order_id,fill_id,raw_fill_id), SELECT (tenant_id,fill_id)
    ON order_fills TO kanz_synthetic_oms_adapter;
GRANT INSERT (order_id,fill_id,venue,venue_account_id,instrument_id,venue_execution_id,fill),
    SELECT (order_id,fill_id,venue,venue_account_id,instrument_id,venue_execution_id,fill)
    ON order_executions TO kanz_synthetic_oms_adapter;
