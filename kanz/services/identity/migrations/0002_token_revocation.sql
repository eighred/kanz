-- #532 — disabling an account revokes the token it ALREADY holds.
--
-- Until this column existed, SetStatus stopped the NEXT login and nothing more.
-- The gateway verifies a signature against JWKS and reads no account state at
-- all, so an offboarded trader's outstanding token kept working for its full
-- remaining lifetime. services/identity/../postgres.go said so in past tense and
-- named this column as the fix; this is that column.
--
-- IT IS A HIGH-WATER MARK, NOT A FLAG, and that is the difference between this
-- and simply publishing `status = 'disabled'`. A flag is cleared on re-enable,
-- which RESURRECTS the exact token the disable was meant to kill. A timestamp
-- that only moves forward keeps that token dead for good, while the account's
-- next login mints one that is trivially newer and is admitted with no operator
-- action at all.
--
-- NULL MEANS "NEVER REVOKED", and it is the right absence here: an account that
-- has never been disabled has no revocation instant, and inventing one (epoch,
-- or created_at) would put a row in the feed for every account on the platform —
-- a denylist the size of the user table, refreshed by every gateway pod.
ALTER TABLE identity_users
    ADD COLUMN IF NOT EXISTS tokens_invalid_before TIMESTAMPTZ;

-- The feed reads exactly the marked rows, and on a platform where disabling is
-- rare that is a small fraction of the table. Partial, so the index costs
-- nothing for the accounts nobody has ever disabled.
CREATE INDEX IF NOT EXISTS identity_users_revoked_idx
    ON identity_users (tokens_invalid_before)
    WHERE tokens_invalid_before IS NOT NULL;

-- EXISTING DISABLED ACCOUNTS ARE BACKFILLED, and leaving them out would have
-- been the quiet failure this whole change exists to remove: an account
-- disabled before this migration would carry status = 'disabled' with no mark,
-- so the gateway would go on honouring its token while every surface an operator
-- can see reports the account as locked out. now() rather than updated_at
-- because updated_at also moves on a credential rehash, so it is not evidence of
-- when the disable happened — and a mark that is too RECENT only fails safe, by
-- refusing tokens minted in the gap as well.
UPDATE identity_users
   SET tokens_invalid_before = now()
 WHERE status = 'disabled'
   AND tokens_invalid_before IS NULL;
