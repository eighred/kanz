// kanz-altevent is the operator's tool for publishing a commitment-lifecycle
// event into the alternatives (private-markets) journal (ALT-01b). It is the
// ONLY thing on this platform that publishes one.
//
// # Why this exists
//
// The alternatives service folds alternatives.commitment.* FACTs into a durable
// Position (committed / called / distributed / NAV, and the dated cashflow
// series IRR/TVPI are computed from). The consumer, the decoder and the
// provisioned journal stream all exist — and NOTHING PUBLISHED TO IT. Capital
// calls, distributions and NAV marks are not computed anywhere in this
// platform: they arrive as GP and fund-administrator notices from OUTSIDE it.
// Nothing in kanz could publish them, so the fold was always fed an empty
// stream. Same situation cmd/kanz-mandate was written for; same answer.
//
// # Why one flag selects both the subject and the message type
//
// -kind is commit|call|distribution|navmark. It is not a convenience: the
// subject a commitment-lifecycle consumer subscribes on determines which
// alternatives.v1 message the wire bytes decode as (proto carries no self-
// describing type), so publishing the wrong pairing would desync every
// consumer that boots against this subject from what is actually on it.
//
// # Why EventTime is the notice's own date, never time.Now()
//
// These are dated business events, and IRR/TVPI are computed from those
// dates — a capital call dated the day the GP drew it, a distribution dated
// the day it was paid, a NAV mark dated its valuation date (as_of), which is
// deliberately NOT the day the administrator's restated letter arrives.
// Stamping ingest time would silently corrupt every return computed
// downstream: the fold would still succeed and the IRR would still be a
// number, just the WRONG one, for a class of error nobody would trace back to
// "the CLI stamped now() instead of the notice's date."
//
// # Why validation happens before ANY network I/O
//
// Unlike compliance's compacted MANDATE stream, the ALTERNATIVES stream this
// publishes onto is APPEND-ONLY (see infra/nats/bootstrap-job.yaml): every
// message this tool publishes stays in the journal forever. There is no
// "just republish a good one and the bad one falls off" recovery — a wrong
// event sits in the fold until somebody publishes an explicit correction
// (and for a NAV mark, a corrected restatement needs its OWN mark_id, or the
// fold's idempotency key treats it as a redelivery of the original and
// silently drops it). So every field this tool cannot verify is a refusal,
// not a best-effort publish.
//
//	kanz-altevent -kind call -tenant acme -file call.json \
//	              -by operator:akif -reason "GP notice 2026-06"
//
// The file is the protojson form of the alternatives.v1 message -kind selects
// (Commitment, CapitalCall, Distribution, or NAVMark).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	altpb "github.com/eighred/kanz/kanz-schemas-go/alternatives/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	alt "github.com/eighred/kanz/internal/alternatives"
	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/transport"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "kanz-altevent: "+err.Error())
		os.Exit(1)
	}
}

type options struct {
	natsURL      string
	spiffeSocket string
	tenant       string
	kind         string
	file         string
	by           string
	reason       string
	dryRun       bool
}

func run(args []string, out *os.File) error {
	opt, err := parseFlags(args)
	if err != nil {
		return err
	}
	ev, err := loadEvent(opt.kind, opt.file)
	if err != nil {
		return err
	}
	// The provenance -by/-reason already required by parseFlags, carried onto
	// the payload itself (alternatives.v1.*.recorded_by / .reason) — not just
	// printed to stdout. Without this, the operator sees "PUBLISHED — by
	// operator:akif" and reasonably believes an audit trail exists on the
	// FACT; before this call it did not.
	setProvenance(ev.payload, opt.by, opt.reason)

	fmt.Fprintf(out, "%s %s — commitment %s, dated %s\n",
		opt.kind, ev.eventID, ev.commitmentID, ev.eventTime.Format(time.RFC3339))
	fmt.Fprintf(out, "subject: %s\n", ev.subject)
	if opt.dryRun {
		// Marshalled, not just the subject: this tool's whole job is
		// publishing something that, once on the append-only journal, cannot
		// be withdrawn (see the package doc's "Why validation happens before
		// ANY network I/O") — so before this becomes unrecoverable state, a
		// human must be able to see exactly what recordedBy/reason (and every
		// other field) will be on the wire, protojson-rendered the same way
		// the fold's decoder sees it.
		body, err := protojson.MarshalOptions{Multiline: true, Indent: "  "}.Marshal(ev.payload)
		if err != nil {
			return fmt.Errorf("marshal %s for dry-run: %w", opt.kind, err)
		}
		fmt.Fprintln(out, "payload:")
		fmt.Fprintln(out, string(body))
		fmt.Fprintln(out, "--dry-run: nothing published.")
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// SEC-M3c: the operator plane's identity, exactly as kanz-mandate and
	// kanz-halt dial it. The production broker requires a client SVID —
	// without one this dial is refused and no lifecycle event can be
	// published at all.
	mesh, err := transport.NewMesh(ctx, opt.spiffeSocket)
	if err != nil {
		return fmt.Errorf("spiffe identity (%s): %w", opt.spiffeSocket, err)
	}
	defer func() { _ = mesh.Close() }()

	client, err := bus.DialNATS(ctx, bus.NATSConfig{
		URL: opt.natsURL, Name: "kanz-altevent", MaxReconnects: 1, TLSConfig: mesh.Client,
	})
	if err != nil {
		return fmt.Errorf("dial %s: %w", opt.natsURL, err)
	}
	defer func() { _ = client.Close() }()

	producer, err := bus.NewProducer(client, bus.ProducerConfig{
		Source:          "kanz-altevent",
		ProducerVersion: "1",
		Tenant:          opt.tenant,
	})
	if err != nil {
		return err
	}

	if err := producer.Publish(ctx, bus.Event{
		Subject:       ev.subject,
		EventType:     ev.subject,
		EventClass:    envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion: 1,
		Domain:        alt.Domain,
		// The event's OWN dated timestamp (committed_date / call_date /
		// distribution_date / as_of) — see the package doc's "Why EventTime"
		// section. NEVER time.Now(): IRR/TVPI are computed from this date, so
		// stamping ingest time would corrupt the return silently rather than
		// failing loudly.
		EventTime: ev.eventTime,
		// The commitment_id, for every kind: it is what orders a capital call
		// against the distribution and NAV mark that follow it. Keying on the
		// event's own id instead would let the bus interleave one
		// commitment's events with another's, and the fold has no way to
		// detect that after the fact.
		PartitionKey:     ev.commitmentID,
		TenantID:         opt.tenant,
		PayloadSchemaRef: ev.payloadSchemaRef,
		Payload:          ev.payload,
	}); err != nil {
		return fmt.Errorf("publish: %w", err)
	}

	fmt.Fprintf(out, "PUBLISHED — %s %s on commitment %s, by %s: %s\n",
		opt.kind, ev.eventID, ev.commitmentID, opt.by, opt.reason)
	fmt.Fprintln(out, "Every alternatives consumer folds it into the fund position — including one that boots tomorrow.")
	return nil
}

func parseFlags(args []string) (options, error) {
	fs := flag.NewFlagSet("kanz-altevent", flag.ContinueOnError)
	var opt options
	fs.StringVar(&opt.natsURL, "nats", envOr("KANZ_NATS_URL", "nats://localhost:4222"), "NATS URL of the spine")
	fs.StringVar(&opt.spiffeSocket, "spiffe-socket", envOr("SPIFFE_ENDPOINT_SOCKET", ""),
		"SPIFFE Workload API socket for the operator SVID the production broker requires.\n"+
			"Empty ⇒ a PLAINTEXT dial: fine against a local dev broker, refused by production.")
	fs.StringVar(&opt.tenant, "tenant", envOr("KANZ_TENANT", ""), "envelope tenant_id — REQUIRED (the bus rejects an untenanted envelope)")
	fs.StringVar(&opt.kind, "kind", "", "event kind: commit|call|distribution|navmark — REQUIRED, selects both the subject and the proto message")
	fs.StringVar(&opt.file, "file", "", "path to the event, as protojson (the alternatives.v1 message -kind selects) — REQUIRED")
	fs.StringVar(&opt.by, "by", "", `operator principal, "{type}:{id}" (e.g. operator:akif) — REQUIRED`)
	fs.StringVar(&opt.reason, "reason", "", "why this event is being published, e.g. the GP/administrator notice it transcribes — REQUIRED")
	fs.BoolVar(&opt.dryRun, "dry-run", false, "validate and print, publish nothing")
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}

	// A commitment-lifecycle event enters the append-only journal forever, so
	// it is attributed, or it does not happen — same stance as kanz-mandate.
	switch {
	case opt.tenant == "":
		return options{}, errors.New("--tenant is required: the bus rejects an envelope with no tenant_id")
	case opt.file == "":
		return options{}, errors.New("--file is required: the event to publish, protojson matching -kind")
	case opt.by == "":
		return options{}, errors.New("--by is required: a lifecycle event is attributed to a named human, or it is not published")
	case opt.reason == "":
		return options{}, errors.New("--reason is required: an unexplained addition to an append-only journal is not auditable")
	}
	// The kind is checked here — a usage error, before the file is even
	// opened, let alone any network I/O — because an unknown kind cannot be
	// mapped to a subject or a proto message; there is nothing to decode.
	switch opt.kind {
	case "commit", "call", "distribution", "navmark":
	default:
		return options{}, fmt.Errorf(
			"--kind %q is not one of commit|call|distribution|navmark: the kind selects both the subject "+
				"and the proto message, so an unrecognized one cannot be guessed at", opt.kind)
	}
	return opt, nil
}

// loadedEvent is the validated, kind-agnostic shape run() needs to publish:
// enough to build the envelope without run() knowing which proto message
// backs it.
type loadedEvent struct {
	subject          string
	payloadSchemaRef string
	payload          proto.Message
	eventID          string
	commitmentID     string
	eventTime        time.Time
}

// loadEvent reads the file as the alternatives.v1 message -kind selects and
// validates it BEFORE anything is published. It validates here — not in
// run(), not on the consumer — because this is an append-only journal (see
// the package doc): a message that reaches the wire stays there, so every
// field the fold depends on is checked before the network is even touched,
// exactly mirroring the consumer's own refusals in
// services/alternatives/internal/consume/proto.go (the two must agree on
// every field, or an event this tool accepts is one the consumer rejects at
// delivery, at which point it DLQs rather than "just not publishing").
func loadEvent(kind, path string) (*loadedEvent, error) {
	b, err := os.ReadFile(path) //#nosec G304 -- an operator-supplied path, by design
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var (
		subject   string
		schemaRef string
		payload   proto.Message
		id        string
		commitID  string
		amt       *commonpb.Decimal
		at        *timestamppb.Timestamp
	)
	switch kind {
	case "commit":
		var m altpb.Commitment
		if err := protojson.Unmarshal(b, &m); err != nil {
			return nil, fmt.Errorf("%s is not an alternatives.v1.Commitment: %w", path, err)
		}
		// A Commitment has no separate lifecycle id of its own — the
		// commitment_id IS the event's identity, exactly as the consumer's
		// decoder treats it (proto.go: event(m.GetCommitmentId(), m.GetCommitmentId(), ...)).
		subject, schemaRef, payload = alt.SubjectCommitted, "alternatives.v1.Commitment:1", &m
		id, commitID, amt, at = m.GetCommitmentId(), m.GetCommitmentId(), m.GetCommittedAmount(), m.GetCommitmentDate()
	case "call":
		var m altpb.CapitalCall
		if err := protojson.Unmarshal(b, &m); err != nil {
			return nil, fmt.Errorf("%s is not an alternatives.v1.CapitalCall: %w", path, err)
		}
		subject, schemaRef, payload = alt.SubjectCalled, "alternatives.v1.CapitalCall:1", &m
		id, commitID, amt, at = m.GetCallId(), m.GetCommitmentId(), m.GetAmount(), m.GetCallDate()
	case "distribution":
		var m altpb.Distribution
		if err := protojson.Unmarshal(b, &m); err != nil {
			return nil, fmt.Errorf("%s is not an alternatives.v1.Distribution: %w", path, err)
		}
		subject, schemaRef, payload = alt.SubjectDistributed, "alternatives.v1.Distribution:1", &m
		id, commitID, amt, at = m.GetDistributionId(), m.GetCommitmentId(), m.GetAmount(), m.GetDistributionDate()
	case "navmark":
		var m altpb.NAVMark
		if err := protojson.Unmarshal(b, &m); err != nil {
			return nil, fmt.Errorf("%s is not an alternatives.v1.NAVMark: %w", path, err)
		}
		// mark_id is required, not merely present-if-set: without it, a GP's
		// restatement of the same as_of date (routine — quarters get
		// restated) is indistinguishable from a redelivery of the original
		// mark, and the fold's idempotency key (tenant_id, event_id) DO
		// NOTHING silently drops the correction instead of applying it.
		subject, schemaRef, payload = alt.SubjectMarked, "alternatives.v1.NAVMark:1", &m
		id, commitID, amt, at = m.GetMarkId(), m.GetCommitmentId(), m.GetNav(), m.GetAsOf()
	default:
		// Unreachable: parseFlags already rejected any other -kind.
		return nil, fmt.Errorf("kanz-altevent: internal error: unhandled kind %q", kind)
	}

	if id == "" {
		return nil, fmt.Errorf("%s: the event id is required — for -kind %s this is the fold's idempotency "+
			"key, and an empty one is indistinguishable from every other empty one", path, kind)
	}
	if commitID == "" {
		return nil, fmt.Errorf("%s: commitment_id is required — every commitment-lifecycle event names the "+
			"commitment it belongs to, and this one names none", path)
	}
	if amt == nil {
		return nil, fmt.Errorf("%s: the amount is required — publishing a %s with no amount would enter the "+
			"append-only journal as a cashflow of nothing, and there is no way to withdraw it, only to correct it later", path, kind)
	}
	// FromProtoChecked, not FromProto: this file came from an operator
	// transcribing a GP/administrator notice, not from code this platform
	// controls. An exponent outside the representable range is not a small
	// number to round away — coercing it to zero would silently record a
	// capital movement of nothing and report success, in a stream nobody can
	// take it back out of.
	if _, ok := dec.FromProtoChecked(amt); !ok {
		return nil, fmt.Errorf("%s: the amount cannot be represented exactly on this platform; refusing rather "+
			"than rounding it into the journal", path)
	}
	if at == nil || !at.IsValid() {
		return nil, fmt.Errorf("%s: the event date is required — IRR/TVPI are computed from this date, and "+
			"there is no safe default to substitute for a business event with none", path)
	}

	return &loadedEvent{
		subject:          subject,
		payloadSchemaRef: schemaRef,
		payload:          payload,
		eventID:          id,
		commitmentID:     commitID,
		eventTime:        at.AsTime().UTC(),
	}, nil
}

// setProvenance stamps the operator-supplied -by/-reason onto the loaded
// payload's recorded_by/reason fields. It exists because -by and -reason are
// REQUIRED flags (see parseFlags) yet, before this function was introduced,
// were used only to format the stdout success line — nothing on the
// published FACT recorded who transcribed the event or why. recorded_by is
// deliberately not named changed_by (contrast lifecycle.v1.ConfigChanged):
// this is not a change to a platform config, it is the record of an event
// that happened OUTSIDE this platform (a GP notice, a fund-administrator
// NAV statement) and was transcribed into it.
func setProvenance(payload proto.Message, by, reason string) {
	switch m := payload.(type) {
	case *altpb.Commitment:
		m.RecordedBy, m.Reason = by, reason
	case *altpb.CapitalCall:
		m.RecordedBy, m.Reason = by, reason
	case *altpb.Distribution:
		m.RecordedBy, m.Reason = by, reason
	case *altpb.NAVMark:
		m.RecordedBy, m.Reason = by, reason
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
