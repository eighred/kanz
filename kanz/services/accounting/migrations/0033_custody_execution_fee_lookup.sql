-- Historical receipt reads follow the fee postings attached to one booked trade.
CREATE INDEX ledger_execution_fee_source_idx ON ledger_entries (tenant_id, portfolio_id, source_ref)
    WHERE entry_type = 3;
