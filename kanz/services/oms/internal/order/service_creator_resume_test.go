package order

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/outbox"
	"github.com/eighred/kanz/internal/platform/halt"
)

type committedCreatorStore struct {
	*MemoryStore
	committed chan struct{}
	proceed   chan struct{}
	unblock   sync.Once
}

func (s *committedCreatorStore) Create(ctx context.Context, state *orderpb.OrderState, facts []outbox.Record) error {
	if err := s.MemoryStore.Create(ctx, state, facts); err != nil {
		return err
	}
	close(s.committed)
	select {
	case <-s.proceed:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestService_CreatorReloadsAfterAnotherDeliveryWorksTheOrder(t *testing.T) {
	ctx, cancel := context.WithTimeout(testCtx(), 10*time.Second)
	defer cancel()
	store := &committedCreatorStore{MemoryStore: NewMemoryStore(), committed: make(chan struct{}), proceed: make(chan struct{})}
	fb := &fakeBus{}
	logs := &captureHandler{}
	venue := &countingVenue{Venue: execution.NewSimVenue("XSIM")}
	svc, err := NewService(testTenant, store, NewEmitter(fb), nil, execution.NewRouter([]execution.Venue{venue}), nil, slog.New(logs), WithHaltGate(halt.OpenGate(nil)))
	if err != nil {
		t.Fatal(err)
	}
	cmd := limitOrder(d(100, 0), d(1025, -2))
	body := mustMarshal(t, cmd)
	done := make(chan error, 1)
	joined := false
	t.Cleanup(func() {
		store.unblock.Do(func() { close(store.proceed) })
		if !joined {
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("creator shutdown: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Error("creator did not join after releasing its barrier")
			}
		}
	})
	go func() { done <- svc.Handle(ctx, submitEnv(), body) }()
	select {
	case <-store.committed:
	case <-ctx.Done():
		t.Fatal("creator did not commit admission")
	}
	// The creator owns the admission row, but has not acquired the per-order
	// claim. A redelivery can finish working it before the creator returns.
	if err := svc.Handle(ctx, submitEnv(), body); err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	state, version, err := store.Load(ctx, cmd.GetOrderId())
	if err != nil || version == 0 || state.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_FILLED || venue.count() != 1 {
		t.Fatalf("redelivery did not finish before creator resumed: version=%d state=%v venue=%d err=%v", version, state.GetStatus(), venue.count(), err)
	}
	store.unblock.Do(func() { close(store.proceed) })
	select {
	case err := <-done:
		joined = true
		if err != nil {
			t.Fatalf("creator after completed redelivery: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("creator did not finish")
	}
	if venue.count() != 1 {
		t.Fatalf("venue executions = %d, want 1", venue.count())
	}
	want := []string{EventTypeAccepted, EventTypeRouted, EventTypeFilled, EventTypeOutcome}
	if got := fb.types(); !equal(got, want) {
		t.Fatalf("facts = %v, want %v", got, want)
	}
	assertUnknownFillPublication(t, logs, 0)
	state, version, err = store.Load(ctx, cmd.GetOrderId())
	if err != nil || dec.FromProto(state.GetFilledQuantity()).Cmp(dec.FromProto(cmd.GetQuantity())) != 0 || state.GetOutcomeAnnouncedAt() == nil || store.outbox.PendingCount() != 0 {
		t.Fatalf("completed state/announcement mismatch: state=%v err=%v", state, err)
	}
	if err := svc.Handle(ctx, submitEnv(), body); err != nil {
		t.Fatalf("later duplicate: %v", err)
	}
	after, afterVersion, err := store.Load(ctx, cmd.GetOrderId())
	if err != nil || afterVersion != version || !proto.Equal(after, state) || !equal(fb.types(), want) || venue.count() != 1 {
		t.Fatalf("later duplicate changed state/facts: version=%d want=%d err=%v", afterVersion, version, err)
	}
}
