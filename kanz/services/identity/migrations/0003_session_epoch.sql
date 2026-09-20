-- A snapshot read before disable can mint after it: issuance time cannot fence it.
-- Keep the generation on the account snapshot and advance it atomically on disable.
ALTER TABLE identity_users ADD COLUMN IF NOT EXISTS session_epoch BIGINT NOT NULL DEFAULT 0
    CHECK (session_epoch >= 0);
-- Previously revoked subjects must refuse all legacy generation-zero sessions.
UPDATE identity_users SET session_epoch = 1
 WHERE tokens_invalid_before IS NOT NULL AND session_epoch = 0;
