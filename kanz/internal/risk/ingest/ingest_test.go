package ingest_test

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	"github.com/eighred/kanz/internal/risk/ingest"
)

// fakeApplier records every dispatch so tests can assert routing
// without needing the real RISK-05 state-apply layer.
type fakeApplier struct {
	portfolios []*domainpb.PortfolioState
	positions  []*domainpb.PositionState
	snapshots  []*domainpb.PortfolioSnapshot
	envs       []*envelopepb.Envelope
	failNext   error
}

func (f *fakeApplier) ApplyPortfolioRevalued(_ context.Context, env *envelopepb.Envelope, p *domainpb.PortfolioState) error {
	f.envs = append(f.envs, env)
	f.portfolios = append(f.portfolios, p)
	return f.takeErr()
}

func (f *fakeApplier) ApplyPositionChanged(_ context.Context, env *envelopepb.Envelope, p *domainpb.PositionState) error {
	f.envs = append(f.envs, env)
	f.positions = append(f.positions, p)
	return f.takeErr()
}

func (f *fakeApplier) ApplyPortfolioSnapshot(_ context.Context, env *envelopepb.Envelope, p *domainpb.PortfolioSnapshot) error {
	f.envs = append(f.envs, env)
	f.snapshots = append(f.snapshots, p)
	return f.takeErr()
}

func (f *fakeApplier) takeErr() error {
	e := f.failNext
	f.failNext = nil
	return e
}

func TestNewIngestorRejectsNilApplier(t *testing.T) {
	if _, err := ingest.NewIngestor(nil); err == nil {
		t.Fatal("expected error for nil applier")
	}
}

func TestHandlerDispatchesPortfolioRevalued(t *testing.T) {
	app := &fakeApplier{}
	ing, err := ingest.NewIngestor(app)
	if err != nil {
		t.Fatalf("NewIngestor: %v", err)
	}
	payload := &domainpb.PortfolioState{
		PortfolioId:  "PORT-1",
		DisplayName:  "Alpha Strategy",
		BaseCurrency: "USD",
		AsOf:         timestamppb.Now(),
	}
	body, err := proto.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	env := &envelopepb.Envelope{EventType: ingest.EventTypePortfolioRevalued, EventId: "evt-1"}

	if err := ing.Handler(context.Background(), env, body); err != nil {
		t.Fatalf("Handler: %v", err)
	}
	if got := len(app.portfolios); got != 1 {
		t.Fatalf("portfolios dispatched=%d want 1", got)
	}
	if got := app.portfolios[0].PortfolioId; got != "PORT-1" {
		t.Errorf("PortfolioId=%q want PORT-1", got)
	}
	if app.envs[0] != env {
		t.Error("envelope not forwarded to applier")
	}
}

func TestHandlerDispatchesPositionChanged(t *testing.T) {
	app := &fakeApplier{}
	ing, _ := ingest.NewIngestor(app)
	payload := &domainpb.PositionState{
		PortfolioId:  "PORT-1",
		InstrumentId: "AAPL",
		AsOf:         timestamppb.Now(),
	}
	body, _ := proto.Marshal(payload)
	env := &envelopepb.Envelope{EventType: ingest.EventTypePositionChanged, EventId: "evt-2"}

	if err := ing.Handler(context.Background(), env, body); err != nil {
		t.Fatalf("Handler: %v", err)
	}
	if got := len(app.positions); got != 1 {
		t.Fatalf("positions dispatched=%d want 1", got)
	}
	if got := app.positions[0].InstrumentId; got != "AAPL" {
		t.Errorf("InstrumentId=%q want AAPL", got)
	}
}

func TestHandlerDispatchesPortfolioSnapshot(t *testing.T) {
	app := &fakeApplier{}
	ing, _ := ingest.NewIngestor(app)
	payload := &domainpb.PortfolioSnapshot{
		Portfolio: &domainpb.PortfolioState{PortfolioId: "PORT-1", BaseCurrency: "USD"},
		Positions: []*domainpb.PositionState{
			{PortfolioId: "PORT-1", InstrumentId: "AAPL"},
		},
	}
	body, _ := proto.Marshal(payload)
	env := &envelopepb.Envelope{EventType: ingest.EventTypePortfolioSnapshot, EventId: "evt-3"}

	if err := ing.Handler(context.Background(), env, body); err != nil {
		t.Fatalf("Handler: %v", err)
	}
	if got := len(app.snapshots); got != 1 {
		t.Fatalf("snapshots dispatched=%d want 1", got)
	}
	if got := len(app.snapshots[0].Positions); got != 1 {
		t.Errorf("snapshot positions=%d want 1", got)
	}
}

func TestHandlerSurfacesUnknownEventType(t *testing.T) {
	app := &fakeApplier{}
	ing, _ := ingest.NewIngestor(app)
	env := &envelopepb.Envelope{EventType: "market.equity.trade", EventId: "evt-x"}

	err := ing.Handler(context.Background(), env, nil)
	if err == nil {
		t.Fatal("expected error for unknown event_type")
	}
	if !errors.Is(err, ingest.ErrUnknownEventType) {
		t.Errorf("err=%v, want errors.Is ErrUnknownEventType", err)
	}
	if len(app.envs) != 0 {
		t.Error("applier called for unknown event_type")
	}
}

func TestHandlerSurfacesUnmarshalError(t *testing.T) {
	app := &fakeApplier{}
	ing, _ := ingest.NewIngestor(app)
	env := &envelopepb.Envelope{EventType: ingest.EventTypePortfolioRevalued, EventId: "evt-x"}

	// Truncated / malformed payload — proto.Unmarshal will fail.
	err := ing.Handler(context.Background(), env, []byte{0xff, 0xff, 0xff})
	if err == nil {
		t.Fatal("expected unmarshal error")
	}
	if len(app.envs) != 0 {
		t.Error("applier called despite unmarshal error")
	}
}

func TestHandlerSurfacesApplierError(t *testing.T) {
	app := &fakeApplier{failNext: errors.New("apply failed")}
	ing, _ := ingest.NewIngestor(app)
	body, _ := proto.Marshal(&domainpb.PortfolioState{PortfolioId: "PORT-1", BaseCurrency: "USD", AsOf: timestamppb.Now()})
	env := &envelopepb.Envelope{EventType: ingest.EventTypePortfolioRevalued, EventId: "evt-1"}

	err := ing.Handler(context.Background(), env, body)
	if err == nil {
		t.Fatal("expected applier error to surface")
	}
	if err.Error() != "apply failed" {
		t.Errorf("err=%v want apply failed", err)
	}
}
