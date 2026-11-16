// Package openlineage builds standard RunEvents from the atomic owned graph
// snapshot. Ownership is carried separately; canonical URNs are never prefixed.
package openlineage

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/eighred/kanz/services/lineage/internal/graph"
)

const SchemaURL = "https://openlineage.io/spec/2-0-2/OpenLineage.json"
const Producer = "https://github.com/eighred/kanz/services/lineage"

type Dataset struct {
	Namespace string         `json:"namespace"`
	Name      string         `json:"name"`
	Facets    map[string]any `json:"facets,omitempty"`
}
type Job struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}
type Run struct {
	RunID string `json:"runId"`
}
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

// OwnedRunEvent cannot be constructed with a caller-supplied owner. Its zero
// value is UNKNOWN and every emitter must refuse it. Event() is a wire view,
// not evidence that an unscoped remote store can isolate its canonical URNs.
type OwnedRunEvent struct {
	owner graph.Owner
	event RunEvent
}

func (e OwnedRunEvent) Owner() graph.Owner { return e.owner }
func (e OwnedRunEvent) Event() RunEvent    { return e.event }

func FromSnapshot(snapshot graph.ObservationSnapshot) (OwnedRunEvent, error) {
	if snapshot.Owner() == "" {
		return OwnedRunEvent{}, errors.New("openlineage: snapshot owner unknown")
	}
	o := snapshot.Observation()
	var inputs []Dataset
	if parent, ok := snapshot.Input(); ok && parent != o.Dataset {
		inputs = append(inputs, Dataset{Namespace: parent.Namespace, Name: parent.Name})
	}
	output := Dataset{Namespace: o.Dataset.Namespace, Name: o.Dataset.Name,
		Facets: map[string]any{"kanzSchema": map[string]any{
			"_producer": Producer, "_schemaURL": SchemaURL, "payloadSchemaRef": o.SchemaRef,
		}}}
	return OwnedRunEvent{owner: snapshot.Owner(), event: RunEvent{
		EventType: "COMPLETE", EventTime: o.At, Run: Run{RunID: o.CorrelationID},
		Job: Job{Namespace: "kanz", Name: o.Source}, Inputs: inputs, Outputs: []Dataset{output},
		Producer: Producer, SchemaURL: SchemaURL,
	}}, nil
}

type Emitter interface {
	Emit(context.Context, OwnedRunEvent) error
}
type LogEmitter struct{ logger *slog.Logger }

func NewLogEmitter(logger *slog.Logger) *LogEmitter {
	if logger == nil {
		logger = slog.Default()
	}
	return &LogEmitter{logger: logger}
}
func (e *LogEmitter) Emit(ctx context.Context, event OwnedRunEvent) error {
	if event.Owner() == "" {
		return errors.New("openlineage: emission owner unknown")
	}
	wire := event.Event()
	out := "(root)"
	if len(wire.Outputs) > 0 {
		out = wire.Outputs[0].Namespace + "." + wire.Outputs[0].Name
	}
	e.logger.LogAttrs(ctx, slog.LevelDebug, "openlineage run",
		slog.String("owner", string(event.Owner())), slog.String("job", wire.Job.Name),
		slog.String("run", wire.Run.RunID), slog.String("output", out), slog.Int("inputs", len(wire.Inputs)))
	return nil
}
