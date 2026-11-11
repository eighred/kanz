-- DERIV-01a: the derivative contract-terms store — strike, expiry and the rest
-- of the static specification a quote joins to (#345).
--
-- WHY THIS TABLE EXISTS AT ALL, since the schema for it has existed for a while
-- and nothing stored it. reference.v1.ContractTerms defines the terms, and
-- compute.TermsProvider is the seam that resolves them — consumed by the Greeks
-- layer (greeks.go) and the revaluation path (reval.go). Every implementation of
-- that seam in the tree is a TEST DOUBLE. So the derivative pricing path has
-- been reading contract terms from nothing in production, and vol calibration
-- cannot start without somewhere to read them from.
--
-- WHY HERE AND NOT IN datamaster. datamaster resolves a golden SecurityMaster
-- ACROSS VENDOR FEEDS, and its golden_records is tenant-scoped because a
-- resolution can legitimately differ per tenant — different vendors, different
-- arbitration. A listed option's strike is not a resolution: it is a fact about
-- the contract, identical for everyone who trades it. That is the same category
-- as price_observations next to it, and it gets the same treatment.
--
-- NOT tenant-scoped: contract terms are universal market fact, not tenant-owned
-- state, so no tenant_id / RLS — the same call price_observations made ("the
-- same tick prices every tenant's book"). Tenant isolation lives on the
-- portfolio state that consumes these terms, never on the terms themselves.
-- Readers open pg.NewGlobalPool with a stated justification, as risk-engine
-- already does for price_observations.
--
-- POINT-IN-TIME BY as_of, mirroring InstrumentReference and ContractTerms's own
-- documented contract ("as_of versions the record for point-in-time reads").
-- A restatement is a new row at a newer as_of, not an overwrite: a backtest as
-- of T must read the terms as they were understood at T. Options are amended in
-- practice — corporate actions adjust strikes and multipliers — and a store that
-- overwrote would silently reprice history against terms that did not apply.
--
-- The terms themselves are stored as the marshaled reference.v1.ContractTerms,
-- the same choice price_observations makes for common.v1.Decimal: the oneof
-- carries three shapes (option, swap, future), and relationalising all three
-- would be three sparse tables or one wide one, both of which drift from the
-- proto the moment a variant gains a field. The columns lifted OUT of the blob
-- are exactly the ones a query needs to filter on.

CREATE TABLE IF NOT EXISTS contract_terms (
    instrument_id  TEXT        NOT NULL,   -- the DERIVATIVE's canonical id, not the underlying's
    as_of          TIMESTAMPTZ NOT NULL,   -- effective time of this terms record
    underlying_id  TEXT,                   -- options and futures; NULL for swaps, which reference no single instrument
    kind           TEXT        NOT NULL,   -- OPTION / SWAP / FUTURE — the oneof case, lifted so a scan can filter without decoding
    terms          BYTEA       NOT NULL,   -- marshaled reference.v1.ContractTerms
    ingested_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (instrument_id, as_of)
);

-- THE REVERSE DIRECTION, WHICH IS THE ONE THE SEAM CANNOT EXPRESS (#345).
--
-- compute.TermsProvider is keyed by the OPTION's instrument_id and returns
-- underlying_id. volsurface.QuoteProvider.Quotes is keyed by the UNDERLYING and
-- must return its whole chain. That direction is not derivable from the seam —
-- it is a different query — so the store has to index both ways or every
-- calibration becomes a full scan.
--
-- Partial, because only options and futures carry an underlying and a swap row
-- would otherwise occupy an index entry under NULL that no query ever reads.
CREATE INDEX IF NOT EXISTS contract_terms_underlying_idx
    ON contract_terms (underlying_id, as_of DESC)
    WHERE underlying_id IS NOT NULL;
