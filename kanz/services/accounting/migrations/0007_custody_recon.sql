-- 0007: custody reconciliation — statements, runs and the break lifecycle (#962).
--
-- THE ENGINE WAS HERE AND THE CONTROL WAS NOT. recon.Reconcile has compared the
-- folded book against a custodian statement since IBOR-01e, and its only caller
-- was an HTTP handler whose statement arrived in the REQUEST BODY. Nothing
-- ingested a statement, nothing ran on a schedule, nothing recorded that a
-- reconciliation had happened, and a break had no lifecycle once found.
--
-- # Why any of this is durable rather than in-process
--
-- Two of the three tables could in principle be rebuilt by replaying the bus.
-- THE BREAK LIFECYCLE COULD NOT. An assignee and an explanation are an operator's
-- work and exist nowhere else, so holding them in memory means a restart silently
-- resets every break to OPEN — which regenerates, every single day, the
-- undifferentiated list that made reconciliation output unreadable and is the
-- reason this control was informational instead of operational.
--
-- The ages are the other half. first_seen_at is what "this position has disagreed
-- with the custodian for three weeks" is measured from, and it is the single most
-- useful sentence a reconciliation can produce. A process-local map makes the
-- oldest break in the estate exactly as old as the newest deploy.

-- CUSTODIAN STATEMENTS — the independent view, as received.
--
-- A LEVEL AND NOT A DELTA, exactly as accounting.v1.PortfolioCashBalance is: the
-- custodian states what it holds, so a redelivery is idempotent and a gap is
-- self-correcting on the next statement. positions and cash are JSONB maps of
-- {key: decimal-as-text}: TEXT and not NUMERIC or float, because a book of record
-- never round-trips money through a type that rounds, and *big.Rat parses and
-- renders text exactly. The same reason ledger_entries stores its amounts as text.
CREATE TABLE custody_statements (
    tenant_id     TEXT        NOT NULL DEFAULT app_current_tenant(),
    statement_id  TEXT        NOT NULL,
    custodian_id  TEXT        NOT NULL,
    portfolio_id  TEXT        NOT NULL,
    business_date DATE        NOT NULL,   -- the CLOSE the statement speaks for
    positions     JSONB       NOT NULL DEFAULT '{}'::jsonb,  -- instrument -> quantity (text)
    cash          JSONB       NOT NULL DEFAULT '{}'::jsonb,  -- currency  -> balance  (text)
    received_at   TIMESTAMPTZ NOT NULL,   -- knowledge time to business_date's effective time
    PRIMARY KEY (tenant_id, statement_id)
);

-- The reconciler's only read: the newest statement for a subject. business_date
-- is part of the key because a statement that arrives late still reconciles the
-- date it DESCRIBES, not the date it turned up.
CREATE INDEX custody_statements_subject_idx
    ON custody_statements (tenant_id, portfolio_id, custodian_id, business_date, received_at DESC);

-- RECONCILIATION RUNS — the record that the control ran.
--
-- ONE ROW PER RUN, INCLUDING THE ONES THAT FOUND NOTHING AND THE ONES THAT FOUND
-- NO STATEMENT. That is the whole point of the table. Before #962 the control
-- produced output only when a human ran it AND it found something, so "reconciled
-- clean today", "nobody ran it", "the custodian stopped sending" and "the book
-- would not materialize" were one observable event: silence.
--
-- ROWS ARE NEVER UPDATED. A restated statement for an already-reconciled date
-- produces a NEW run, so the history shows both and which came first — the
-- audit requirement is to reconstruct what the system knew and when, and an
-- overwritten run destroys exactly that.
CREATE TABLE custody_runs (
    tenant_id      TEXT        NOT NULL DEFAULT app_current_tenant(),
    run_id         TEXT        NOT NULL,
    portfolio_id   TEXT        NOT NULL,
    custodian_id   TEXT        NOT NULL,
    business_date  DATE        NOT NULL,
    outcome        TEXT        NOT NULL,   -- clean | breaks | no_statement | failed
    statement_id   TEXT        NOT NULL DEFAULT '',
    tolerance      TEXT        NOT NULL DEFAULT '0',
    completed_at   TIMESTAMPTZ NOT NULL,
    failure_reason TEXT        NOT NULL DEFAULT '',
    PRIMARY KEY (tenant_id, run_id)
);

-- The staleness read: the newest run for a (portfolio, custodian) pair across ALL
-- business dates. It deliberately ignores the date — a run for last Friday
-- performed this morning is recent evidence that the control is alive, whatever
-- date it reconciled.
CREATE INDEX custody_runs_recent_idx
    ON custody_runs (tenant_id, portfolio_id, custodian_id, completed_at DESC);

-- BREAKS — the working items, with the lifecycle that makes them chaseable.
--
-- break_id IS STABLE ACROSS RUNS, derived from (portfolio, custodian, kind, key).
-- A break given a fresh id on every daily run would be permanently one day old
-- and the age — the thing an operator triages by — would be unrepresentable. So
-- the primary key is that derived id, and a redetection UPDATES this row rather
-- than inserting beside it.
--
-- WHAT A REDETECTION MAY TOUCH IS DELIBERATELY NARROW: the three figures and
-- last_seen_at. status, assignee, explanation and first_seen_at are the
-- operator's, and a run that overwrote them would reset somebody's assignment
-- every morning — the specific bug that turns a break queue into noise.
CREATE TABLE custody_breaks (
    tenant_id         TEXT        NOT NULL DEFAULT app_current_tenant(),
    break_id          TEXT        NOT NULL,
    portfolio_id      TEXT        NOT NULL,
    custodian_id      TEXT        NOT NULL,
    kind              TEXT        NOT NULL,   -- recon.BreakKind.String()
    break_key         TEXT        NOT NULL,   -- instrument id, or currency code for a cash break
    ibor              TEXT        NOT NULL,   -- exact decimals as text; never float
    custodian         TEXT        NOT NULL,
    difference        TEXT        NOT NULL,
    status            TEXT        NOT NULL,   -- open | assigned | explained | resolved
    assignee          TEXT        NOT NULL DEFAULT '',
    explanation       TEXT        NOT NULL DEFAULT '',
    first_seen_at     TIMESTAMPTZ NOT NULL,   -- never advanced by a redetection
    last_seen_at      TIMESTAMPTZ NOT NULL,
    status_changed_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, break_id)
);

-- The gauge's read and the operator queue's read are the same one: what is still
-- outstanding. PARTIAL on the non-terminal statuses so the index holds the WORK
-- and not the history of everything ever reconciled.
--
-- 'explained' IS IN THIS INDEX ON PURPOSE. An explanation is a claim about the
-- future ("this clears when tomorrow's settlement lands"), not evidence the
-- difference is gone, and a break still present a week after being explained is a
-- WORSE signal than an unexplained one. Dropping it out of the outstanding set
-- here would let a wrong explanation silence the control indefinitely.
CREATE INDEX custody_breaks_outstanding_idx
    ON custody_breaks (tenant_id, custodian_id, kind, first_seen_at)
    WHERE status IN ('open', 'assigned', 'explained');

-- Tenant isolation (MT-01d/e). These three tables carry one fund's custodian
-- holdings, the discrepancies against its book, and the operators investigating
-- them — a cross-tenant read here is one fund seeing another's positions and
-- another's unresolved control failures. app_current_tenant() RAISES on an
-- unscoped session rather than returning NULL, so a connection that forgot the
-- GUC errors instead of quietly reading zero rows (see 0002).
ALTER TABLE custody_statements ENABLE ROW LEVEL SECURITY;
ALTER TABLE custody_statements FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON custody_statements
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());

ALTER TABLE custody_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE custody_runs FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON custody_runs
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());

ALTER TABLE custody_breaks ENABLE ROW LEVEL SECURITY;
ALTER TABLE custody_breaks FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON custody_breaks
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());
