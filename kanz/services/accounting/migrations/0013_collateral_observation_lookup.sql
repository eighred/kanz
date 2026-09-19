-- Agreement IDs are tenant-wide: detecting ambiguous observations must not
-- depend on which portfolio the source used to batch an agreement.
CREATE INDEX IF NOT EXISTS collateral_observation_time
    ON collateral_snapshots(tenant_id,as_of);
