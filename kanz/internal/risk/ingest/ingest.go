// Package ingest is the risk module's bus→engine bridge. It takes
// bus deliveries (after the bus.Consumer has already validated the
// envelope per EVT-17), discriminates by event_type, unmarshals the
// payload into the correct kanz-schemas domain.v1 proto, and
// dispatches to the RISK-05 Applier.
//
// # Scope split with RISK-05
//
// RISK-04 owns the *routing* — "this event_type ⇒ that proto type ⇒
// that Applier method." It is deliberately thin: no domain
// translation, no idempotency, no ordering. Those are RISK-05's job
// because they need the engine's state and the per-aggregate lock
// the Applier owns; doing them here would split state across two
// packages and make the per-aggregate guarantee impossible to enforce.
//
// # Why Applier takes proto types directly
//
// Translating proto → domain types (RISK-03) here would force the
// Applier to either accept domain types (losing access to envelope
// metadata needed for dedup / ordering) or to receive both. The
// simpler boundary is "Ingestor parses the proto, Applier owns the
// state transition" — Applier sees envelope + proto, decides what to
// do, mutates domain types it already owns.
//
// # Live-only consumer
//
// The risk engine is a live consumer — REPLAYED-flagged events are
// already rejected by bus.Validate before reaching Handler (EVT-20c).
// Any future replay-scoped state consumer would construct a separate
// Ingestor wired to a Consumer configured with
// bus.WithValidator(bus.ValidateReplay).
package ingest

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	"github.com/eighred/kanz/internal/dec"
)

// Event-type names follow kanz-schemas/README.md § Subject Taxonomy §1
// ({domain}.{entity}.{event_type}) and the per-payload doc comments in
// kanz-schemas/proto/domain/v1/portfolio.proto. They are the values
// the engine's orchestrator subscribes to and the values RISK-10 will
// publish under.
const (
	// EventTypePortfolioRevalued carries domain.v1.PortfolioState.
	EventTypePortfolioRevalued = "risk.portfolio.revalued"
	// EventTypePositionChanged carries domain.v1.PositionState.
	EventTypePositionChanged = "risk.position.changed"
	// EventTypePortfolioSnapshot carries domain.v1.PortfolioSnapshot
	// (event class STATE_SNAPSHOT).
	EventTypePortfolioSnapshot = "risk.portfolio.snapshot"
)

// Applier is the RISK-05 boundary. Ingestor depends on this
// interface — not on a concrete state package — so RISK-04 lands
// before RISK-05 and tests can drop in a fake. The methods receive
// the envelope (needed for dedup_key / event_id / partition_key /
// as_of) plus the typed payload; the Applier owns the proto→domain
// translation and per-aggregate state mutation.
type Applier interface {
	// ApplyPortfolioRevalued applies a portfolio-level state update.
	ApplyPortfolioRevalued(ctx context.Context, env *envelopepb.Envelope, p *domainpb.PortfolioState) error
	// ApplyPositionChanged applies a position-level state update.
	ApplyPositionChanged(ctx context.Context, env *envelopepb.Envelope, p *domainpb.PositionState) error
	// ApplyPortfolioSnapshot applies a full materialized snapshot; see
	// kanz-schemas/README.md § Event Class Rules §3 (snapshot then log-resume from
	// the snapshot's log_position).
	ApplyPortfolioSnapshot(ctx context.Context, env *envelopepb.Envelope, p *domainpb.PortfolioSnapshot) error
}

// Ingestor is the bus.EventHandler implementation that fans
// deliveries out by event_type to the appropriate Applier method.
// Construct it once per engine and pass Handler to as many
// bus.Consumer.Subscribe calls as needed — one per state subject.
type Ingestor struct {
	applier Applier
}

// NewIngestor returns an Ingestor that dispatches to applier.
func NewIngestor(applier Applier) (*Ingestor, error) {
	if applier == nil {
		return nil, errors.New("ingest: applier is nil")
	}
	return &Ingestor{applier: applier}, nil
}

// Handler is the bus.EventHandler value the orchestrator wires into
// bus.Consumer.Subscribe. Returning an error nacks (NATS) /
// skip-commits (Kafka) the delivery via bus.Consumer's retry+DLQ
// path (EVT-17e); a nil return acks.
func (i *Ingestor) Handler(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
	switch env.EventType {
	case EventTypePortfolioRevalued:
		var p domainpb.PortfolioState
		if err := proto.Unmarshal(payload, &p); err != nil {
			return fmt.Errorf("ingest: %s unmarshal: %w", env.EventType, err)
		}
		if field, in := dec.InDomainDeep(&p); !in {
			return fmt.Errorf("ingest: %s carries an out-of-domain exponent at %s", env.EventType, field)
		}
		return i.applier.ApplyPortfolioRevalued(ctx, env, &p)

	case EventTypePositionChanged:
		var p domainpb.PositionState
		if err := proto.Unmarshal(payload, &p); err != nil {
			return fmt.Errorf("ingest: %s unmarshal: %w", env.EventType, err)
		}
		if field, in := dec.InDomainDeep(&p); !in {
			return fmt.Errorf("ingest: %s carries an out-of-domain exponent at %s", env.EventType, field)
		}
		return i.applier.ApplyPositionChanged(ctx, env, &p)

	case EventTypePortfolioSnapshot:
		var p domainpb.PortfolioSnapshot
		if err := proto.Unmarshal(payload, &p); err != nil {
			return fmt.Errorf("ingest: %s unmarshal: %w", env.EventType, err)
		}
		if field, in := dec.InDomainDeep(&p); !in {
			return fmt.Errorf("ingest: %s carries an out-of-domain exponent at %s", env.EventType, field)
		}
		return i.applier.ApplyPortfolioSnapshot(ctx, env, &p)

	default:
		// Subscription scope matched but event_type is not one this
		// Ingestor knows. Surface as an error so the bus retry+DLQ
		// path picks it up — an unknown event_type on a state subject
		// is misconfiguration the operator must see, not silent drop.
		return fmt.Errorf("%w: %q", ErrUnknownEventType, env.EventType)
	}
}

// ErrUnknownEventType is returned when Handler receives an event_type
// it does not dispatch on. Wrapped via fmt.Errorf so callers can
// errors.Is(err, ErrUnknownEventType).
var ErrUnknownEventType = errors.New("ingest: unknown event_type on state subject")
