-- Recovery is an identity lookup before authentication, using the same global
-- pool boundary as identity_users. Public responses never project these rows.
CREATE TABLE IF NOT EXISTS identity_mailboxes (
    subject TEXT PRIMARY KEY REFERENCES identity_users(subject),
    address TEXT NOT NULL,
    verified_at TIMESTAMPTZ NOT NULL
);

-- One bounded slot per account and purpose. Tokens are generated in memory by
-- a delivery attempt; only their digest is durable. A retry replaces the digest.
CREATE TABLE IF NOT EXISTS identity_mail_challenges (
    subject TEXT NOT NULL REFERENCES identity_users(subject),
    purpose TEXT NOT NULL CHECK (purpose IN ('verify', 'recover')),
    request_id TEXT NOT NULL UNIQUE,
    address TEXT NOT NULL,
    session_epoch BIGINT NOT NULL CHECK (session_epoch >= 0),
    requested_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    token_hash TEXT UNIQUE,
    state TEXT NOT NULL CHECK (state IN ('pending','sending','sent','failed','consumed')),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 3),
    lease_until TIMESTAMPTZ,
    PRIMARY KEY(subject,purpose)
);
CREATE INDEX IF NOT EXISTS identity_mail_pending ON identity_mail_challenges(state,lease_until)
    WHERE state IN ('pending','sending');
