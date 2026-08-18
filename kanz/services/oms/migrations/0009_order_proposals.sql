-- 0009: an order that needs two people, held until the second one signs (#410).
--
-- THIS TABLE IS WHERE AN UNAPPROVED ORDER LIVES, AND ITS EXISTENCE IS WHAT LETS
-- OMS_REQUIRE_DUAL_CONTROL BE ARMED AT ALL. Until it existed, config.Load refused
-- the flag outright: arming with nowhere to put a held order would have REJECTED
-- every order at or above OMS_DUAL_CONTROL_MIN_NOTIONAL instead of holding it —
-- an act that neither takes effect nor can be approved, which is exactly the
-- silent drop #410's acceptance forbids.
--
-- # Why it is a table and not a ninth OrderStatus
--
-- A PENDING_APPROVAL status would put an unapproved order in `orders`, which is
-- the book: the routing gate at internal/order/service.go's work(), the startup
-- sweep, the position projector, tv-sync and kanz-web all read that table and
-- fold status. Miss any one of them and an order nobody approved reaches a live
-- exchange. test/arch/order_status_exhaustive_test.go pins ONE status by a
-- hardcoded string, so a ninth value passes that guard green and uncovered, and
-- kanz-web sits outside every arch guard in this repository (it is still missing
-- ORDER_STATUS_WORKING_SCHEDULED from #435). A separate table is refused by
-- default instead: a reader that does not know about it cannot mistake a
-- proposal for an order.
--
-- # Why it is a table and not a map in memory
--
-- The shipped OMS runs replicas: 2. A proposal made on pod A must be approvable
-- on pod B, or the second signature succeeds or fails depending on which pod the
-- approver's request reached — intermittent, and indistinguishable from a bug in
-- the approver's client. A restart would drop every pending order silently.
-- Same argument datamaster's 0004 makes for exception_override_proposals.
--
-- # The command is STORED, not just its digest
--
-- The digest proves the approver signed this exact order; ADMITTING it needs the
-- order itself. Asking the approver's client to resend the SubmitOrder would make
-- "the approver supplies the payload" the normal path — the digest would catch a
-- client that sent something else, but a control whose safety depends on a check
-- that fires on the happy path is one refactor away from being ceremonial
-- (datamaster/internal/store/proposals.go states the same rule).
--
-- Stored as marshaled order.v1.SubmitOrder BYTEA, the same opaque-descriptor
-- stance orders.state and outbox.payload take: the command carries exact base-10
-- common.v1.Decimal quantities and prices, `double` is banned on any path that
-- moves capital, and there is no lossless native SQL column for one.
--
-- # THE PRIMARY KEY IS THE ORDER ID, AND THERE IS NO SEPARATE proposal_id
--
-- This deviates from datamaster's shape deliberately. An exception can be
-- overridden more than once, so its proposals need identities of their own; an
-- order is ONE decision, and a second proposal for the same order_id is never a
-- second decision — it is a redelivery of the same SubmitOrder. Making the order
-- id the key means the engine refuses the duplicate, exactly as the `orders`
-- primary key refuses a duplicate admission, so a redelivered command cannot
-- produce a second pending order or a second announcement of it. A random
-- proposal id would have made that an application-level uniqueness check on a
-- path where the OMS has already paid once for check-then-act.
CREATE TABLE order_proposals (
    tenant_id    TEXT        NOT NULL DEFAULT app_current_tenant(),
    order_id     TEXT        NOT NULL,
    -- Denormalized out of the command blob for the same reason orders.portfolio_id
    -- is (0007): no SQL can decode a protobuf, and "whose pending orders are
    -- these" is a question an operator must be able to ask without one.
    portfolio_id TEXT        NOT NULL,
    -- dualcontrol.Act. Stored rather than assumed constant so an approval
    -- collected for one act can never be replayed against another, and so a
    -- fourth act added later does not silently inherit this table's rows.
    act          TEXT        NOT NULL,
    -- The AUTHENTICATED subject that submitted the order — never self-asserted.
    -- The forged-actor defect #444 fixed on the override path arrived exactly
    -- here, from a field a caller could choose.
    proposer     TEXT        NOT NULL,
    -- approval.Terms.Digest over the command below. Re-derived and re-checked at
    -- approval time, so an order edited between proposal and approval cannot
    -- inherit the signature.
    digest       TEXT        NOT NULL,
    command      BYTEA       NOT NULL,   -- marshaled order.v1.SubmitOrder (authoritative)
    -- The SECOND signature. '' means still pending; a decided proposal keeps its
    -- row, and that is the difference from datamaster's proposals table.
    --
    -- THERE, A CLAIMED PROPOSAL IS DELETED, because the durable record of what
    -- happened is the append-only exception_overrides row carrying both names,
    -- and a second copy here could disagree with it. THE OMS HAS NO SUCH TRAIL:
    -- the admitted order records the order, not who approved it, and the
    -- ORDER_ACCEPTED FACT carries an OrderState with no approver field. So this
    -- row IS the durable evidence that two people signed, and deleting it would
    -- leave the audit question "who approved this order" answerable only from a
    -- bus FACT that services/audit may or may not have folded.
    approver     TEXT        NOT NULL DEFAULT '',
    -- When the second signature was given. NULL while pending.
    decided_at   TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL,
    expires_at   TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, order_id),

    -- A SELF-APPROVAL IS NOT DUAL CONTROL, AND THE DATABASE IS THE LAST PLACE
    -- THAT CAN STILL SAY SO.
    --
    -- internal/dualcontrol refuses it, and that refusal is well tested — but it
    -- is a refusal in ONE process, and this row is the audit evidence. A row
    -- where proposer = approver reads to an auditor as four-eyes while one
    -- person held both signatures, which is the single most valuable row to
    -- forge and the one an auditor is least able to check. The same constraint
    -- datamaster/0004 puts on exception_overrides, on the same argument.
    --
    -- Case and surrounding space are folded because "Alice@kanz" approving
    -- "alice@kanz" is one person and a byte comparison would call it two — the
    -- clause #495 recorded as the one that fails quietly, because the trail then
    -- shows two distinct actors.
    --
    -- WHAT IT DOES NOT CATCH is a homoglyph ("alice" with a Cyrillic 'a').
    -- Deciding two subjects denote one person is the identity provider's job
    -- (#364); both sides here are subjects the gateway authenticated, so that
    -- attack needs the IdP to have issued the look-alike credential.
    CONSTRAINT order_proposals_approver_is_not_proposer
        CHECK (approver = '' OR lower(btrim(approver)) <> lower(btrim(proposer))),

    -- A proposal born expired can never be approved and would sit in the pending
    -- list forever looking like work nobody did.
    CONSTRAINT order_proposals_expires_after_creation
        CHECK (expires_at > created_at),

    -- An empty proposer makes the self-approval check VACUOUS: every approver
    -- differs from "", so the one rule this table exists for would pass for
    -- anyone. dualcontrol.Propose refuses it too; this is the second line.
    CONSTRAINT order_proposals_proposer_present
        CHECK (btrim(proposer) <> ''),

    -- "Approved" and "when" must move together. A row with an approver and no
    -- timestamp cannot be aged or ordered in a trail; a row with a timestamp and
    -- no approver claims a decision nobody made. Either both or neither.
    CONSTRAINT order_proposals_decided_together
        CHECK ((approver = '') = (decided_at IS NULL))
);

-- The pending list's only read: what is still awaiting a signature, oldest
-- first. PARTIAL on approver = '' so the index holds the QUEUE and not the
-- history — a decided proposal leaves the index, so listing what needs
-- attention stays the same cost on day 1000 as on day 1.
--
-- THE COST OF THAT CHOICE IS THAT THE QUERY MUST CARRY THE PREDICATE TOO.
-- Postgres uses a partial index only where it can prove the predicate holds, so
-- ProposalStore.Pending spells out `approver = ''`; that clause is load-bearing
-- and removing it as redundant turns every pending-list read into a sequential
-- scan. Same rule 0008's orders_parent_idx documents.
CREATE INDEX order_proposals_pending_idx
    ON order_proposals (tenant_id, created_at, order_id) WHERE approver = '';

-- Tenant isolation (MT-01d/e), via app_current_tenant() (0002) rather than the
-- raw current_setting, so an UNSCOPED read RAISES instead of returning zero
-- rows. It is load-bearing in the way outbox's is: these rows hold whole
-- SubmitOrder commands — another tenant's positions, sizes and limit prices —
-- and, worse, a proposal readable across tenants is a proposal APPROVABLE across
-- tenants. The app role must be NOSUPERUSER or FORCE RLS does not apply.
ALTER TABLE order_proposals ENABLE ROW LEVEL SECURITY;
ALTER TABLE order_proposals FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON order_proposals
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());
