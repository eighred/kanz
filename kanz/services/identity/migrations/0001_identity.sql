-- The platform's own identity store (#364).
--
-- NO ROW-LEVEL SECURITY ON THESE TWO TABLES, DELIBERATELY, and the reason is
-- structural rather than an omission: login must find an account BEFORE it knows
-- which tenant that account belongs to. A tenant-scoped pool
-- (internal/pg.NewTenantPool) binds `app.tenant_id` once at connect, so a
-- credential lookup through one could only ever find users of whichever tenant
-- the pool was opened for — which is every tenant except the one signing in.
--
-- The control is internal/pg.NewGlobalPool, which REFUSES to open without a
-- written reason for the missing scope. That is the same seam price_observations
-- uses, and it is why an unscoped store here is distinguishable from a service
-- that forgot to scope itself.
--
-- Tenancy is still carried and still enforced — on the TOKEN. identity_users.
-- tenant_id becomes the principal's tenant claim, and every downstream store is
-- RLS-scoped by it. The isolation boundary is not weakened; it starts one step
-- later, at the point the caller is known.

CREATE TABLE IF NOT EXISTS identity_users (
    subject         TEXT PRIMARY KEY,
    tenant_id       TEXT        NOT NULL,
    roles           TEXT[]      NOT NULL,
    portfolios      TEXT[]      NOT NULL DEFAULT '{}',
    -- The Argon2id PHC string. NEVER a plaintext credential, and never returned
    -- to any caller — the only read is by Verify inside the login path.
    credential_hash TEXT        NOT NULL,
    status          TEXT        NOT NULL DEFAULT 'active',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT identity_users_status_known CHECK (status IN ('active', 'disabled')),
    CONSTRAINT identity_users_tenant_present CHECK (tenant_id <> ''),
    -- An account with no role authenticates and can do nothing, which reads as a
    -- broken login rather than the misconfiguration it is.
    CONSTRAINT identity_users_has_a_role CHECK (cardinality(roles) > 0)
);

CREATE TABLE IF NOT EXISTS identity_invites (
    id          TEXT PRIMARY KEY,
    -- SHA-256 of the raw token, never the token itself: a stolen database must
    -- not yield usable invites, each of which carries whatever authority an
    -- operator attached to it. UNIQUE because redemption looks up BY this value.
    token_hash  TEXT        NOT NULL UNIQUE,
    subject     TEXT        NOT NULL,
    tenant_id   TEXT        NOT NULL,
    roles       TEXT[]      NOT NULL,
    portfolios  TEXT[]      NOT NULL DEFAULT '{}',
    -- Who granted this authority. The first question after an incident.
    created_by  TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL,
    expires_at  TIMESTAMPTZ NOT NULL,
    -- NULL until redeemed. The single-use guarantee is the partial index below
    -- plus a conditional UPDATE, not a read-then-write in application code.
    redeemed_at TIMESTAMPTZ,
    CONSTRAINT identity_invites_has_a_role CHECK (cardinality(roles) > 0),
    CONSTRAINT identity_invites_expires_after_creation CHECK (expires_at > created_at)
);

-- AT MOST ONE UNREDEEMED INVITE PER SUBJECT. Two live invites for one person are
-- two different authorities racing to become their account, and whichever is
-- redeemed second silently loses — including the case where an operator issued a
-- corrected invite and the stale one is used instead.
CREATE UNIQUE INDEX IF NOT EXISTS identity_invites_one_live_per_subject
    ON identity_invites (subject) WHERE redeemed_at IS NULL;
