-- Filled independently of mutable protobuf state, only after the signed order
-- response identifies our client order. Existing claims retain unknown ACKs.
ALTER TABLE venue_orders ADD COLUMN placement_account_uid text,
    ADD COLUMN placement_symbol text,
    ADD COLUMN placement_order_id bigint,
    ADD COLUMN placement_source text,
    ADD COLUMN placement_ack_at timestamptz,
    ADD CONSTRAINT venue_placement_ack_complete CHECK (
        (placement_ack_at IS NULL AND placement_source IS NULL AND placement_account_uid IS NULL
         AND placement_symbol IS NULL AND placement_order_id IS NULL)
        OR (placement_ack_at IS NOT NULL AND placement_source IS NOT NULL
            AND placement_source IN ('placement_response','recovery_query','execution_report')
            AND placement_account_uid IS NOT NULL AND placement_account_uid <> ''
            AND placement_symbol IS NOT NULL AND placement_symbol <> ''
            AND placement_order_id IS NOT NULL AND placement_order_id > 0)
    );

CREATE UNIQUE INDEX venue_placement_source_identity
    ON venue_orders(tenant_id, placement_account_uid, placement_symbol, placement_order_id)
    WHERE placement_ack_at IS NOT NULL;
