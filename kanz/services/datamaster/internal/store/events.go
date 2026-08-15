package store

import (
	"fmt"
	"math/big"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	masterpb "github.com/eighred/kanz/kanz-schemas-go/master/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/datamaster/internal/pricing"
)

// THE OVERRIDE FACT (#410).
//
// #410's acceptance is explicit: the approval is recorded AS A FACT carrying
// both identities. Until this existed the override was durable in
// exception_overrides and INVISIBLE to services/audit, which projects bus FACTs
// into the signed, tamper-evident store an examiner actually reads. "Durable in
// a table the audit service cannot see" is not an audit trail.
//
// It is emitted through the transactional outbox rather than published inline —
// see 0005_outbox.sql for why both alternatives are wrong.

const (
	// Domain is this service's event domain.
	Domain = "data"

	// SubjectExceptionOverridden carries the override FACT.
	//
	// UNDER data.> DELIBERATELY, not a new master.> stream. infra/nats's DATA
	// stream already exists for "FACT-grade data-quality signals" with 168h
	// retention, and a pricing-oversight override is exactly one. A subject
	// belongs to exactly ONE stream in JetStream, so inventing master.> would
	// mean a new stream, a new retention decision and a new consumer binding to
	// carry a message the existing stream was described for.
	//
	// An unbound subject is not a soft failure: a JetStream publish to one is a
	// HARD ERROR, so this name is checked against the production bootstrap by
	// test/arch's TestEverySubjectIsCarriedByAStream rather than trusted.
	SubjectExceptionOverridden = "data.exception.overridden"

	// EventTypeExceptionOverridden names the event.
	EventTypeExceptionOverridden = "data.exception.overridden"

	// schemaVersion is the envelope's schema_version for this domain's events.
	schemaVersion = 1
)

// overrideEvent builds the FACT for one applied override.
//
// PARTITIONED BY EXCEPTION ID, so the overrides of one exception are ordered
// with respect to each other. Ordering ACROSS exceptions carries no meaning and
// partitioning by anything coarser would serialise unrelated decisions.
func overrideEvent(ex pricing.Exception, o pricing.Override) (bus.Event, error) {
	payload := &masterpb.ExceptionOverridden{
		ExceptionId:  ex.ID,
		InstrumentId: ex.InstrumentID,
		Status:       statusProto(ex.Status),
		Override: &masterpb.Override{
			Actor: o.Actor,
			// BOTH IDENTITIES. Empty approver is a single-signed override and is
			// carried as such — the FACT must be able to say "one person did
			// this", or an auditor reading the bus cannot tell the two apart.
			Approver:    o.Approver,
			Reason:      o.Reason,
			ChosenPrice: priceProto(o.ChosenPrice),
			At:          timestamppb.New(o.At.UTC()),
		},
	}
	// A ZERO TIMESTAMP IS REFUSED, NOT STAMPED WITH now(). On the wire it renders
	// as the Unix epoch and reads as a real decision made in 1970; substituting
	// now() would instead assert that the decision happened at publish time,
	// which is a different lie. Neither belongs in an audit FACT.
	if o.At.IsZero() {
		return bus.Event{}, fmt.Errorf("store: override on %s carries no timestamp", ex.ID)
	}
	if payload.GetOverride().GetChosenPrice() == nil && o.ChosenPrice != nil {
		// The price did not survive conversion to the platform's fixed scale.
		// Refusing here fails the OVERRIDE, which is correct: a FACT announcing a
		// price nobody chose is worse than no FACT, and the handler already
		// refuses over-precise prices, so reaching this means the two disagree.
		return bus.Event{}, fmt.Errorf("store: override price %s cannot be represented exactly on the wire",
			o.ChosenPrice.RatString())
	}
	return bus.Event{
		Subject:          SubjectExceptionOverridden,
		EventType:        EventTypeExceptionOverridden,
		EventClass:       envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion:    schemaVersion,
		Domain:           Domain,
		EventTime:        o.At.UTC(),
		PartitionKey:     ex.ID,
		PayloadSchemaRef: payloadSchemaRef(payload, schemaVersion),
		Payload:          payload,
	}, nil
}

func payloadSchemaRef(payload proto.Message, version int32) string {
	return fmt.Sprintf("%s:%d", payload.ProtoReflect().Descriptor().FullName(), version)
}

// priceProto converts an exact price, returning nil for a nil price.
//
// It uses the ASSERTING conversion, not the wrapping one: the handler already
// refuses a price that does not survive the platform's fixed scale, so a value
// that fails here means the two checks disagree, and silently rounding it would
// put a number nobody chose into an audit FACT (#94/#189).
func priceProto(r *big.Rat) *commonpb.Decimal {
	if r == nil {
		return nil
	}
	p := dec.ToProto(r)
	if dec.FromProto(p).Cmp(r) != 0 {
		return nil
	}
	return p
}

func statusProto(s pricing.Status) masterpb.ExceptionStatus {
	switch s {
	case pricing.StatusOpen:
		return masterpb.ExceptionStatus_EXCEPTION_STATUS_OPEN
	case pricing.StatusOverridden:
		return masterpb.ExceptionStatus_EXCEPTION_STATUS_OVERRIDDEN
	case pricing.StatusResolved:
		return masterpb.ExceptionStatus_EXCEPTION_STATUS_RESOLVED
	default:
		// UNSPECIFIED rather than a guess. A status this code does not know is a
		// status it must not assert, and the enum's zero value says exactly that.
		return masterpb.ExceptionStatus_EXCEPTION_STATUS_UNSPECIFIED
	}
}
