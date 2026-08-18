-- ONE INDEX FOR THE OPEN PROPOSALS, BECAUSE THERE WERE TWO AND ONE GREW FOREVER (#548).
--
-- # What was wrong
--
-- 0009 created order_proposals_pending_idx with this justification:
--
--     a decided proposal leaves the index, so listing what needs attention
--     stays the same cost on day 1000 as on day 1.
--
-- That is true of an APPROVED proposal and false of an EXPIRED one. A proposal
-- is decided by ProposalStore.Claim, which sets approver. Nobody claims a
-- proposal nobody signed, so approver stays '' for the life of the row and the
-- row never leaves the index. Every proposal that has ever lapsed unsigned was
-- still in it, and the index grew monotonically with the lapse rate.
--
-- Pending filtered those rows out at QUERY time (expires_at > now), so they were
-- invisible to the caller and not to the index. Nothing returned a wrong answer;
-- the pending-queue read simply walked more index every day — and that read is
-- what an approver's screen calls, so the symptom would have arrived as "the
-- approvals page got slow" long after the cause.
--
-- # Why this is a DROP and not a narrowing
--
-- 0010 added order_proposals_expiry_sweep_idx on the SAME three columns with a
-- strictly narrower predicate. Narrowing the pending index to match — the
-- obvious repair — would have produced two identical indexes on one table, each
-- maintained on every write, which is the cost of the bug plus the cost of the
-- fix. The sweep index already IS the bounded pending index; it only needed a
-- name that says so and a query that can use it.
--
-- # The predicate has to be spelled in the query, and that is load-bearing
--
-- 0009's own comment states the rule and it applies with more force here:
-- Postgres uses a partial index only where it can prove the predicate holds.
-- `expires_at > now()` does NOT prove `expiry_announced_at IS NULL` — the
-- CHECK added by 0010 makes the two equivalent in practice, but the planner
-- does not reason across a CHECK constraint. So ProposalStore.Pending now
-- spells out BOTH clauses. Removing either as redundant returns this read to a
-- sequential scan, which is the failure 0009 warned about and this migration
-- would then have made worse rather than better.
--
-- test/arch/pending_index_predicate_test.go fails the build if the query and
-- this predicate drift apart, because a comment saying "keep these in step" is
-- exactly what did not keep them in step for order_proposals_pending_idx.
--
-- # Adding the clause is not merely redundant — it is more correct
--
-- A proposal is announced only when a pod observed expires_at <= its own clock
-- (AnnounceExpiry's WHERE). A pod running fast can therefore announce a row a
-- slower reader still considers live. Before this change that row would come
-- back on the pending queue as work, after the estate had already been told the
-- order was rejected and would not trade. It is now excluded, which is the
-- answer that matches what was published.
--
-- # The boundedness has a precondition, stated so it is not assumed
--
-- The index is bounded by (live proposals + one sweep interval of expiries)
-- ONLY WHILE THE SWEEP RUNS. With OMS_PROPOSAL_EXPIRY_INTERVAL=0 nothing sets
-- expiry_announced_at, no row ever leaves this index, and the growth this
-- migration removes comes back exactly as it was. That is not a reason to keep
-- two indexes; it is a reason the sweep being disabled is a posture main logs
-- loudly at startup (#539).

-- The rename first, so the drop below cannot leave the table with no index for
-- the pending read even for the length of this transaction.
ALTER INDEX order_proposals_expiry_sweep_idx RENAME TO order_proposals_open_idx;

-- OPEN = undecided AND not yet reported dead. Both halves are needed: approver
-- = '' alone is what grew forever, and expiry_announced_at IS NULL alone would
-- admit a signed proposal.
COMMENT ON INDEX order_proposals_open_idx IS
    'Proposals still awaiting a decision and not yet announced as expired. Serves both '
    'ProposalStore.Pending (the approver queue) and ProposalStore.ExpiredUnannounced (the '
    'sweep). Both queries must spell out approver = '''' AND expiry_announced_at IS NULL or '
    'Postgres cannot prove the partial predicate and falls back to a sequential scan (#548).';

DROP INDEX order_proposals_pending_idx;
