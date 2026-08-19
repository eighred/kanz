package registry

import "context"

// Op is the kind of registry mutation mirrored onto the platform.model log.
type Op int

const (
	OpUnspecified Op = iota
	OpRegister       // a Register(meta, role)
	OpValidate       // a RecordValidation(id, validation)
	// OpUnregister is a Unregister(id) — the model leaves the serving set.
	//
	// It was ABSENT until #112 while inference.v1.ModelRegistryEvent has always
	// carried MODEL_REGISTRY_OP_UNREGISTER, so a Go follower had no fold for a
	// retirement the registrar published. The consequence of the gap was not a
	// missing feature: it was a registry that kept naming a WITHDRAWN model as
	// primary for a contract, indefinitely.
	OpUnregister
)

// Event is one registry mutation as it rides the platform.model append log. It
// carries Metadata + role + validation — never the live Model (not
// serializable); apply rematerializes via the Registry's ModelLoader. Origin is
// the producing replica's id, so a replica can skip re-applying its own live
// echoes (loop prevention).
type Event struct {
	Origin     string
	Op         Op
	Metadata   Metadata
	Role       Role
	Validation Validation
}

// Log is the platform.model append-log seam (DEBT-02b). Publish appends a
// mutation; Replay walks the log from offset 0 and calls apply for each event
// (the rebuild path — the topic is the source of truth, the local Registry the
// materialized cache). The in-memory default is per-process; the concrete
// NATS/Kafka binding onto the platform.model topic — keyed by model_id so a
// model's validate precedes its primary-register in per-partition order (the
// MLOPS-01a gate holds identically on every replica) — wires at the composition
// root. BusFollower in this package IS that binding, and risk-engine is the
// composition root that constructs it (#112).
//
// REPLAY DOES NOT NECESSARILY END. The in-memory default walks a finite slice and
// returns; the bus binding folds the retained log and then KEEPS FOLDING as
// entries arrive, because a replica that stopped at the last entry present when
// it started would be correct for exactly one instant. Rebuild therefore blocks
// for the life of the process on that implementation — BusFollower.Replay
// documents the armed signal a caller reads instead of waiting on the return.
type Log interface {
	Publish(ctx context.Context, e Event) error
	Replay(ctx context.Context, apply func(Event) error) error
}

// CoordinatedRegistry is a Registry whose mutations are mirrored onto the log so
// N replicas converge (DEBT-02b). Local Register/RecordValidation apply to the
// embedded Registry and publish the event; Apply folds an inbound peer event
// (skipping own origin); Rebuild replays the whole log at startup.
type CoordinatedRegistry struct {
	*Registry
	log    Log
	origin string
}

// NewCoordinated wraps reg with cross-replica coordination over log. origin is
// this replica's unique id (used to skip its own live echoes).
func NewCoordinated(reg *Registry, log Log, origin string) *CoordinatedRegistry {
	return &CoordinatedRegistry{Registry: reg, log: log, origin: origin}
}

// Register applies locally (enforcing the MLOPS-01a PRIMARY gate) and, on
// success, publishes the mutation so peers converge. A gate failure is not
// published — a rejected promotion never propagates.
func (c *CoordinatedRegistry) Register(ctx context.Context, meta Metadata, role Role) error {
	if err := c.Registry.Register(meta, role); err != nil {
		return err
	}
	return c.log.Publish(ctx, Event{Origin: c.origin, Op: OpRegister, Metadata: meta, Role: role})
}

// Unregister removes the model locally and publishes the retirement so peers
// stop serving it too. Unlike Register there is no gate to fail, so the publish
// is unconditional — a retirement that reached one replica and not the others is
// the worst of the three possible states.
func (c *CoordinatedRegistry) Unregister(ctx context.Context, modelID string) error {
	c.Registry.Unregister(modelID)
	return c.log.Publish(ctx, Event{Origin: c.origin, Op: OpUnregister, Metadata: Metadata{ModelID: modelID}})
}

// RecordValidation applies locally and publishes so the gate outcome reaches
// every replica before the corresponding PRIMARY register (log keyed by model_id
// preserves that order).
func (c *CoordinatedRegistry) RecordValidation(ctx context.Context, modelID string, v Validation) error {
	if err := c.Registry.RecordValidation(modelID, v); err != nil {
		return err
	}
	return c.log.Publish(ctx, Event{Origin: c.origin, Op: OpValidate, Metadata: Metadata{ModelID: modelID}, Validation: v})
}

// Apply folds an inbound platform.model event into the local registry. Events
// this replica produced (matching origin) are skipped — they were already
// applied locally, and re-applying would be a redundant echo. The composition
// root's bus consumer calls this for each delivered event.
func (c *CoordinatedRegistry) Apply(e Event) error {
	if e.Origin == c.origin {
		return nil // own echo — already applied locally
	}
	return c.applyEvent(e)
}

// Rebuild replays the platform.model log from offset 0 to reconstruct the local
// registry at startup (the topic is the source of truth). Own-origin events ARE
// applied here — a restarted replica must rebuild everything, including what a
// prior incarnation of itself wrote.
func (c *CoordinatedRegistry) Rebuild(ctx context.Context) error {
	return c.log.Replay(ctx, c.applyEvent)
}

func (c *CoordinatedRegistry) applyEvent(e Event) error {
	switch e.Op {
	case OpRegister:
		return c.Registry.Register(e.Metadata, e.Role)
	case OpValidate:
		return c.Registry.RecordValidation(e.Metadata.ModelID, e.Validation)
	case OpUnregister:
		c.Registry.Unregister(e.Metadata.ModelID)
		return nil
	default:
		return nil
	}
}
