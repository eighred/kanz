// Package harvest is the bus consumer that folds the event backbone into the
// lineage graph and emits OpenLineage RunEvents (LIN-01a). It records EVERY
// delivered event — a dropped event is a hole in the lineage graph — mapping
// each to its dataset (domain + entity) and linking it to the dataset of the
// event that caused it.
package harvest

import (
	"context"
	"strings"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	"github.com/eighred/kanz/services/lineage/internal/graph"
	"github.com/eighred/kanz/services/lineage/internal/openlineage"
)

// Harvester maps envelopes into the graph + an OpenLineage emitter.
type Harvester struct {
	graph   graph.Graph
	emitter openlineage.Emitter
}

func New(g graph.Graph, e openlineage.Emitter) *Harvester {
	return &Harvester{graph: g, emitter: e}
}

// Handle matches bus.EventHandler. It is idempotent at the dataset level
// (re-observing an event re-counts it but the edges are a set), so at-least-once
// redelivery is safe. An emitter error is returned so the bus can retry/DLQ —
// lineage completeness is durable-grade, not lossy.
func (h *Harvester) Handle(ctx context.Context, env *envelopepb.Envelope, _ []byte) error {
	ds := datasetFor(env)
	output := openlineage.Dataset{
		Namespace: ds.Namespace,
		Name:      ds.Name,
		Facets:    schemaFacet(env.GetPayloadSchemaRef()),
	}

	// Resolve the input dataset(s) from the cause, if the graph has seen it.
	var inputs []openlineage.Dataset
	if cause := env.GetCausationId(); cause != "" {
		if parentDS, ok := h.graph.DatasetOf(cause); ok && parentDS != ds {
			inputs = append(inputs, openlineage.Dataset{Namespace: parentDS.Namespace, Name: parentDS.Name})
		}
	}

	h.graph.Observe(env.GetEventId(), ds, env.GetDomain(), env.GetPayloadSchemaRef(),
		env.GetEventTime().AsTime(), env.GetCausationId())

	return h.emitter.Emit(ctx, openlineage.FromEnvelope(env, output, inputs))
}

// datasetFor derives an event's dataset: namespace "kanz.{domain}", name from the
// schema ref's message type ("risk.v1.ExposureSet:3" → "ExposureSet"), falling
// back to event_type. Mirrors the lake-sink entity derivation so the lineage
// graph and the lakehouse tables key on the same dataset identity.
func datasetFor(env *envelopepb.Envelope) graph.DatasetID {
	domain := env.GetDomain()
	if domain == "" {
		domain = "unknown"
	}
	return graph.DatasetID{Namespace: "kanz." + domain, Name: entityFrom(env.GetPayloadSchemaRef(), env.GetEventType())}
}

func entityFrom(ref, eventType string) string {
	if ref != "" {
		id := ref
		if i := strings.LastIndex(ref, ":"); i >= 0 {
			id = ref[:i]
		}
		if j := strings.LastIndex(id, "."); j >= 0 {
			return id[j+1:]
		}
		return id
	}
	if eventType != "" {
		return eventType
	}
	return "unknown"
}

func schemaFacet(ref string) map[string]any {
	if ref == "" {
		return nil
	}
	// A minimal custom facet carrying the EVT-16 schema ref — enough for the
	// catalog (LIN-01b) to join the dataset to its registry schema.
	return map[string]any{"kanzSchema": map[string]any{
		"_producer":        openlineage.Producer,
		"_schemaURL":       openlineage.SchemaURL,
		"payloadSchemaRef": ref,
	}}
}
