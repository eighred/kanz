// Package sink is the lakehouse landing layer (LAKE-01a). Sink is the seam a
// concrete table format (Iceberg/Delta) plugs into; the default FileSink lands
// Hive-partitioned NDJSON that a catalog ingest (Trino/Spark) commits as a
// table.
package sink

import (
	"context"
	"encoding/json"
	"time"
)

// Row is one event materialized for the lakehouse: the stable envelope columns
// plus the decoded payload as a nested object (nil for envelope-only events or a
// permanent decode failure, in which case DecodeError explains it). Table
// identity is (Domain, Entity); EventTime partitions it Hive-style by date.
type Row struct {
	EventID       string          `json:"event_id"`
	CorrelationID string          `json:"correlation_id,omitempty"`
	CausationID   string          `json:"causation_id,omitempty"`
	Domain        string          `json:"domain"`
	Entity        string          `json:"entity"`
	EventType     string          `json:"event_type"`
	EventClass    string          `json:"event_class"`
	TenantID      string          `json:"tenant_id,omitempty"`
	Source        string          `json:"source"`
	PartitionKey  string          `json:"partition_key,omitempty"`
	EventTime     time.Time       `json:"event_time"`
	SchemaVersion uint32          `json:"schema_version"`
	SchemaRef     string          `json:"payload_schema_ref,omitempty"`
	IngestedAt    time.Time       `json:"ingested_at"`
	Payload       json.RawMessage `json:"payload,omitempty"`
	DecodeError   string          `json:"decode_error,omitempty"`
}

// Sink is the lakehouse landing target. Write appends one row (buffered, made
// durable by Flush/Close). Sinks see at-least-once delivery — a duplicate
// carries the same event_id and is resolved by downstream compaction, so Write
// need not dedup.
type Sink interface {
	Write(ctx context.Context, row Row) error
	Flush() error
	Close() error
}
