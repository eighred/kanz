package harvest_test

import (
	"context"
	"testing"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/services/lineage/internal/graph"
	"github.com/eighred/kanz/services/lineage/internal/harvest"
	"github.com/eighred/kanz/services/lineage/internal/openlineage"
)

type fakeEmitter struct{ evs []openlineage.RunEvent }

func (f *fakeEmitter) Emit(_ context.Context, ev openlineage.RunEvent) error {
	f.evs = append(f.evs, ev)
	return nil
}

func env(id, cause, domain, ref, source string) *envelopepb.Envelope {
	return &envelopepb.Envelope{
		EventId:          id,
		CorrelationId:    "corr-1",
		CausationId:      cause,
		Domain:           domain,
		EventType:        domain + ".event",
		PayloadSchemaRef: ref,
		Source:           source,
		EventTime:        timestamppb.New(time.Unix(0, 0).UTC()),
	}
}

// LIN-01e: any output's full upstream lineage is queryable. Harvest a 3-hop
// cascade and assert the graph resolves the leaf's full transitive provenance.
func TestHarvestBuildsQueryableLineage(t *testing.T) {
	g := graph.NewMemory()
	em := &fakeEmitter{}
	h := harvest.New(g, em)
	ctx := context.Background()

	// market.MarketDataEvent → risk.ExposureSet → risk.Measures
	must(t, h.Handle(ctx, env("e1", "", "market", "market.v1.MarketDataEvent:1", "market-data"), nil))
	must(t, h.Handle(ctx, env("e2", "e1", "risk", "risk.v1.ExposureSet:1", "risk-engine"), nil))
	must(t, h.Handle(ctx, env("e3", "e2", "risk", "risk.v1.Measures:1", "risk-engine"), nil))

	leaf, ok := g.DatasetOf("e3")
	if !ok {
		t.Fatal("leaf event not in graph")
	}
	up := g.Upstream(leaf)
	if len(up) != 2 {
		t.Fatalf("full upstream not queryable: got %v", up)
	}
	want := map[string]bool{"kanz.market.MarketDataEvent": true, "kanz.risk.ExposureSet": true}
	for _, d := range up {
		if !want[d.String()] {
			t.Errorf("unexpected upstream dataset %s", d)
		}
	}

	// The emitted OpenLineage event for the leaf carries the immediate input.
	leafEv := em.evs[2]
	if len(leafEv.Inputs) != 1 || leafEv.Inputs[0].Name != "ExposureSet" {
		t.Errorf("leaf RunEvent inputs = %+v, want [ExposureSet]", leafEv.Inputs)
	}
	if len(leafEv.Outputs) != 1 || leafEv.Outputs[0].Name != "Measures" {
		t.Errorf("leaf RunEvent outputs = %+v, want [Measures]", leafEv.Outputs)
	}
	if leafEv.Job.Name != "risk-engine" {
		t.Errorf("job name = %q, want risk-engine (source)", leafEv.Job.Name)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
