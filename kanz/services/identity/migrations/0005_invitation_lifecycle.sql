-- Deploy with old identity writers stopped: their redemption predicate does
-- not know revoked_at. Resume traffic only on the lifecycle-aware binary.
ALTER TABLE identity_invites ADD COLUMN IF NOT EXISTS revoked_at TIMESTAMPTZ CHECK (revoked_at IS NULL OR redeemed_at IS NULL);
ALTER TABLE identity_invites ADD COLUMN IF NOT EXISTS revoked_by TEXT NOT NULL DEFAULT '' CHECK ((revoked_at IS NULL AND revoked_by='') OR (revoked_at IS NOT NULL AND revoked_by<>''));
ALTER TABLE identity_invites ADD COLUMN IF NOT EXISTS revision BIGINT NOT NULL DEFAULT 0 CHECK (revision >= 0);
ALTER TABLE identity_invites ADD COLUMN IF NOT EXISTS reissued_as TEXT REFERENCES identity_invites(id) DEFERRABLE INITIALLY DEFERRED CHECK (reissued_as IS NULL OR (revoked_at IS NOT NULL AND reissued_as<>id));

-- Build the replacement before dropping the old index. Expired invitations
-- retain their slot until an explicit, audited revoke or reissue releases it.
CREATE UNIQUE INDEX IF NOT EXISTS identity_invites_one_unrevoked_per_subject
    ON identity_invites(subject) WHERE redeemed_at IS NULL AND revoked_at IS NULL;
DROP INDEX IF EXISTS identity_invites_one_live_per_subject;
