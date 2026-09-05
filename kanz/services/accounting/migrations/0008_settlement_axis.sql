-- 0008: the trade-date / settlement-date axis on the book of record (#1043).
--
-- THE JOURNAL HAD NO SETTLEMENT DIMENSION AT ALL. ledger_entries carried
-- effective_time and knowledge_time and nothing else temporal, and every fill was
-- booked as fully settled at the instant of execution: the position leg and the
-- FULL cash leg landed at the venue's execution time, unconditionally. There was
-- no column anywhere that could express "I traded it, I do not yet own it", and
-- every control that reads this book — NAV, exposure, leverage, mandate headroom,
-- margin, and buying power through internal/cashview — inherited that assumption
-- without being told it was one.
--
-- # This is NOT the bitemporal axis, and that is the easy mistake here
--
-- effective_time versus knowledge_time is a RESTATEMENT axis: one event, and two
-- views of it as the book's knowledge changes. Trade date versus settlement date
-- is orthogonal to both — one uncontested event with TWO economic dates, the day
-- it was traded and the day the asset and the cash actually change hands. A book
-- can be perfectly bitemporal and still unable to say that a purchase has not
-- settled, which is exactly where this table was.
--
-- # UNKNOWN IS THE DEFAULT AND IT IS A REAL THIRD VALUE
--
-- settlement_status DEFAULT 0 is ledger.SettlementUnknown, not "settled". A
-- producer that cannot assert a settlement basis — accounting.v1.LedgerEntry has
-- no settlement field, so the cash-movement decoder genuinely cannot — writes 0,
-- and the fold keeps that entry OUT of the settled book while counting it as an
-- unknown. Book.SettlementBasisComplete then reports the settled basis as
-- unanswerable rather than returning a lower bound somebody could spend against.
-- Defaulting to "settled" would have been the cheaper migration and it is the
-- precise failure this platform rules against: never unknown ⇒ 0 ⇒ proceed as if
-- known.
--
-- settlement_date is NULLABLE for the same reason. NULL is "nobody asserted a
-- settlement date", and it is deliberately NOT defaulted to effective_time —
-- "settles the day it traded" is a claim about a venue's convention, and it is
-- made in one place by the component that knows the venue (ledger.fillSettlement),
-- never by the schema on behalf of a producer that said nothing.
--
-- # Existing rows
--
-- Every row written before this migration honestly asserted no settlement basis,
-- and 0 / NULL says exactly that. They are NOT backfilled to settled: the journal
-- is append-only and WORM-protected (0004), so a backfill would be both forbidden
-- and a fabrication — it would put a claim into the book of record that no
-- producer ever made.

ALTER TABLE ledger_entries
    ADD COLUMN IF NOT EXISTS settlement_status INTEGER     NOT NULL DEFAULT 0,  -- ledger.SettlementBasis; 0 = UNKNOWN
    ADD COLUMN IF NOT EXISTS settlement_date   TIMESTAMPTZ;                     -- NULL = nobody asserted one

-- THE UNSETTLED LADDER'S READ: what is owed, and when. A forward cash ladder or a
-- settled-basis balance asks "which of this portfolio's entries are not settled,
-- ordered by the date they are due" — a range scan, not a scan of the journal.
-- PARTIAL on the non-settled statuses so the index holds the OUTSTANDING legs
-- rather than a second copy of every entry the fund has ever booked; with both
-- venue adapters on spot today that is a near-empty index, which is the point.
CREATE INDEX IF NOT EXISTS ledger_entries_unsettled_idx
    ON ledger_entries (tenant_id, portfolio_id, settlement_date, entry_id)
    WHERE settlement_status <> 1;   -- 1 = ledger.SettlementSettled

-- THE CHECKPOINT MUST CARRY BOTH BASES OR THE MATERIALIZED BOOK IS WRONG ON ONE.
--
-- MaterializeCurrent restores a book from ledger_snapshots and folds only the
-- journal TAIL onto it. A checkpoint that held the traded fold and not the settled
-- one would produce a book whose settled balances are short by everything the
-- checkpoint absorbed — not a late number but a wrong one, and it would look
-- exactly like a portfolio whose trades had genuinely not settled.
--
-- ALL FOUR ARE NULLABLE, and NULL is load-bearing: it marks a checkpoint written
-- before this axis existed, whose settled maps are empty because nobody wrote them
-- rather than because nothing settled. LoadSnapshot reads a NULL settled_positions
-- as "this checkpoint states no settled view", RestoreBook clears the book's
-- SettlementBasisComplete, and the settled read fails closed until the
-- Snapshotter's next pass rewrites the row. That is the same stance
-- 0005_ledger_snapshot_tail.sql's max_effective_time takes on a fenceless
-- checkpoint, for the same reason: an unbounded, loud recomputation beats a
-- bounded, quiet, wrong answer.
--
-- Unlike ledger_entries this table is a DERIVED CACHE and is outside the WORM
-- trigger, so it self-heals: no backfill is needed or attempted.
ALTER TABLE ledger_snapshots
    ADD COLUMN IF NOT EXISTS settled_positions  JSONB,     -- NULL = checkpoint predates the settlement axis
    ADD COLUMN IF NOT EXISTS settled_cash       JSONB,
    ADD COLUMN IF NOT EXISTS unknown_settlement INTEGER,   -- entries folded that asserted no basis
    ADD COLUMN IF NOT EXISTS pending_settlement INTEGER;   -- entries folded that are traded-not-settled
