-- #410 — maker-checker: an override records the SECOND person who approved it.
--
-- Dual control ships unarmed (DATAMASTER_REQUIRE_DUAL_CONTROL, default false),
-- so most rows in this trail are single-signed and will stay that way. That is
-- precisely why the column exists rather than being inferred: whether a second
-- person signed a given override is a fact about that override, and reading it
-- back from today's configuration would answer for the wrong day.
--
-- DEFAULT '' RATHER THAN NULL. Every existing row was written before dual
-- control existed, and '' says "one person signed this" — which is true of them.
-- A NULL would mean "unknown", and this is the one question about these rows
-- that is not unknown. NOT NULL keeps the read path from having to decide what a
-- missing approver means, a decision that would land in three places and diverge.
ALTER TABLE exception_overrides
    ADD COLUMN approver TEXT NOT NULL DEFAULT '';

-- A SELF-APPROVAL IS NOT DUAL CONTROL, and the database is the last place that
-- can still say so. The handler refuses it and pricing.Override.Validate refuses
-- it, but this trail is the audit evidence: a row where actor = approver would
-- read to an auditor as four-eyes while one person held both signatures. Case
-- and surrounding space are folded because "Alice@kanz" approving "alice@kanz"
-- is one person, and a byte comparison would call it two.
ALTER TABLE exception_overrides
    ADD CONSTRAINT exception_overrides_approver_is_not_actor
    CHECK (approver = '' OR lower(btrim(approver)) <> lower(btrim(actor)));

-- The pending half of maker-checker: an override that has been PROPOSED and is
-- waiting for a second signature.
--
-- IT IS A TABLE AND NOT A MAP IN MEMORY, for two reasons that are not about
-- durability. This service runs more than one replica, so a proposal made on pod
-- A must be approvable on pod B — an in-process map makes the second signature
-- depend on which pod the approver's request happened to reach, which fails
-- intermittently and looks like a bug in the approver's client. And a restart
-- would drop pending proposals silently, which is the failure mode #410's
-- "verified when" names explicitly: an act that neither takes effect nor reports
-- why.
--
-- THE PAYLOAD IS STORED, NOT JUST ITS DIGEST. The digest proves the approver
-- signed this exact value; applying it needs the value itself, and asking the
-- approver's client to resend it would let a client apply something other than
-- what was approved — the digest would catch it, but only after making that the
-- normal path.
CREATE TABLE exception_override_proposals (
    tenant_id    TEXT        NOT NULL DEFAULT app_current_tenant(),
    proposal_id  TEXT        NOT NULL,
    exception_id TEXT        NOT NULL,
    act          TEXT        NOT NULL,
    proposer     TEXT        NOT NULL,
    -- The dualcontrol.Digest of the payload below. Re-checked at approval, so a
    -- payload edited between proposal and approval cannot inherit the signature.
    digest       TEXT        NOT NULL,
    reason       TEXT        NOT NULL,
    chosen_price TEXT        NOT NULL,       -- exact decimal string, never a double
    created_at   TIMESTAMPTZ NOT NULL,
    expires_at   TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, proposal_id),
    FOREIGN KEY (tenant_id, exception_id) REFERENCES exceptions (tenant_id, exception_id) ON DELETE CASCADE,
    -- A proposal born expired can never be approved and would sit in the pending
    -- list forever looking like work nobody did.
    CONSTRAINT exception_override_proposals_expires_after_creation CHECK (expires_at > created_at),
    CONSTRAINT exception_override_proposals_proposer_present CHECK (btrim(proposer) <> '')
);

CREATE INDEX exception_override_proposals_pending_idx
    ON exception_override_proposals (tenant_id, expires_at);

-- Same deny-by-default tenant isolation as every other table here, via
-- app_current_tenant() (0003) rather than the raw current_setting.
ALTER TABLE exception_override_proposals ENABLE ROW LEVEL SECURITY;
ALTER TABLE exception_override_proposals FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON exception_override_proposals
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());
