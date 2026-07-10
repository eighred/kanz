-- 0001: durable AUDIT-01 hash-chain links (REG-02).
--
-- The regulatory filing signer (internal/audit/signer.ChainSigner) folds each
-- filing's signature into the previous one, so a signature is a position in a
-- tamper-evident chain. That guarantee only holds if the chain persists: with
-- links in memory only, a restart resets the head to Genesis and the earlier
-- links vanish, breaking continuity across the restart. This table is the
-- durable append-only sequence of links; the service recovers the head from it
-- on startup so one unbroken, walk-verifiable chain spans the deployment.
--
-- # Not tenant-scoped
--
-- Unlike the IBOR ledger, the audit chain is a single per-deployment sequence:
-- the regulatory service is a stateless filing assembler with no principal, so
-- there is no tenant to scope by. The table therefore carries no tenant_id and
-- no row-level security (the RISK-12 universal-fact rationale). A deployment
-- that ever needs per-tenant chains adds a tenant column + RLS in a later
-- migration; today one chain per service is the contract.
--
-- # Content
--
-- Each row is one signer.Link: the predecessor hash it chained from, its own
-- hash (== the filing signature, unique — a chain position), and the exact
-- canonical bytes that were signed (so the chain is independently verifiable).

CREATE TABLE audit_chain_links (
    seq        BIGINT      GENERATED ALWAYS AS IDENTITY,  -- append order (chain order)
    prev_hash  TEXT        NOT NULL,                       -- predecessor hash (chain.Genesis at the root)
    hash       TEXT        NOT NULL,                       -- this link's hash == the filing signature
    canonical  BYTEA       NOT NULL,                       -- exact signed bytes, for independent verification
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (hash)                                     -- idempotent append; a hash is a unique chain position
);

-- Head recovery and ordered verification both read by descending/ascending seq.
CREATE INDEX audit_chain_links_seq_idx ON audit_chain_links (seq);
