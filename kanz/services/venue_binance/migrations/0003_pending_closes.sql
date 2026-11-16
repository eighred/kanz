-- Pending cancellation ownership survives adapter restarts and redelivery.
CREATE TABLE venue_pending_closes (
 tenant_id TEXT NOT NULL DEFAULT app_current_tenant(),
 order_id TEXT NOT NULL,
 instrument_id TEXT NOT NULL,
 requested_at TIMESTAMPTZ NOT NULL,
 next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT '-infinity',
 attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 7),
 intent BYTEA NOT NULL,
 PRIMARY KEY (tenant_id, order_id)
);
CREATE INDEX venue_pending_closes_due ON venue_pending_closes (tenant_id, next_attempt_at, requested_at);
ALTER TABLE venue_pending_closes ENABLE ROW LEVEL SECURITY;
ALTER TABLE venue_pending_closes FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON venue_pending_closes
 USING (tenant_id = app_current_tenant()) WITH CHECK (tenant_id = app_current_tenant());
