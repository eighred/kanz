-- Identity's global pre-authentication lookup boundary also owns WebAuthn.
-- No private authenticator key is ever stored on the relying party.
ALTER TABLE identity_users ADD COLUMN IF NOT EXISTS mfa_enabled BOOLEAN NOT NULL DEFAULT FALSE;
CREATE TABLE IF NOT EXISTS identity_mfa_credentials (
 credential_id TEXT PRIMARY KEY,
 subject TEXT NOT NULL REFERENCES identity_users(subject),
 name TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 80),
 credential JSONB NOT NULL,
 created_at TIMESTAMPTZ NOT NULL,
 last_used_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS identity_mfa_subject ON identity_mfa_credentials(subject);
-- Fixed slots bound storage per account. A new ceremony supersedes its old
-- purpose slot; consumption and public-key/counter/audit mutations commit together.
CREATE TABLE IF NOT EXISTS identity_mfa_ceremonies (
 subject TEXT NOT NULL REFERENCES identity_users(subject),
 purpose TEXT NOT NULL CHECK (purpose IN ('register','login','stepup')),
 token_hash TEXT NOT NULL UNIQUE,
 session_epoch BIGINT NOT NULL CHECK (session_epoch >= 0),
 session_data JSONB NOT NULL,
 name TEXT NOT NULL,
 expires_at TIMESTAMPTZ NOT NULL,
 consumed BOOLEAN NOT NULL DEFAULT FALSE,
 PRIMARY KEY(subject,purpose)
);
