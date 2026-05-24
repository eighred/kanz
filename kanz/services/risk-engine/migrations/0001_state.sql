-- 0001: durable risk-engine state (PERS-01).
--
-- One snapshot row per portfolio plus its positions and a bounded tail of
-- applied idempotency keys. A restarted engine loads the latest row set,
-- rehydrates in-memory state, and resumes the Kafka log of record at
-- log_offset + 1 — gap-free, RPO=0 (see internal/risk/state/persist).
--
-- Money / Decimal / LogPosition value types are stored as marshaled
-- common.v1 proto bytes (BYTEA), the same opaque-descriptor pattern the
-- schema registry uses (EVT-16a). Reason: these are exact base-10 decimals
-- and `double` is banned for money/sizes/prices (KANZ_BRAIN), so there is
-- no lossless native SQL column for them; the engine is the only reader,
-- the bytes are its contract with itself.

CREATE TABLE portfolios (
    portfolio_id       TEXT        NOT NULL PRIMARY KEY,
    display_name       TEXT        NOT NULL DEFAULT '',
    base_currency      TEXT        NOT NULL DEFAULT '',
    cash_balance       BYTEA,                      -- common.v1.Money,        nullable pre-state
    total_market_value BYTEA,                      -- common.v1.Money,        nullable pre-state
    position_count     BIGINT      NOT NULL DEFAULT 0,
    as_of              TIMESTAMPTZ,                -- newest applied event time; null before any state
    -- LogPosition (common.v1) the snapshot includes up to; resume at offset + 1.
    -- All three null together before the first snapshot.
    log_topic          TEXT,
    log_partition      BIGINT,
    log_offset         BIGINT,
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE positions (
    portfolio_id             TEXT        NOT NULL REFERENCES portfolios (portfolio_id) ON DELETE CASCADE,
    instrument_id            TEXT        NOT NULL,
    quantity                 BYTEA,             -- common.v1.Decimal
    average_price            BYTEA,             -- common.v1.Decimal, nullable when flat
    market_value             BYTEA,             -- common.v1.Money
    market_value_uncertainty BYTEA,             -- common.v1.Money,   nullable (RISK-08)
    realized_pnl             BYTEA,             -- common.v1.Money
    unrealized_pnl           BYTEA,             -- common.v1.Money
    as_of                    TIMESTAMPTZ,
    PRIMARY KEY (portfolio_id, instrument_id)
);

-- Durable analog of the per-portfolio in-memory dedup window (RISK-05).
-- Bounded per portfolio by the writer (PERS-01b prunes by applied_at);
-- the bootstrap path pre-seeds the in-memory window from these so a
-- replayed boundary event is recognized as already-applied.
CREATE TABLE applied_keys (
    portfolio_id    TEXT        NOT NULL REFERENCES portfolios (portfolio_id) ON DELETE CASCADE,
    idempotency_key TEXT        NOT NULL,
    applied_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (portfolio_id, idempotency_key)
);

-- Supports the writer's age-based pruning of the applied-keys tail.
CREATE INDEX applied_keys_portfolio_applied_at_idx
    ON applied_keys (portfolio_id, applied_at);
