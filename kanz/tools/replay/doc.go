// Package replay is the Kanz replay toolkit. EVT-20a is the read-only
// foundation: given a Kafka topic and a bounded offset- or timestamp-range, the
// Reader streams unframed events (envelope + payload + Kafka metadata) without
// any consumer-group state, so a replay never mutates live consumer positions.
// EVT-20b layers on the isolated `replay.{run_id}.*` namespace and publish
// path; EVT-20c stamps QUALITY_FLAG_REPLAYED and wires the live-sink
// hard-reject defended by kanz-schemas/README.md § Event Class Rules §5.
package replay
