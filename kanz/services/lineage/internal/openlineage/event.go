// Package openlineage maps Kanz envelope lineage into the OpenLineage standard
// (LIN-01a). The substrate already rides every event — correlation_id (the unit
// of work / run), causation_id (the upstream event), source (the producing job),
// payload_schema_ref (the dataset's schema). This package harvests that into
// spec-shaped RunEvents an OpenLineage backend (Marquez/DataHub) ingests, and
// the in-memory graph (package graph) answers LIN-01d "where did this come from"
// over.
//
// OpenLineage, not a bespoke format, because the catalog (LIN-01b) and most
// lineage UIs speak it natively — emitting the standard is free interoperability.
package openlineage

import (
	"context"
	"log/slog"
	"time"

	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
)

// SchemaURL pins the OpenLineage spec version these events conform to.
const SchemaURL = "https://openlineage.io/spec/2-0-2/OpenLineage.json"

// Producer identifies the emitter in every event (OpenLineage `producer`).
const Producer = "https://github.com/kanz-eng/kanz/services/lineage"

// Dataset is an OpenLineage dataset reference: a logical table in the lineage
// graph. Facets carry extra metadata (here the Kanz payload schema ref).
type Dataset struct {
	Namespace string         `json:"namespace"`
	Name      string         `json:"name"`
	Facets    map[string]any `json:"facets,omitempty"`
}

// Job is the processing step that produced an event — the emitting service.
type Job struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// Run is one execution of a job — keyed on the envelope correlation_id, so all
// events of one logical unit of work share a run.
type Run struct {
	RunID string `json:"runId"`
}

// RunEvent is the OpenLineage event: a job run consuming inputs, producing
// outputs. Kanz emits a single COMPLETE per event (each event is an atomic,
// already-finished production — there is no separate START/COMPLETE pair).
type RunEvent struct {
	EventType string    `json:"eventType"`
	EventTime time.Time `json:"eventTime"`
	Run       Run       `json:"run"`
	Job       Job       `json:"job"`
	Inputs    []Dataset `json:"inputs"`
	Outputs   []Dataset `json:"outputs"`
	Producer  string    `json:"producer"`
	SchemaURL string    `json:"schemaURL"`
}

// FromEnvelope builds the RunEvent for one event. output is this event's dataset
// (derived by the caller, which owns the domain/entity mapping); inputs are the
// datasets of the causing events the graph has already seen (may be empty — a
// root event, or a cause outside the harvest window).
func FromEnvelope(env *envelopepb.Envelope, output Dataset, inputs []Dataset) RunEvent {
	jobName := env.GetSource()
	if jobName == "" {
		jobName = env.GetDomain()
	}
	return RunEvent{
		EventType: "COMPLETE",
		EventTime: env.GetEventTime().AsTime(),
		Run:       Run{RunID: env.GetCorrelationId()},
		Job:       Job{Namespace: "kanz", Name: jobName},
		Inputs:    inputs,
		Outputs:   []Dataset{output},
		Producer:  Producer,
		SchemaURL: SchemaURL,
	}
}

// Emitter ships RunEvents to an OpenLineage backend. The HTTP transport
// (POST /api/v1/lineage to Marquez/DataHub) is deployment wiring behind this
// seam; LogEmitter is the always-available default.
type Emitter interface {
	Emit(ctx context.Context, ev RunEvent) error
}

// LogEmitter writes each RunEvent as a structured log line — kanz logs are
// stdout JSON shipped by the platform (OBS-01a), so this is a real sink, and the
// safe default when no OpenLineage backend is wired.
type LogEmitter struct{ logger *slog.Logger }

func NewLogEmitter(logger *slog.Logger) *LogEmitter {
	if logger == nil {
		logger = slog.Default()
	}
	return &LogEmitter{logger: logger}
}

func (e *LogEmitter) Emit(ctx context.Context, ev RunEvent) error {
	out := "(root)"
	if len(ev.Outputs) > 0 {
		out = ev.Outputs[0].Namespace + "." + ev.Outputs[0].Name
	}
	e.logger.LogAttrs(ctx, slog.LevelDebug, "openlineage run",
		slog.String("job", ev.Job.Name), slog.String("run", ev.Run.RunID),
		slog.String("output", out), slog.Int("inputs", len(ev.Inputs)))
	return nil
}
