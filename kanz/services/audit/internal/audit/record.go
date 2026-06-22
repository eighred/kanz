// Package audit is the append-only audit projection (AUDIT-01a): it consumes
// the event backbone and materializes decisions, command outcomes, data-quality
// events, and AUTH-01d authz decisions into a queryable, tamper-evident store.
//
// The envelope's lineage fields (event_id / correlation_id / causation_id) are
// the spine — every event becomes a Record keyed by them, so the causal chain
// (AUDIT-01c) is reconstructable and the hash chain (AUDIT-01b) is well-ordered.
// Recognized payloads are enriched into a human Summary + structured Attributes;
// everything else is still recorded (audit completeness) with an envelope-level
// summary.
package audit

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

// Kind is the audit classification of a record — what the event IS to an
// auditor, derived from its envelope + payload.
type Kind string

const (
	KindAuthzDecision  Kind = "authz_decision"  // AUTH-01d DecisionLog
	KindDecision       Kind = "decision"        // model/strategy DecisionLog
	KindCommandOutcome Kind = "command_outcome" // command.v1.CommandOutcome FACT
	KindCommand        Kind = "command"         // a COMMAND-class event
	KindDataQuality    Kind = "data_quality"    // observation.v1.DataQualityEvent
	KindEvent          Kind = "event"           // recorded but unrecognized (generic)
)

// Record is one immutable entry in the audit log. Seq, PrevHash, and Hash are
// assigned by the store on append (the chain, AUDIT-01b); everything else is
// projected from the source event.
type Record struct {
	Seq           int64             `json:"seq"`
	EventID       string            `json:"event_id"`
	CorrelationID string            `json:"correlation_id"`
	CausationID   string            `json:"causation_id,omitempty"`
	Domain        string            `json:"domain"`
	EventType     string            `json:"event_type"`
	EventClass    string            `json:"event_class"`
	TenantID      string            `json:"tenant_id,omitempty"`
	Source        string            `json:"source"`
	OccurredAt    time.Time         `json:"occurred_at"`
	RecordedAt    time.Time         `json:"recorded_at"`
	Kind          Kind              `json:"kind"`
	Summary       string            `json:"summary"`
	Attributes    map[string]string `json:"attributes,omitempty"`
	SchemaRef     string            `json:"schema_ref,omitempty"`

	PrevHashV string `json:"prev_hash"`
	HashV     string `json:"hash"`
}

// --- chain.Link ---

func (r *Record) PrevHash() string { return r.PrevHashV }
func (r *Record) Hash() string     { return r.HashV }

// Canonical is the deterministic byte serialization the hash chain signs. It
// covers every projected field (Seq binds position) but NOT the hashes
// themselves — Hash is the output, PrevHash is folded in by chain.Next. Map keys
// are sorted and times are RFC3339Nano UTC so the encoding is stable across
// runs, machines, and Go versions.
func (r *Record) Canonical() []byte {
	var b strings.Builder
	w := func(k, v string) { b.WriteString(k); b.WriteByte('='); b.WriteString(v); b.WriteByte('\n') }
	w("seq", strconv.FormatInt(r.Seq, 10))
	w("event_id", r.EventID)
	w("correlation_id", r.CorrelationID)
	w("causation_id", r.CausationID)
	w("domain", r.Domain)
	w("event_type", r.EventType)
	w("event_class", r.EventClass)
	w("tenant_id", r.TenantID)
	w("source", r.Source)
	w("occurred_at", r.OccurredAt.UTC().Format(time.RFC3339Nano))
	w("recorded_at", r.RecordedAt.UTC().Format(time.RFC3339Nano))
	w("kind", string(r.Kind))
	w("summary", r.Summary)
	w("schema_ref", r.SchemaRef)
	keys := make([]string, 0, len(r.Attributes))
	for k := range r.Attributes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		w("attr."+k, r.Attributes[k])
	}
	return []byte(b.String())
}
