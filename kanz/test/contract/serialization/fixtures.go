// Package serialization owns the cross-language wire-fixture catalog for
// EVT-21b. Go is the canonical generator: every fixture is built here,
// marshaled with deterministic proto-wire output, and written to
// fixtures/*.bin + fixtures/manifest.json by the genfixtures CLI. The
// Python and TS bus clients load the same files and assert byte-identical
// round-trip — that is the wire-level "Go bytes can be read by Python /
// TS, with identical semantics" claim EVT-21b makes.
//
// Why Go is canonical (not a language-neutral source of truth like YAML):
// the Envelope is the constitution and the Go bus client (EVT-17) is the
// reference implementation. Building fixtures from Go envelopes guarantees
// the fixtures track the live Go bindings; any drift in the .proto
// surfaces in the Go test before the Python / TS tests run.
package serialization

import (
	"encoding/hex"
	"fmt"
	"sort"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
)

// fixedTime pins every fixture's three Timestamp fields so the marshaled
// bytes are byte-stable across regenerations and across languages.
var fixedTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// Fixture is one named envelope + opaque payload pair. The payload is
// distinct per fixture so a test asserting payload bytes can confirm the
// loader didn't accidentally swap fixtures.
type Fixture struct {
	Name     string
	Envelope *envelopepb.Envelope
	Payload  []byte
}

// Fixtures returns the canonical catalog. The set covers every event
// class plus the lineage, no-partition, and quality-flag corner cases —
// these are the variants that have failed cross-language round-trips in
// other systems (zero-valued fields, repeated-enum encoding, empty
// optional strings).
func Fixtures() []Fixture {
	ts := timestamppb.New(fixedTime)
	return []Fixture{
		{
			Name: "fact_root",
			Envelope: &envelopepb.Envelope{
				EventId:          "fix-fact-1",
				EventType:        "market.equity.trade",
				SchemaVersion:    1,
				EnvelopeVersion:  1,
				TenantId:         "acme",
				EventClass:       envelopepb.EventClass_EVENT_CLASS_FACT,
				Domain:           "market",
				EventTime:        ts,
				IngestionTime:    ts,
				PublishTime:      ts,
				CorrelationId:    "fix-fact-1",
				Source:           "kanz-fixtures/gen",
				ProducerVersion:  "fixtures-1.0.0",
				PartitionKey:     "AAPL",
				ProducerSequence: 1,
				IdempotencyKey:   "fix-fact-1",
				PayloadSchemaRef: "market.v1.MarketDataEvent:1",
			},
			Payload: []byte("fact-root-payload"),
		},
		{
			Name: "fact_with_lineage",
			Envelope: &envelopepb.Envelope{
				EventId:          "fix-fact-2",
				EventType:        "risk.exposure.computed",
				SchemaVersion:    1,
				EnvelopeVersion:  1,
				TenantId:         "acme",
				EventClass:       envelopepb.EventClass_EVENT_CLASS_FACT,
				Domain:           "risk",
				EventTime:        ts,
				IngestionTime:    ts,
				PublishTime:      ts,
				CorrelationId:    "fix-root-correlation",
				CausationId:      "fix-fact-1",
				TraceContext:     "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
				Source:           "kanz-fixtures/gen",
				ProducerVersion:  "fixtures-1.0.0",
				PartitionKey:     "PORT-1",
				ProducerSequence: 1,
				IdempotencyKey:   "fix-fact-2",
				PayloadSchemaRef: "risk.v1.ExposureSet:1",
			},
			Payload: []byte("fact-lineage-payload"),
		},
		{
			Name: "command_caller_key",
			Envelope: &envelopepb.Envelope{
				EventId:          "fix-cmd-1",
				EventType:        "risk.command.rebalance",
				SchemaVersion:    1,
				EnvelopeVersion:  1,
				TenantId:         "acme",
				EventClass:       envelopepb.EventClass_EVENT_CLASS_COMMAND,
				Domain:           "risk",
				EventTime:        ts,
				IngestionTime:    ts,
				PublishTime:      ts,
				CorrelationId:    "fix-cmd-1",
				Source:           "kanz-fixtures/gen",
				ProducerVersion:  "fixtures-1.0.0",
				PartitionKey:     "PORT-1",
				ProducerSequence: 1,
				// COMMAND: idempotency_key is caller-supplied (NOT == event_id).
				IdempotencyKey:   "caller-supplied-cmd-key",
				PayloadSchemaRef: "risk.v1.RebalanceCommand:1",
			},
			Payload: []byte("command-payload"),
		},
		{
			Name: "state_snapshot",
			Envelope: &envelopepb.Envelope{
				EventId:          "fix-snap-1",
				EventType:        "risk.portfolio.snapshot",
				SchemaVersion:    1,
				EnvelopeVersion:  1,
				TenantId:         "acme",
				EventClass:       envelopepb.EventClass_EVENT_CLASS_STATE_SNAPSHOT,
				Domain:           "risk",
				EventTime:        ts,
				IngestionTime:    ts,
				PublishTime:      ts,
				CorrelationId:    "fix-snap-1",
				Source:           "kanz-fixtures/gen",
				ProducerVersion:  "fixtures-1.0.0",
				PartitionKey:     "PORT-1",
				ProducerSequence: 42,
				IdempotencyKey:   "fix-snap-1",
				PayloadSchemaRef: "risk.v1.PortfolioSnapshot:1",
			},
			Payload: []byte("snapshot-payload"),
		},
		{
			// No partition_key ⇒ producer_sequence MUST be 0
			// (envelope.proto field comment). Covers the corner case the
			// validator enforces explicitly.
			Name: "observation_no_partition",
			Envelope: &envelopepb.Envelope{
				EventId:          "fix-obs-1",
				EventType:        "platform.metric.observed",
				SchemaVersion:    1,
				EnvelopeVersion:  1,
				TenantId:         "acme",
				EventClass:       envelopepb.EventClass_EVENT_CLASS_OBSERVATION,
				Domain:           "platform",
				EventTime:        ts,
				IngestionTime:    ts,
				PublishTime:      ts,
				CorrelationId:    "fix-obs-1",
				Source:           "kanz-fixtures/gen",
				ProducerVersion:  "fixtures-1.0.0",
				IdempotencyKey:   "fix-obs-1",
				PayloadSchemaRef: "observation.v1.MetricObservation:1",
				// PartitionKey: "" (default), ProducerSequence: 0 (default).
			},
			Payload: []byte("observation-payload"),
		},
		{
			// Asserts repeated-enum wire encoding survives the round-trip.
			// LATE is the right cross-language flag to test because it's a
			// regular data-integrity flag, not the REPLAYED flag the
			// validator special-cases.
			Name: "observation_with_late_flag",
			Envelope: &envelopepb.Envelope{
				EventId:          "fix-obs-2",
				EventType:        "platform.metric.observed",
				SchemaVersion:    1,
				EnvelopeVersion:  1,
				TenantId:         "acme",
				EventClass:       envelopepb.EventClass_EVENT_CLASS_OBSERVATION,
				Domain:           "platform",
				EventTime:        ts,
				IngestionTime:    ts,
				PublishTime:      ts,
				CorrelationId:    "fix-obs-2",
				Source:           "kanz-fixtures/gen",
				ProducerVersion:  "fixtures-1.0.0",
				IdempotencyKey:   "fix-obs-2",
				PayloadSchemaRef: "observation.v1.MetricObservation:1",
				QualityFlags: []envelopepb.QualityFlag{
					envelopepb.QualityFlag_QUALITY_FLAG_LATE,
				},
			},
			Payload: []byte("observation-late-payload"),
		},
	}
}

// Manifest is the language-agnostic description of the fixture set the
// non-Go tests load. Field names are proto canonical (snake_case); enums
// are encoded as their string names so a language can resolve them via
// its enum-by-name API without hard-coding integer values.
type Manifest struct {
	Version  int               `json:"version"`
	Fixtures []FixtureManifest `json:"fixtures"`
}

// FixtureManifest is one fixture's wire description: which .bin file holds
// it, what the unframed envelope must look like field-by-field, and the
// raw payload bytes (hex-encoded) the frame must yield. WireBytes is the
// generator's serialized EventFrame; the CLI writes it to .bin and the
// parity test consumes it in-memory.
type FixtureManifest struct {
	Name       string         `json:"name"`
	File       string         `json:"file"`
	PayloadHex string         `json:"payload_hex"`
	Envelope   EnvelopeFields `json:"envelope"`
	WireBytes  []byte         `json:"-"`
}

// EnvelopeFields is the per-field assertion target. Timestamps are split
// into seconds + nanos (matching google.protobuf.Timestamp wire shape) so
// every language can compare them without parsing RFC 3339. quality_flags
// is a list of enum string names.
type EnvelopeFields struct {
	EventID          string    `json:"event_id"`
	EventType        string    `json:"event_type"`
	SchemaVersion    uint32    `json:"schema_version"`
	EnvelopeVersion  uint32    `json:"envelope_version"`
	EventClass       string    `json:"event_class"`
	Domain           string    `json:"domain"`
	EventTime        TimeStamp `json:"event_time"`
	IngestionTime    TimeStamp `json:"ingestion_time"`
	PublishTime      TimeStamp `json:"publish_time"`
	CorrelationID    string    `json:"correlation_id"`
	CausationID      string    `json:"causation_id"`
	TraceContext     string    `json:"trace_context"`
	Source           string    `json:"source"`
	ProducerVersion  string    `json:"producer_version"`
	PartitionKey     string    `json:"partition_key"`
	ProducerSequence uint64    `json:"producer_sequence"`
	IdempotencyKey   string    `json:"idempotency_key"`
	QualityFlags     []string  `json:"quality_flags"`
	PayloadSchemaRef string    `json:"payload_schema_ref"`
}

// TimeStamp mirrors google.protobuf.Timestamp's wire layout so the
// manifest is decodable without a Timestamp parser.
type TimeStamp struct {
	Seconds int64 `json:"seconds"`
	Nanos   int32 `json:"nanos"`
}

func tsFromPB(ts *timestamppb.Timestamp) TimeStamp {
	if ts == nil {
		return TimeStamp{}
	}
	return TimeStamp{Seconds: ts.Seconds, Nanos: ts.Nanos}
}

func qualityFlagNames(flags []envelopepb.QualityFlag) []string {
	if len(flags) == 0 {
		return []string{}
	}
	out := make([]string, len(flags))
	for i, f := range flags {
		out[i] = f.String()
	}
	return out
}

// BuildAll marshals every fixture deterministically and returns the
// manifest with each fixture's .bin payload populated in WireBytes. The
// genfixtures CLI writes these to disk; the parity test marshals in
// memory and asserts the round-trip directly.
func BuildAll() (*Manifest, error) {
	fxs := Fixtures()
	m := &Manifest{Version: 1, Fixtures: make([]FixtureManifest, 0, len(fxs))}
	opts := proto.MarshalOptions{Deterministic: true}
	for _, f := range fxs {
		body, err := opts.Marshal(&envelopepb.EventFrame{
			Envelope: f.Envelope,
			Payload:  f.Payload,
		})
		if err != nil {
			return nil, fmt.Errorf("marshal %s: %w", f.Name, err)
		}
		m.Fixtures = append(m.Fixtures, FixtureManifest{
			Name:       f.Name,
			File:       f.Name + ".bin",
			PayloadHex: hex.EncodeToString(f.Payload),
			Envelope:   envelopeFields(f.Envelope),
			WireBytes:  body,
		})
	}
	sort.Slice(m.Fixtures, func(i, j int) bool {
		return m.Fixtures[i].Name < m.Fixtures[j].Name
	})
	return m, nil
}

func envelopeFields(env *envelopepb.Envelope) EnvelopeFields {
	return EnvelopeFields{
		EventID:          env.EventId,
		EventType:        env.EventType,
		SchemaVersion:    env.SchemaVersion,
		EnvelopeVersion:  env.EnvelopeVersion,
		EventClass:       env.EventClass.String(),
		Domain:           env.Domain,
		EventTime:        tsFromPB(env.EventTime),
		IngestionTime:    tsFromPB(env.IngestionTime),
		PublishTime:      tsFromPB(env.PublishTime),
		CorrelationID:    env.CorrelationId,
		CausationID:      env.CausationId,
		TraceContext:     env.TraceContext,
		Source:           env.Source,
		ProducerVersion:  env.ProducerVersion,
		PartitionKey:     env.PartitionKey,
		ProducerSequence: env.ProducerSequence,
		IdempotencyKey:   env.IdempotencyKey,
		QualityFlags:     qualityFlagNames(env.QualityFlags),
		PayloadSchemaRef: env.PayloadSchemaRef,
	}
}
