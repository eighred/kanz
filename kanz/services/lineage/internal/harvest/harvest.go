// Package harvest folds validated live envelopes into the owned projection.
package harvest

import (
	"context"
	"errors"
	"strings"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/lineage/internal/graph"
	"github.com/eighred/kanz/services/lineage/internal/openlineage"
)

type Harvester struct {
	graph   graph.Graph
	emitter openlineage.Emitter
}

func New(g graph.Graph, e openlineage.Emitter) *Harvester { return &Harvester{graph: g, emitter: e} }

// Handle validates even direct calls. Owner is the exact envelope tenant, not a
// principal, context fallback or LINEAGE_TENANT. Validation establishes envelope
// invariants; it does not cryptographically bind an account to payload tenancy.
// Observe returns the parent/output projection from its atomic admission. Emit
// failures propagate to the existing one-attempt DLQ path; a retained retry
// emits that same snapshot without recounting or resolving a later parent.
func (h *Harvester) Handle(ctx context.Context, env *envelopepb.Envelope, _ []byte) error {
	if err := bus.Validate(env); err != nil {
		return bus.Terminal(err)
	}
	snapshot, err := h.graph.Observe(graph.Observation{
		Owner: graph.Owner(env.GetTenantId()), EventID: env.GetEventId(),
		Dataset: datasetFor(env), Domain: env.GetDomain(), SchemaRef: env.GetPayloadSchemaRef(),
		At: env.GetEventTime().AsTime(), ParentEventID: env.GetCausationId(),
		Source: env.GetSource(), CorrelationID: env.GetCorrelationId(),
	})
	if err != nil {
		if errors.Is(err, graph.ErrConflict) {
			return bus.Terminal(err)
		}
		return err // capacity and retained-history UNKNOWN stay TRANSIENT
	}
	event, err := openlineage.FromSnapshot(snapshot)
	if err != nil {
		return err
	}
	return h.emitter.Emit(ctx, event)
}

// datasetFor preserves the canonical lake/catalog namespace and schema identity.
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
