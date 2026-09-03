-- 0002: boot cost was proportional to ALL history (#809).
--
-- 0001 made tv-sync's book survive a restart by replaying every FACT it had ever
-- folded. That was the right fix for EXEC-M21 — a pod roll used to leave a trader
-- looking at an empty account while real positions sat open at the exchanges — and
-- it has one consequence nobody bounded: a booting pod unmarshals and folds N facts
-- before it can report ready, so startup time grows with the fund's entire history
-- and the outage after each OOM kill is longer than the one before it.
--
-- # Why this is a checkpoint and not a retention window
--
-- #809 proposed bounding the replay to a recent window. That re-creates EXEC-M21's
-- defect with a shorter horizon: a windowed rebuild loses every position opened
-- before the window and reports realized P&L since the window start rather than
-- since inception. On the surface a trader acts from, that is a wrong number, not a
-- saving. Retention on tv_facts is the same problem one layer down — it destroys
-- the only durable record of what was folded, so the pre-retention view becomes
-- unrebuildable.
--
-- A checkpoint bounds the boot WITHOUT losing anything: `checkpoint + tail` folds to
-- exactly the same view as `all facts`, so the horizon is unchanged and only the
-- work is smaller.
--
-- # Why this is not the SECOND BOOK 0001 argues against
--
-- 0001 is right that a table of orders, executions and positions would make tv-sync
-- a second book of record — one that could disagree with the OMS's, with nobody able
-- to say which was right. The distinction is authority, not shape:
--
--   * tv_facts and this table both hold DERIVED data. tv_facts is the input tv-sync
--     folded; this is the state that fold reached.
--   * Nothing reads this table except tv-sync's own boot. It answers no query and
--     originates nothing.
--   * It is DISCARDABLE. Truncate it and the service still reaches the identical
--     view by replaying tv_facts from the beginning — it just takes longer. A book
--     of record is precisely the thing you cannot truncate.
--
-- The house determinism contract is unchanged and is what makes this safe: a full
-- re-fold of the FACT log reproduces the view exactly, and the checkpoint is only
-- ever an optimisation of a computation whose answer is defined elsewhere.

CREATE TABLE tv_checkpoints (
    tenant_id  TEXT        NOT NULL DEFAULT app_current_tenant(),
    -- seq is the LAST tv_facts.seq folded into this checkpoint. Replay resumes
    -- strictly after it.
    --
    -- ONE SEQ FOR THE WHOLE TENANT, not one per account. Facts arrive interleaved
    -- across accounts and the fold is order-dependent, so a per-account watermark
    -- would replay a different tail for each and assemble a view that never
    -- existed at any instant.
    seq        BIGINT      NOT NULL,
    -- The serialized tvsync.v1.Checkpoint. A single blob rather than columns per
    -- field, deliberately: columns would be a schema for a SECOND BOOK, queryable
    -- and joinable and therefore eventually joined. This is opaque to SQL, which
    -- is what keeps it a private checkpoint of a fold rather than a model of the
    -- fund.
    payload    BYTEA       NOT NULL,
    taken_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- ONE ROW PER TENANT, replaced on each checkpoint. Keeping a history of
    -- checkpoints would be a retention decision of its own, and the older ones
    -- answer no question the fact log cannot: the log IS the history, and this is
    -- only a shortcut to its end.
    PRIMARY KEY (tenant_id)
);

-- Tenant isolation (MT-01d/e), identical in shape to tv_facts. app_current_tenant()
-- RAISES when the GUC is unset, so an unscoped session gets an ERROR rather than a
-- silently absent checkpoint — and a silently absent checkpoint would send the pod
-- back to a full replay without saying why, which reads as "the fix did nothing"
-- rather than as a misconfiguration.
ALTER TABLE tv_checkpoints ENABLE ROW LEVEL SECURITY;
ALTER TABLE tv_checkpoints FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON tv_checkpoints
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());
