// Package positionfixture joins disposable live-OMS publications to their test
// inputs. It stays separate from the base helper because fillfact owns outbox
// types and outbox's in-package tests already consume the base helper.
package positionfixture

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/bustest"
	"github.com/eighred/kanz/internal/fillfact"
	"github.com/eighred/kanz/internal/platform/subject"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
)

// PositionReceipts joins one test's indirect live-OMS effects to their owned
// fill identities. It observes the canonical frame; it never builds a packet.
type PositionReceipts struct {
	mu                    sync.Mutex
	seen                  map[string]map[string]bool
	err                   error
	portfolio, instrument string
	keys                  map[string]bool
}

type receiptReader interface {
	Stop()
	Closed() <-chan struct{}
}

// close releases the finite test's receipts only after its callback has joined.
// A failed join retains the collections rather than clearing a live reader's state.
func (r *PositionReceipts) close(reader receiptReader) error {
	if reader != nil {
		reader.Stop()
		select {
		case <-reader.Closed():
		case <-time.After(5 * time.Second):
			return errors.New("POSITION receipt reader did not stop/join")
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for input := range r.seen {
		clear(r.seen[input])
		delete(r.seen, input)
	}
	clear(r.keys)
	return nil
}

// ReclaimOMSPositions registers the exact system-fixture output keys and starts
// their receipt reader before commands/fills can reach the shared disposable OMS.
// AwaitFills must join the actual owned inputs before this fixture returns.
func ReclaimOMSPositions(t testing.TB, js jetstream.JetStream, portfolio, instrument string, venues ...string) *PositionReceipts {
	t.Helper()
	keys := []string{subject.PositionFor(bus.SystemTenant, portfolio, instrument)}
	for _, venue := range venues {
		keys = append(keys, subject.VenuePositionFor(bus.SystemTenant, portfolio, venue, instrument))
	}
	bustest.ReclaimSubjects(t, js, keys...)
	r := &PositionReceipts{seen: map[string]map[string]bool{}, portfolio: portfolio, instrument: instrument, keys: map[string]bool{}}
	for _, key := range keys {
		r.keys[key] = true
	}
	var c jetstream.Consumer
	var cc jetstream.ConsumeContext
	t.Cleanup(func() {
		if err := r.close(cc); err != nil {
			t.Error(err)
			return
		}
		if c != nil {
			cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			if err := js.DeleteConsumer(cleanup, "POSITION", c.CachedInfo().Name); err != nil && !errors.Is(err, jetstream.ErrConsumerNotFound) {
				t.Error(err)
			}
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.err != nil {
			t.Error(r.err)
		}
	})
	if os.Getenv("TEST_OMS_ON_BUS") != "1" {
		return r
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Preserve the former server-default 30s acknowledgment window and unlimited
	// redelivery explicitly. This finite observer must not exhaust an input before
	// its causal join; stop/join/delete and the 30s inactivity limit own its lifetime.
	c, err := js.CreateConsumer(ctx, "POSITION", jetstream.ConsumerConfig{FilterSubjects: keys, DeliverPolicy: jetstream.DeliverNewPolicy, AckPolicy: jetstream.AckExplicitPolicy, AckWait: 30 * time.Second, MaxDeliver: -1, MaxAckPending: 512, InactiveThreshold: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	cc, err = c.Consume(func(message jetstream.Msg) {
		env, payload, err := bus.Unframe(message.Data())
		var state domainpb.PositionState
		if err != nil || bus.Validate(env) != nil || proto.Unmarshal(payload, &state) != nil || state.PortfolioId != portfolio || state.InstrumentId != instrument || env.GetCausationId() == "" {
			r.mu.Lock()
			r.err = errors.New("invalid owned canonical POSITION receipt")
			r.mu.Unlock()
			return
		}
		key := subject.PositionFor(bus.SystemTenant, portfolio, instrument)
		if state.Venue != "" {
			key = subject.VenuePositionFor(bus.SystemTenant, portfolio, state.Venue, instrument)
		}
		r.mu.Lock()
		if !r.keys[key] || key != message.Subject() || (len(r.seen) >= 128 && r.seen[env.CausationId] == nil) {
			r.err = errors.New("POSITION receipt escaped its bounded exact owner")
		} else {
			if r.seen[env.CausationId] == nil {
				r.seen[env.CausationId] = map[string]bool{}
			}
			r.seen[env.CausationId][key] = true
		}
		r.mu.Unlock()
		if err := message.Ack(); err != nil {
			r.mu.Lock()
			r.err = errors.New("POSITION receipt acknowledgement failed")
			r.mu.Unlock()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	for {
		info, err := c.Info(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if info.NumWaiting > 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("POSITION receipt reader never established its pull")
		case <-time.After(10 * time.Millisecond):
		}
	}
	return r
}

// AwaitFills reads only this run's finite input interval and order IDs, then
// waits for aggregate AND venue facts carrying each input's causation_id.
// A count or quiet sleep cannot satisfy this causal completion condition.
func (r *PositionReceipts) AwaitFills(t testing.TB, js jetstream.JetStream, from uint64, orderIDs ...string) {
	t.Helper()
	if os.Getenv("TEST_OMS_ON_BUS") != "1" {
		return
	}
	if len(orderIDs) == 0 || len(orderIDs) > 64 {
		t.Fatal("1..64 owned input orders required")
	}
	orders := map[string]bool{}
	for _, id := range orderIDs {
		if id == "" {
			t.Fatal("empty owned input identity")
		}
		orders[id] = false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	stream, err := js.Stream(ctx, "EXECUTION")
	if err != nil {
		t.Fatal(err)
	}
	info, err := stream.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.State.LastSeq < from || info.State.LastSeq-from > 4096 {
		t.Fatal("owned input receipt interval exceeds bound")
	}
	expected := map[string]string{}
	for seq := from + 1; seq <= info.State.LastSeq; seq++ {
		message, err := stream.GetMsg(ctx, seq)
		if errors.Is(err, jetstream.ErrMsgNotFound) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if message.Subject != fillfact.SubjectFilled && message.Subject != fillfact.SubjectPartiallyFilled {
			continue
		}
		env, payload, err := bus.Unframe(message.Data)
		if err != nil || bus.Validate(env) != nil {
			continue
		}
		var fill *orderpb.Fill
		var portfolio string
		if message.Subject == fillfact.SubjectFilled {
			var event orderpb.OrderFilled
			if proto.Unmarshal(payload, &event) != nil {
				t.Fatal("invalid fill receipt")
			}
			fill, portfolio = event.Fill, event.GetState().GetPortfolioId()
		} else {
			var event orderpb.OrderPartiallyFilled
			if proto.Unmarshal(payload, &event) != nil {
				t.Fatal("invalid partial-fill receipt")
			}
			fill, portfolio = event.Fill, event.GetState().GetPortfolioId()
		}
		if _, owned := orders[fill.GetOrderId()]; !owned {
			continue
		}
		if portfolio != r.portfolio || fill.GetInstrumentId() != r.instrument || !r.keys[subject.VenuePositionFor(bus.SystemTenant, portfolio, fill.GetVenue(), r.instrument)] {
			t.Fatal("owned fill differs from registered exact output keys")
		}
		orders[fill.OrderId] = true
		expected[env.EventId] = fill.Venue
	}
	for _, found := range orders {
		if !found {
			t.Fatal("owned live-OMS fill receipt missing")
		}
	}
	r.await(t, expected)
}

// AwaitInput uses a canonical input already observed and decoded by its fixture,
// including the strict recovery path whose event type is not a normal fill.
func (r *PositionReceipts) AwaitInput(t testing.TB, eventID, venue string) {
	t.Helper()
	if os.Getenv("TEST_OMS_ON_BUS") != "1" {
		return
	}
	if eventID == "" || !r.keys[subject.VenuePositionFor(bus.SystemTenant, r.portfolio, venue, r.instrument)] {
		t.Fatal("unbound input/venue receipt")
	}
	r.await(t, map[string]string{eventID: venue})
}

func (r *PositionReceipts) await(t testing.TB, expected map[string]string) {
	t.Helper()
	if len(expected) == 0 || len(expected) > 128 {
		t.Fatal("1..128 owned fill identities required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for {
		r.mu.Lock()
		failure := r.err
		complete := true
		for input, venue := range expected {
			complete = complete && r.seen[input][subject.PositionFor(bus.SystemTenant, r.portfolio, r.instrument)] && r.seen[input][subject.VenuePositionFor(bus.SystemTenant, r.portfolio, venue, r.instrument)]
		}
		r.mu.Unlock()
		if failure != nil {
			t.Fatal(failure)
		}
		if complete {
			t.Logf("causal live-OMS POSITION receipts: %d inputs, both exact keys per input", len(expected))
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(fmt.Errorf("owned live-OMS input did not complete both POSITION receipts: %w", ctx.Err()))
		case <-time.After(10 * time.Millisecond):
		}
	}
}
