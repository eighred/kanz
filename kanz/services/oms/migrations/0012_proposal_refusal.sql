-- 0012: a refused approval that told the approver nothing (#558).
--
-- # What was silent
--
-- An approval arrives on the bus. handleApprove checks it against the stored
-- proposal, and on every refusal — self-approval, a respelling of the proposer,
-- a digest covering other terms, an expired proposal — it logged a WARN and
-- acked. The gateway had already answered 202 at publish time, no FACT went out,
-- and no CommandOutcome could: all four CommandOutcomeStatus values are TERMINAL
-- and a refused proposal is not, because the order is still pending and somebody
-- else may legitimately sign it. So the approver's signature was rejected by a
-- control and the only record of it lived in a log line they cannot read.
--
-- This is the third time this platform has paid for the same shape. #410 held an
-- order and announced it (ORDER_PENDING_APPROVAL). #539/#547 found the expiry
-- half silent and announced that (ORDER_REJECTED, terminal, so a FACT worked).
-- The refusal half is the branch neither covers, and a FACT does not work for it.
--
-- # Why this is three columns and not a new subject or a new outcome status
--
-- #558 enumerated four options. Two of them predate two facts:
--
--  1. #557 shipped GET /v1/orders/pending-approvals — gated on authz.Approve,
--     mounted, reachable. When #558 was filed, "poll the queue" was dismissed as
--     dishonest because no client could see the queue. That is no longer true,
--     and the queue is now the surface the approver already uses.
--
--  2. #563/#564 settled the sibling act. An override proposal that lapsed used
--     to vanish; the repair was NOT a new FACT, because datamaster publishes one
--     subject that only the audit projector consumes, so a new subject would have
--     been delivered to nobody. The lapsed proposal is LISTED on the surface the
--     proposer already reads, carrying a `state`. That is the shape mirrored
--     here.
--
-- A new COMMAND_OUTCOME_STATUS_REFUSED would be a command.v1 change every
-- consumer folds and a migration of meaning for anyone who assumed four values
-- were exhaustive — to say something only the approver needs, on the one surface
-- the approver is already looking at.
--
-- # Why the refusal is recorded on the proposal rather than in its own table
--
-- A refusal is not an event with a life of its own; it is a property of the
-- proposal it was refused against, read only when that proposal is listed. A
-- refusals table would be a second row per attempt with no reader, and a join on
-- the one query an approver's screen calls.
--
-- ONLY THE LATEST REFUSAL IS KEPT, and that is deliberate rather than a
-- limitation waiting to be lifted. The question this answers is "why is my
-- signature not working", which is about the last attempt. A history of every
-- attempt is an audit question, and the audit answer is the WARN in the log plus
-- the FACTs — not an unbounded column on a hot queue read.
--
-- # These columns MUST NOT decide the proposal
--
-- The row stays approver = '' and decided_at NULL. That is what keeps
-- TestSelfApprovalIsRefusedAndTheProposalStaysPending true: recording a refusal
-- must not consume the proposal, or one person could destroy a colleague's
-- pending decision by attempting their own approval and being turned away. The
-- columns are additive to a PENDING row and nothing else reads them to decide
-- anything.
--
-- No index. Every read of these columns is by (tenant_id, order_id) on Get or as
-- payload on the already-indexed open-proposal scan; an index on a column nothing
-- filters by is write cost with no reader.
ALTER TABLE order_proposals
    -- The refusal label from services/oms/internal/order.approvalRefusal — the
    -- closed set internal/dualcontrol's sentinel errors define. '' means no
    -- attempt has been refused. STORED AS THE CODE, not the message: the
    -- approver's client branches on it, and a sentence would drift between the
    -- log, this row and the queue reply.
    ADD COLUMN IF NOT EXISTS refusal_reason TEXT NOT NULL DEFAULT '',
    -- The AUTHENTICATED subject whose signature was refused. It is here so a
    -- returning approver can tell "mine was refused" from "somebody else's was";
    -- without it the queue can say a refusal happened and not to whom, which is
    -- the same "something is wrong somewhere" the WARN already was.
    ADD COLUMN IF NOT EXISTS refused_by TEXT NOT NULL DEFAULT '',
    -- When. NULL means never refused. A timestamp rather than a boolean for the
    -- reason 0010 gives for expiry_announced_at: an approver looking at a
    -- refusal needs to know whether it happened before or after the terms they
    -- are reading were proposed.
    ADD COLUMN IF NOT EXISTS refused_at TIMESTAMPTZ;

-- THE THREE MOVE TOGETHER OR NOT AT ALL. A row carrying a reason with no time,
-- or a time with no reason, would render on the approver's queue as a refusal it
-- cannot describe — the "nothing configured and checked-and-fine look the same"
-- failure, arriving as a half-written row. The engine is the last place that can
-- still refuse it, exactly as 0009's CHECK is the last place that can refuse a
-- forged self-approval.
ALTER TABLE order_proposals
    DROP CONSTRAINT IF EXISTS order_proposals_refusal_is_whole;
ALTER TABLE order_proposals
    ADD CONSTRAINT order_proposals_refusal_is_whole
    CHECK ((refusal_reason = '' AND refused_by = '' AND refused_at IS NULL)
        OR (refusal_reason <> '' AND refused_by <> '' AND refused_at IS NOT NULL));
