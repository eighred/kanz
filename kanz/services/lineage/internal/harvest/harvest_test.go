package harvest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/lineage/internal/graph"
	"github.com/eighred/kanz/services/lineage/internal/harvest"
	"github.com/eighred/kanz/services/lineage/internal/openlineage"
)

type fakeEmitter struct {
	evs  []openlineage.OwnedRunEvent
	fail int
}

func (f *fakeEmitter) Emit(_ context.Context, ev openlineage.OwnedRunEvent) error {
	f.evs = append(f.evs, ev)
	if f.fail > 0 {
		f.fail--
		return errors.New("owned emitter failure")
	}
	return nil
}

func env(id, cause, domain, ref, source string) *envelopepb.Envelope {
	return &envelopepb.Envelope{
		TenantId: "acme", EnvelopeVersion: 2, SchemaVersion: 1, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT,
		IdempotencyKey: id, ProducerVersion: "fixture", IngestionTime: timestamppb.New(time.Unix(0, 0)), PublishTime: timestamppb.New(time.Unix(0, 0)),
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
	g, err := graph.NewMemory(graph.Limits{EventIndexMax: 32, DatasetMax: 16, EdgeMax: 32, StringBytesMax: 65536})
	if err != nil {
		t.Fatal(err)
	}
	em := &fakeEmitter{}
	h := harvest.New(g, em)
	ctx := context.Background()

	// market.MarketDataEvent → risk.ExposureSet → risk.Measures
	must(t, h.Handle(ctx, env("e1", "", "market", "market.v1.MarketDataEvent:1", "market-data"), nil))
	must(t, h.Handle(ctx, env("e2", "e1", "risk", "risk.v1.ExposureSet:1", "risk-engine"), nil))
	must(t, h.Handle(ctx, env("e3", "e2", "risk", "risk.v1.Measures:1", "risk-engine"), nil))

	leaf, look := g.DatasetOf("acme", "e3")
	if look != graph.LookupRetained {
		t.Fatalf("leaf event not in graph: %q", look)
	}
	up := g.Upstream("acme", leaf)
	if len(up) != 2 {
		t.Fatalf("full upstream not queryable: got %v", up)
	}
	want := map[string]bool{"kanz.market.MarketDataEvent": true, "kanz.risk.ExposureSet": true}
	for _, d := range up {
		if !want[d.ID.String()] {
			t.Errorf("unexpected upstream dataset %s", d.ID)
		}
	}

	// The emitted OpenLineage event for the leaf carries the immediate input.
	leafEv := em.evs[2].Event()
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

func harvestGraph(t *testing.T, capacity int, datasets int) *graph.Memory {
	t.Helper()
	g, err := graph.NewMemory(graph.Limits{EventIndexMax: capacity, DatasetMax: datasets, EdgeMax: 32, StringBytesMax: 65536})
	if err != nil {
		t.Fatal(err)
	}
	return g
}
func TestHarvesterUsesValidatedEnvelopeOwnerWithoutFallback(t *testing.T) {
	g := harvestGraph(t, 8, 8)
	em := &fakeEmitter{}
	h := harvest.New(g, em)
	e := env("a", "", "risk", "risk.v1.ExposureSet:1", "risk-engine")
	ctx := bus.WithTenantID(context.Background(), "forged-context")
	if err := h.Handle(ctx, e, nil); err != nil {
		t.Fatal(err)
	}
	if em.evs[0].Owner() != "acme" {
		t.Fatal("context supplied graph/emission owner")
	}
	before := g.Stats()
	e.TenantId = ""
	if err := h.Handle(ctx, e, nil); err == nil || !bus.IsTerminal(err) {
		t.Fatal("unknown envelope owner fell back instead of refusing")
	}
	if g.Stats() != before || len(em.evs) != 1 {
		t.Fatal("unknown owner mutated/emitted")
	}
}
func TestEmitterRetryUsesTheFrozenOwnedAdmission(t *testing.T) {
	g := harvestGraph(t, 8, 8)
	em := &fakeEmitter{fail: 1}
	h := harvest.New(g, em)
	child := env("child", "parent", "risk", "risk.v1.ExposureSet:1", "risk-engine")
	if err := h.Handle(context.Background(), child, nil); err == nil {
		t.Fatal("emitter failure swallowed")
	}
	parent := env("parent", "", "market", "market.v1.MarketDataEvent:1", "market-ingest")
	if err := h.Handle(context.Background(), parent, nil); err != nil {
		t.Fatal(err)
	}
	if err := h.Handle(context.Background(), child, nil); err != nil {
		t.Fatal(err)
	}
	first, _ := json.Marshal(em.evs[0].Event())
	again, _ := json.Marshal(em.evs[2].Event())
	if !bytes.Equal(first, again) || em.evs[0].Owner() != em.evs[2].Owner() {
		t.Fatal("retry acquired later parent or changed owner")
	}
	info, _ := g.Get("acme", graph.DatasetID{Namespace: "kanz.risk", Name: "ExposureSet"})
	if info.Events != 1 || g.Coverage("acme").UnlinkedCauses != 1 {
		t.Fatal("retry recounted the admitted observation")
	}
}
func TestCapacityAndHistoryUnknownStayTransientAndDoNotEmit(t *testing.T) {
	t.Run("capacity", func(t *testing.T) {
		g := harvestGraph(t, 8, 1)
		em := &fakeEmitter{}
		h := harvest.New(g, em)
		must(t, h.Handle(context.Background(), env("a", "", "market", "market.v1.MarketDataEvent:1", "market-ingest"), nil))
		before := g.Stats()
		err := h.Handle(context.Background(), env("b", "a", "risk", "risk.v1.ExposureSet:1", "risk-engine"), nil)
		if !errors.Is(err, graph.ErrCapacity) || bus.IsTerminal(err) || g.Stats() != before || len(em.evs) != 1 {
			t.Fatal("capacity refusal became terminal, partial state or emission")
		}
	})
	t.Run("retention", func(t *testing.T) {
		g := harvestGraph(t, 1, 8)
		em := &fakeEmitter{}
		h := harvest.New(g, em)
		old := env("a", "", "risk", "risk.v1.ExposureSet:1", "risk-engine")
		must(t, h.Handle(context.Background(), old, nil))
		newer := env("b", "", "risk", "risk.v1.ExposureSet:1", "risk-engine")
		newer.EventTime = timestamppb.New(time.Unix(1, 0))
		must(t, h.Handle(context.Background(), newer, nil))
		before := g.Stats()
		err := h.Handle(context.Background(), old, nil)
		if !errors.Is(err, graph.ErrHistoryUnknown) || bus.IsTerminal(err) || g.Stats() != before || len(em.evs) != 2 {
			t.Fatal("evicted same bytes were recounted or marked poison")
		}
	})
}
