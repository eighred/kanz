-- 0013: the fills already folded INTO THE ORDER AGGREGATE (#782).
--
-- # What was missing
--
-- ApplyFill folds a fill into the order — recomputing filled_quantity, the
-- quantity-weighted average_fill_price and the status — with an over-fill guard
-- and NO fill_id dedup. Folding a fill the order already contains silently
-- double-counts both numbers, and nothing downstream catches it: the stored
-- OrderState keeps only the cumulative aggregate, so there is no record of which
-- fills produced it.
--
-- The position projector has had this guarantee since 0003. position_fills'
-- PRIMARY KEY (tenant_id, fill_id) is what makes the POSITION fold exactly-once
-- across pods. The order aggregate — which cancel, amend and the read API all
-- consult directly — had no equivalent, so reconciliation refused to adopt a
-- venue view carrying more than one fill rather than risk it. That refusal is
-- retired in the same commit as this table: the guard and the reason for it
-- leave together.
--
-- # Why a SECOND table rather than reusing position_fills
--
-- They are two different consumers claiming the same fill for two different
-- folds. position_fills says "the POSITION has this fill"; order_fills says "the
-- ORDER AGGREGATE has this fill". Sharing one table would let whichever fold ran
-- first suppress the other — the position would move and the order would never
-- record the execution that moved it, or the reverse. Each fold owns its own
-- claim.
--
-- # The claim is read the way 0003 reads its own
--
-- INSERT ... ON CONFLICT DO NOTHING, and the writer reads RowsAffected: 1 ⇒ this
-- write owns the fill and folds it, 0 ⇒ it is already folded and this write must
-- not land at all. A SELECT-then-INSERT would reintroduce the check-then-act
-- window this exists to close.
--
-- IT IS IN THE SAME TRANSACTION AS THE STATE WRITE AND THE OUTBOX RECORD, which
-- is the part that makes it a guarantee rather than a hint (#292). A claim
-- committed separately from the fold has two failure modes and both are real: a
-- claim that lands without its fold loses the fill permanently, and a fold that
-- lands without its claim double-counts on the next redelivery.
--
-- order_id is carried for attribution rather than uniqueness: a fill belongs to
-- exactly one order, and being able to answer "which order claimed this fill"
-- from the table is worth one column.

CREATE TABLE order_fills (
    tenant_id  TEXT        NOT NULL DEFAULT app_current_tenant(),
    order_id   TEXT        NOT NULL,
    fill_id    TEXT        NOT NULL,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, fill_id)
);

-- "Which fills does this order contain" — the question the aggregate cannot
-- answer from its own state, asked by reconciliation and by anyone auditing a
-- weighted average that looks wrong.
CREATE INDEX order_fills_order_idx ON order_fills (tenant_id, order_id);

-- Tenant isolation (MT-01d/e), the same shape 0003 gives position_fills.
-- app_current_tenant() RAISES when the GUC is unset, so an unscoped session gets
-- an ERROR rather than a silently empty claim set — and a silently empty claim
-- set would make every fill look new, which is the exact defect this table
-- closes.
ALTER TABLE order_fills ENABLE ROW LEVEL SECURITY;
ALTER TABLE order_fills FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON order_fills
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());
