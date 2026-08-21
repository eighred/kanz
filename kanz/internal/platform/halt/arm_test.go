package halt

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	lifecyclepb "github.com/eighred/kanz/kanz-schemas-go/lifecycle/v1"

	"github.com/eighred/kanz/pkg/bus"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeSub is a broadcast subscriber under test control: it can refuse the
// subscription (the missing-grant case), arm and then deliver, or arm and never
// deliver anything (the empty-stream case every fresh deployment starts in).
type fakeSub struct {
	err      error
	deliver  [][]byte
	subject  string
	blockFor time.Duration
}

func (f *fakeSub) SubscribeBroadcastReady(ctx context.Context, subject string, h bus.EventHandler, ready func()) error {
	f.subject = subject
	if f.err != nil {
		return f.err
	}
	if f.blockFor > 0 {
		select {
		case <-time.After(f.blockFor):
		case <-ctx.Done():
			return nil
		}
	}
	for _, payload := range f.deliver {
		if err := h(ctx, nil, payload); err != nil {
			return err
		}
	}
	if ready != nil {
		ready()
	}
	<-ctx.Done()
	return nil
}

func TestArm_SubscribesToTheHaltSubject(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub := &fakeSub{}
	gate := NewGate(nil)

	wait, err := Arm(ctx, sub, gate, quietLogger())
	if err != nil {
		t.Fatalf("Arm: %v", err)
	}
	// THE SUBJECT IS THE CONTRACT. A service that armed against the wrong subject
	// would report healthy and hear nothing — which is the whole defect, one
	// typo out.
	if sub.subject != SubjectModeChanged {
		t.Fatalf("subscribed to %q, want %q", sub.subject, SubjectModeChanged)
	}
	cancel()
	if err := wait(); err != nil {
		t.Fatalf("wait: %v", err)
	}
}

// AN ARMED SUBSCRIPTION ON AN EMPTY STREAM LEAVES THE GATE CLOSED. This is the
// deployment sign-off cmd/kanz-halt documents — bringing the platform live is an
// explicit, attributable act — and the case a "the subscription worked, so we
// are fine" reading would get backwards.
func TestArm_LeavesTheGateClosedWhenNothingHasBeenPublished(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gate := NewGate(nil)

	if _, err := Arm(ctx, &fakeSub{}, gate, quietLogger()); err != nil {
		t.Fatalf("Arm: %v", err)
	}
	if !gate.Halted() {
		t.Fatal("the gate opened on an empty halt stream — a platform nobody has declared NORMAL " +
			"must not trade")
	}
}

// THE OPERATOR'S RESUME ARRIVES THROUGH THE SUBSCRIPTION, not through a method
// call. This is the whole chain: FACT bytes → Gate.Handle → open.
func TestArm_FoldsTheOperatorResumeThatWasWaitingOnTheStream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gate := NewGate(nil)
	sub := &fakeSub{deliver: [][]byte{modeChanged(ComponentSystem,
		lifecyclepb.OperatingMode_OPERATING_MODE_NORMAL, "operator:akif", "initial deployment validation")}}

	if _, err := Arm(ctx, sub, gate, quietLogger()); err != nil {
		t.Fatalf("Arm: %v", err)
	}
	if gate.Halted() {
		_, reason, _ := gate.State()
		t.Fatalf("the gate stayed closed after an operator resume was replayed: %s", reason)
	}
}

// A REFUSED SUBSCRIPTION IS THE MISSING-GRANT CASE, and it is the one this whole
// issue turns on: a service whose broker account cannot subscribe
// platform.mode.changed authenticates fine and receives NOTHING. It must not
// resolve to "trading".
func TestArm_TripsTheGateWhenTheBrokerRefusesTheSubscription(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gate := OpenGate(nil) // deliberately OPEN, so only Arm can close it
	sub := &fakeSub{err: errors.New("permissions violation for subscribe to \"platform.mode.changed\"")}

	wait, err := Arm(ctx, sub, gate, quietLogger())
	if err == nil {
		t.Fatal("Arm reported success against a broker that refused the subscription")
	}
	if wait != nil {
		t.Fatal("Arm returned a wait func for a subscription that never armed")
	}
	if !gate.Halted() {
		t.Fatal("the gate is still OPEN after the halt subscription was refused — this service " +
			"would trade through a declared halt and report healthy while doing it")
	}
	_, reason, _ := gate.State()
	if !strings.Contains(reason, "permissions violation") {
		t.Fatalf("gate reason = %q, want the broker's own refusal — an operator reading this at 3am "+
			"needs the cause, not 'halted'", reason)
	}
}

// A NIL SUBSCRIBER IS A WIRING BUG, and it must present as one rather than as a
// service that quietly never subscribes.
func TestArm_RefusesANilSubscriber(t *testing.T) {
	gate := OpenGate(nil)
	if _, err := Arm(context.Background(), nil, gate, quietLogger()); err == nil {
		t.Fatal("Arm accepted a nil subscriber")
	}
	if !gate.Halted() {
		t.Fatal("a service with no subscriber wired is still trading")
	}
}

func TestArm_RefusesANilGate(t *testing.T) {
	if _, err := Arm(context.Background(), &fakeSub{}, nil, quietLogger()); err == nil {
		t.Fatal("Arm accepted a nil gate — nothing would fold the brake signal")
	}
}

// A SUBSCRIPTION THAT DIES AFTER ARMING closes the gate too. The wire is the
// only thing that can carry a resume, so losing it means this process can no
// longer establish that trading is safe.
func TestArm_TripsTheGateWhenTheSubscriptionDiesLater(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gate := OpenGate(nil)
	sub := &dyingSub{fail: make(chan error, 1)}

	wait, err := Arm(ctx, sub, gate, quietLogger())
	if err != nil {
		t.Fatalf("Arm: %v", err)
	}
	sub.fail <- errors.New("consumer deleted")
	if err := wait(); err == nil {
		t.Fatal("wait() reported a clean end for a subscription that died")
	}
	if !gate.Halted() {
		t.Fatal("the gate is still OPEN after the halt subscription died")
	}
}

// dyingSub arms immediately and then fails when told to.
type dyingSub struct{ fail chan error }

func (d *dyingSub) SubscribeBroadcastReady(_ context.Context, _ string, _ bus.EventHandler, ready func()) error {
	if ready != nil {
		ready()
	}
	return <-d.fail
}

// *bus.Consumer MUST SATISFY Subscriber. The interface exists so tests can arm
// without a broker; if it drifts from the real consumer's signature, every test
// above passes against a shape production never uses.
var _ Subscriber = (*bus.Consumer)(nil)

func TestArmTimeoutIsNotZero(t *testing.T) {
	// A zero ArmTimeout would make every Arm fail instantly and latch every gate
	// on the platform closed at startup — a total trading outage from a constant.
	if ArmTimeout <= 0 {
		t.Fatalf("ArmTimeout = %v", ArmTimeout)
	}
}
