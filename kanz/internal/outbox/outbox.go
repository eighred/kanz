// Package outbox makes "the row is committed and the FACT was never published"
// structurally impossible for the OMS (#292).
//
// # The defect this replaces
//
// Every state change in this service was TWO INDEPENDENT WRITES: a store write,
// then a publish. Between them the process can die, the broker can refuse, the
// network can drop — and the two halves disagree with nobody to notice. The
// platform's answer, three times over, was a hand-rolled compensator: a
// `*_announced_at` marker on the order plus a recovery branch that republishes
// (order/v1/order_events.proto fields 18, 19, 20 and #238's periodic sweep).
// Those work. What they do not do is generalize: each new FACT on a transition
// needs a new marker, a new recovery branch and a new test, and NOTHING forces
// that — which is exactly how accepted_announced_at came to be missing while its
// two siblings existed.
//
// An outbox row is written IN THE SAME TRANSACTION as the state change. Either
// both land or neither does. A relay then drains the table and publishes. The
// failure mode moves from "a FACT is lost and a compensator has to notice" to "a
// FACT is late and a queue depth says so".
//
// # Where this lives, and why it is not in pkg/
//
// CLAUDE.md: shared code starts in a service's internal/ and is promoted only
// when a SECOND consumer appears. The OMS is the first, and it is the right
// first: it holds all three markers and every commit-then-publish pair the issue
// counted. `pkg/outbox` for one consumer is the mistake this repository has a
// rule against — and the interfaces below are deliberately shaped so that
// promotion, when a second service adopts this, is a move rather than a rewrite:
// nothing here knows what an order is.
//
// # What is NOT here
//
// This is not a bus consumer. bus.MaxAttempts is 1 estate-wide and defended by
// test/arch/bus_dlq_test.go's retryCertifiedConsumers allow-list, which governs
// whether a CONSUMER may wire bus.WithRetry. The relay is a PRODUCER driven by a
// database table; it retries by re-reading a row that is still unpublished, not
// by asking the broker to redeliver. It is therefore outside that rule rather
// than an exception to it, and this paragraph exists so a reader does not have
// to work that out.
package outbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/observability"
)

// Record is one FACT, captured at the moment its state change was decided and
// held until the broker has it.
//
// IT STORES THE PRODUCER-FACING EVENT, NOT A STAMPED ENVELOPE. bus.Producer
// stamps event_id, publish_time, producer_sequence and source at publish time,
// and every one of those would be a lie if frozen at enqueue time: publish_time
// would predate the publish, and producer_sequence is a per-process counter that
// means nothing once a different goroutine — or a different pod — does the
// sending. So the row carries exactly the fields the CALLER knew, and the relay
// hands them back to Producer.Publish, which stamps the rest as it always has.
//
// THE LINEAGE FIELDS ARE THE EXCEPTION, AND THEY ARE WHY THIS TYPE EXISTS AT
// ALL. correlation_id, causation_id, trace_context and tenant_id are normally
// inherited from ctx — bus.Consumer stashes them off the inbound envelope so a
// derived FACT is automatically linked to the command that caused it
// (pkg/bus/producer.go stamp()). The relay publishes from a ticker, long after
// that ctx is gone. Captured here, they are handed back on the explicit
// bus.Event fields, which take precedence over ctx — so a relayed FACT carries
// the same lineage the synchronous publish would have carried. Dropping them
// would silently break causation across every FACT this service emits, and
// nothing would fail: the events would publish fine and simply stop being
// traceable to the order that produced them.
type Record struct {
	Subject          string
	EventType        string
	EventClass       envelopepb.EventClass
	SchemaVersion    uint32
	Domain           string
	EventTime        time.Time
	PartitionKey     string
	PayloadSchemaRef string

	CorrelationID string
	CausationID   string
	TraceContext  string
	// TenantID is the tenant the FACT is PUBLISHED under — the one lifted off
	// the inbound command's envelope. It is deliberately NOT the tenant the row
	// is stored under: on today's deployment the OMS serves __system__ while
	// commands carry the caller's tenant, and conflating the two would put every
	// record outside its own connection's RLS scope. See Enqueue and
	// migrations/0006_outbox.sql.
	TenantID string

	// Payload is the marshaled domain message. Bytes, not a proto.Message,
	// because this type is what gets written to a BYTEA column and read back by
	// a process that no longer holds the original.
	Payload []byte
}

// Pending is a Record still in the table, with the bookkeeping the relay needs.
type Pending struct {
	// ID is the enqueue order. See the ordering argument on Queue.
	ID       int64
	Attempts int
	Record   Record
	// LastError is what the PREVIOUS attempt on this record recorded — possibly
	// by a different replica, possibly in a previous pod's lifetime. Empty means
	// no attempt has failed yet, which is why it is only meaningful read
	// alongside Attempts: empty with Attempts>0 would mean the cause is not
	// being recorded, not that the record is healthy.
	//
	// IT IS ON THE PROJECTION BECAUSE OTHERWISE IT IS UNREACHABLE FROM GO (#817).
	// The relay stops at the head of a key by design, so one record that will not
	// publish halts every FACT behind it for that key; the age gauge says a
	// record is ageing and nothing said why. The cause was already being written
	// on every failed attempt and cleared on every success — it was simply never
	// selected, so the only route to it was a psql session against production
	// during the audit-trail outage that made it matter.
	//
	// FREE TEXT, DELIBERATELY, AND THEREFORE NEVER A METRIC LABEL. It is a
	// broker's refusal string: unbounded in shape, one distinct value per
	// failure mode per subject. As a label value it would be an unbounded
	// cardinality explosion in Prometheus during exactly the incident it was
	// added to explain. It belongs on a log line and in a read plane, not in a
	// series. See Relay.drainKey.
	LastError string
}

// causeLimit bounds a recorded cause. It is a COLUMN, not a log line: a venue
// returning an HTML error page, or a wrapped chain naming every hop, must not be
// what a row grows to.
const causeLimit = 1000

// boundedCause renders a failure cause for storage, in ONE place.
//
// Both Queue implementations record the cause and both must agree on what they
// stored, or the in-process queue certifies a length production does not have —
// the same permissive-double problem Memory's doc names for ordering, reached
// through the reporting side instead.
func boundedCause(cause error) string {
	if cause == nil {
		return ""
	}
	msg := cause.Error()
	if len(msg) > causeLimit {
		msg = msg[:causeLimit]
	}
	return msg
}

var (
	// ErrNoTenant is returned by From when the event being captured has no
	// tenant. See the comment there — this is a refusal, not a default.
	ErrNoTenant = errors.New("outbox: no tenant on the event or its context")

	// ErrUnknownPayloadType is returned when a stored record names a payload
	// message this binary does not link in. It is a head-of-line stop, not a
	// skip; see Relay.drainKey.
	ErrUnknownPayloadType = errors.New("outbox: payload_schema_ref names a message this binary does not know")
)

// From captures a bus.Event as a Record, resolving from ctx exactly what
// bus.Producer.stamp would have resolved had the event been published here and
// now.
//
// AN EMPTY TENANT IS REFUSED, LOUDLY, AT ENQUEUE. bus.Validate rejects an empty
// tenant_id on the live path, so a record captured without one is a FACT that
// can never be published — it would sit in the table failing forever, and the
// only symptom would be a queue that stopped draining. Worse, it crash-looped
// the OMS once already when a publish outside an inbound delivery had no tenant
// to stamp. Failing at the enqueue means the state change ROLLS BACK too, which
// is the correct direction: a transition whose FACT cannot be announced must not
// be committed either. That is the whole property this package exists for,
// applied to its own precondition.
//
// The producer's ProducerConfig.Tenant fallback is deliberately NOT consulted
// here: this package has no producer, and reaching for a fallback is how "the
// caller forgot" and "the caller meant the platform tenant" became the same
// observable event elsewhere in this estate.
func From(ctx context.Context, e bus.Event) (Record, error) {
	if e.Payload == nil {
		return Record{}, errors.New("outbox: bus.Event.Payload required")
	}
	if e.EventTime.IsZero() {
		return Record{}, errors.New("outbox: bus.Event.EventTime required")
	}
	if e.PartitionKey == "" {
		// Ordering is per partition key (see Queue). A record with no key
		// belongs to no ordered stream, so nothing could say where it goes
		// relative to the FACTs around it. The OMS keys every order FACT on
		// order_id; an empty one is a bug in the caller, not a case to handle.
		return Record{}, errors.New("outbox: bus.Event.PartitionKey required — " +
			"it is the key the relay preserves order within")
	}
	tenant := e.TenantID
	if tenant == "" {
		tenant = bus.TenantIDFromContext(ctx)
	}
	if tenant == "" {
		return Record{}, fmt.Errorf("%w: %s — publish inside a delivery, or set bus.WithTenantID "+
			"(cmd/oms/main.go does this for the sweep)", ErrNoTenant, e.EventType)
	}

	payload, err := proto.Marshal(e.Payload)
	if err != nil {
		return Record{}, fmt.Errorf("outbox: marshal %s payload: %w", e.EventType, err)
	}

	// Same precedence as pkg/bus/producer.go stamp(): explicit field, then ctx.
	// The root default (correlation = the event's own id) is NOT applied here —
	// it needs the event_id, which only the producer mints, so it is left to the
	// producer exactly as it is on the synchronous path.
	correlation := e.CorrelationID
	if correlation == "" {
		correlation = bus.CorrelationIDFromContext(ctx)
	}
	causation := e.CausationID
	if causation == "" {
		causation = bus.CausationIDFromContext(ctx)
	}
	trace := e.TraceContext
	if trace == "" {
		trace = observability.TraceparentFromContext(ctx)
	}
	if trace == "" {
		trace = bus.TraceContextFromContext(ctx)
	}

	return Record{
		Subject:          e.Subject,
		EventType:        e.EventType,
		EventClass:       e.EventClass,
		SchemaVersion:    e.SchemaVersion,
		Domain:           e.Domain,
		EventTime:        e.EventTime.UTC(),
		PartitionKey:     e.PartitionKey,
		PayloadSchemaRef: schemaRef(e),
		CorrelationID:    correlation,
		CausationID:      causation,
		TraceContext:     trace,
		TenantID:         tenant,
		Payload:          payload,
	}, nil
}

// schemaRef mirrors the producer's own derivation (pkg/bus/producer.go
// schemaRef) so a record captured here resolves to the byte-identical ref the
// synchronous publish would have stamped. It is also the ONLY thing that names
// the payload's concrete type, so Event() below can rebuild it — which is why an
// empty one is not tolerated.
func schemaRef(e bus.Event) string {
	if e.PayloadSchemaRef != "" {
		return e.PayloadSchemaRef
	}
	return fmt.Sprintf("%s:%d", e.Payload.ProtoReflect().Descriptor().FullName(), e.SchemaVersion)
}

// Event rebuilds the bus.Event this record was captured from, re-inflating the
// payload into its concrete message type.
//
// THE TYPE IS RESOLVED FROM payload_schema_ref, NOT FROM A TABLE IN THIS
// PACKAGE. The ref's convention is "{proto full name}:{schema_version}", and the
// generated descriptors are linked into this binary, so protoregistry already
// holds the mapping. A hand-maintained event_type→constructor table would be a
// second place that has to learn about every new FACT — the exact instalment
// pattern this package exists to end — and it would fail by SILENTLY dropping an
// unknown event rather than by refusing.
//
// An unresolvable ref returns ErrUnknownPayloadType, which the relay treats as a
// head-of-line stop for that partition key. That is deliberate: it means a
// deployment is running a relay older than the FACTs it is being asked to send,
// and publishing the events AROUND the one it cannot decode would deliver a
// consumer a history with a hole in the middle.
func (r Record) Event() (bus.Event, error) {
	name := r.PayloadSchemaRef
	if i := strings.LastIndex(name, ":"); i >= 0 {
		name = name[:i]
	}
	if name == "" {
		return bus.Event{}, fmt.Errorf("%w: record %s carries no payload_schema_ref", ErrUnknownPayloadType, r.EventType)
	}
	mt, err := protoregistry.GlobalTypes.FindMessageByName(protoreflect.FullName(name))
	if err != nil {
		return bus.Event{}, fmt.Errorf("%w: %q (%s): %v", ErrUnknownPayloadType, name, r.EventType, err)
	}
	msg := mt.New().Interface()
	if err := proto.Unmarshal(r.Payload, msg); err != nil {
		return bus.Event{}, fmt.Errorf("outbox: decode %s payload as %s: %w", r.EventType, name, err)
	}
	// EVERY LINEAGE FIELD IS SET EXPLICITLY. bus.Event's explicit fields beat
	// ctx (producer.go stamp()), and the relay's ctx belongs to the relay — it
	// carries no correlation, no causation and no tenant of its own. Leaving one
	// empty here would not error; it would publish a FACT that validates and is
	// simply detached from the command that caused it.
	return bus.Event{
		Subject:          r.Subject,
		EventType:        r.EventType,
		EventClass:       r.EventClass,
		SchemaVersion:    r.SchemaVersion,
		Domain:           r.Domain,
		EventTime:        r.EventTime,
		PartitionKey:     r.PartitionKey,
		PayloadSchemaRef: r.PayloadSchemaRef,
		CorrelationID:    r.CorrelationID,
		CausationID:      r.CausationID,
		TraceContext:     r.TraceContext,
		TenantID:         r.TenantID,
		Payload:          msg,
	}, nil
}

// Queue is the drain surface the relay needs. Postgres is the production
// implementation; Memory is the seam the in-memory order store uses, and it
// honours the SAME ordering contract for the same reason order.MemoryStore
// honours the CAS contract — a permissive double certifies behaviour production
// does not have.
//
// # THE ORDERING CONTRACT, WHICH IS THE HARD PART
//
// Consumers FOLD these FACTs. tv-sync's transition() drops an ORDER_ROUTED for
// an order its projection never admitted; the position book folds fills into a
// running quantity. A relay that published order X's ROUTED before its ACCEPTED
// would not merely be late — it would leave a projection permanently blind to a
// live order. So:
//
//	Records with the same PartitionKey are published in ID order, and one key is
//	drained by at most one relay at a time. Nothing is promised ACROSS keys.
//
// Per-key is exactly the granularity the bus already uses (partition_key is
// order_id for every OMS FACT), and it is the granularity a consumer folds at.
//
// # WHY NOT `FOR UPDATE SKIP LOCKED`
//
// It is the usual answer and it is wrong here. SKIP LOCKED hands the second
// worker the next UNLOCKED ROW — which, for a key whose earlier row is already
// locked by the first worker, is a LATER row of the SAME key. Two relays would
// then publish that key out of order, and the failure would be invisible in any
// test with one relay. LockKey below locks the KEY, not the row, so the second
// worker skips the whole key rather than jumping over its head.
//
// # WHY ID ORDER IS COMMIT ORDER HERE (and the assumption that makes it true)
//
// A sequence allocates at INSERT time, not COMMIT time, so in general id order
// and commit order can disagree: transaction A can take id 5, transaction B take
// id 6 and commit first, and a relay reading between the two commits would see 6
// alone and publish it — putting 5 behind it forever.
//
// That cannot happen for one partition key in this service, and the reason is
// worth stating because it is a CONSTRAINT, not a coincidence: every enqueue
// rides a versioned write of the same aggregate. Admission's INSERT ... ON
// CONFLICT DO NOTHING admits exactly one transaction per order_id, and every
// transition after it carries the compare-and-swap predicate on orders.version
// (#122). Two transactions for one order therefore cannot both commit — the
// loser is refused and its outbox row rolls back with it — so for a given
// partition key the committed rows are strictly sequential and id order IS
// commit order.
//
// THE CONSTRAINT: an enqueue that does NOT ride a versioned write of the
// aggregate it is keyed on would break this, silently, in the reordering
// direction. Do not add one. If a transition ever needs a FACT without a
// state write, it needs a different mechanism, not a bare Enqueue.
type Queue interface {
	// PendingKeys returns partition keys that have unpublished records, oldest
	// pending record first, capped at limit.
	PendingKeys(ctx context.Context, limit int) ([]string, error)
	// LockKey takes exclusive ownership of one key's drain across every replica.
	//
	// wait=false is TRY: ok=false means another relay holds the key, which is
	// not an error — the background pass moves on to the next key rather than
	// queueing behind a drain that is already happening.
	//
	// wait=true BLOCKS until the key is free, and is for the INLINE flush a
	// handler runs after committing a FACT. That caller cannot move on: it is
	// about to publish the next FACT for this same order directly, and doing so
	// while the enqueued one is still in the table is the reordering this whole
	// contract exists to prevent. Treating contention as a failure there would
	// nack a live order command because a background tick happened to reach the
	// same key first — a trading command sent to the DLQ by a benign race. The
	// wait is bounded by ctx, which on the handler path is the delivery's.
	LockKey(ctx context.Context, key string, wait bool) (release func(), ok bool, err error)
	// Pending returns up to limit unpublished records for one key, in ID order.
	Pending(ctx context.Context, key string, limit int) ([]Pending, error)
	// MarkPublished records that the broker has this record.
	MarkPublished(ctx context.Context, id int64) error
	// MarkFailed records an attempt that did not reach the broker. It must NOT
	// mark the record published: the relay retries it on the next pass, and the
	// attempt count is what tells an operator the difference between a blip and
	// a record that will never go out.
	//
	// THE CAUSE MUST COME BACK ON THE NEXT Pending, as Pending.LastError,
	// bounded by boundedCause. An implementation that accepts the cause and
	// drops it is write-only, and #817 is what that costs: the attempt count
	// climbs, the age gauge climbs, and the field that says why is reachable
	// only by hand-written SQL against a production database mid-incident.
	MarkFailed(ctx context.Context, id int64, cause error) error
	// OldestPendingAge reports how long the oldest unpublished record has been
	// waiting, and false when the queue is empty. It is the ONLY number that
	// distinguishes a drained outbox from a relay that stopped running — both of
	// which otherwise look like "no errors in the log".
	OldestPendingAge(ctx context.Context, now time.Time) (time.Duration, bool, error)
}
