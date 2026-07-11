package translate

import (
	"context"
	"strings"
	"sync"
	"time"

	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
	lifecyclepb "github.com/kanz-eng/kanz-schemas-go/lifecycle/v1"
	"google.golang.org/protobuf/proto"
)

// SubjectModeChanged carries the lifecycle.v1.ModeChanged FACT — the halt signal.
// It is the operator's brake and the system's, and it is the ONLY thing that can
// reopen a latched gate.
const SubjectModeChanged = "platform.mode.changed"

// ComponentSystem is the ModeChanged.component value for a whole-system
// transition. The gate reacts to this and nothing else: a single DEGRADED
// component is a page, not a reason to stop trading the whole fund. Escalating a
// component fault into a system halt is the caller's decision, published as a
// system-level ModeChanged.
const ComponentSystem = "system"

// Gate is the kill-switch. It is the ONE brake both brains pass through: the
// TradingView webhook perimeter checks it before an alert becomes an intent, and
// the native alpha runner checks it at the top of every engine tick. A single
// gate is the point — two brakes drift, and the one that is not wired is the one
// that fails during the incident.
//
// DENY-BY-DEFAULT. The gate is CLOSED until something explicitly opens it: a
// zero-value Gate is halted, and trading is permitted only in OPERATING_MODE_NORMAL.
// Any other mode — UNSPECIFIED, DEGRADED, MAINTENANCE, HALTED — blocks. That means
// a process that starts up and never learns the system mode does not trade, which
// is the whole reason this type exists: the seam it replaces defaulted to
// `func(string) bool { return false }` and was never wired, so the kill-switch was
// inert in production.
//
// FAIL-CLOSED AND LATCHING. Trip() closes the gate and it STAYS closed. A NATS
// reconnect does not reopen it; nor does a system-generated ModeChanged. Only an
// explicit operator Resume — or an operator-issued ModeChanged(NORMAL) — clears the
// latch. This mirrors lifecycle.v1's own contract for OPERATING_MODE_HALTED:
// "stopped by a fault or a safety trip ... requires intervention to leave". The
// tradeoff is deliberate: a transient spine blip halts the fund until a human
// resumes it, which is the side of the trade we want to be wrong on.
type Gate struct {
	mu     sync.RWMutex
	mode   lifecyclepb.OperatingMode
	reason string
	since  time.Time
	now    func() time.Time
}

// NewGate returns a CLOSED gate. now may be nil (⇒ time.Now).
func NewGate(now func() time.Time) *Gate {
	if now == nil {
		now = time.Now
	}
	return &Gate{
		mode:   lifecyclepb.OperatingMode_OPERATING_MODE_UNSPECIFIED,
		reason: "gate has not been opened since startup",
		since:  now(),
		now:    now,
	}
}

// OpenGate returns a gate already in NORMAL. It exists for tests and for local
// dev binaries that have no lifecycle stream to learn the mode from. Production
// wiring should start from NewGate and let the ModeChanged FACT open it, so that
// "trading is allowed" is always something the system was told, never something it
// assumed.
func OpenGate(now func() time.Time) *Gate {
	g := NewGate(now)
	g.mode = lifecyclepb.OperatingMode_OPERATING_MODE_NORMAL
	g.reason = "opened explicitly at construction"
	return g
}

// Halted reports whether execution is blocked. A nil Gate is halted: a caller that
// forgot to wire the brake does not get to trade.
func (g *Gate) Halted() bool {
	if g == nil {
		return true
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.mode != lifecyclepb.OperatingMode_OPERATING_MODE_NORMAL
}

// State returns the current mode, the reason it was entered, and when. For the
// /healthz surface and for the error an operator reads at 3am.
func (g *Gate) State() (lifecyclepb.OperatingMode, string, time.Time) {
	if g == nil {
		return lifecyclepb.OperatingMode_OPERATING_MODE_UNSPECIFIED, "gate not wired", time.Time{}
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.mode, g.reason, g.since
}

// Trip closes the gate and latches it. It is idempotent and the FIRST reason wins:
// during a cascade the root cause is what an operator needs to see, not the last
// symptom to fire. Safe on a nil Gate (already closed).
func (g *Gate) Trip(mode lifecyclepb.OperatingMode, reason string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.mode != lifecyclepb.OperatingMode_OPERATING_MODE_NORMAL {
		return // already closed — keep the original cause
	}
	g.mode = mode
	g.reason = reason
	g.since = g.now()
}

// Resume clears the latch. This is the operator intervention that
// OPERATING_MODE_HALTED requires; nothing automatic calls it.
func (g *Gate) Resume(by, reason string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.mode = lifecyclepb.OperatingMode_OPERATING_MODE_NORMAL
	g.reason = "resumed by " + by + ": " + reason
	g.since = g.now()
}

// Observe applies a lifecycle.v1.ModeChanged FACT from the bus.
//
// Any non-NORMAL mode trips the gate. A NORMAL mode reopens it ONLY when the
// transition was made by an operator: changed_by is "{type}:{id}", and the proto
// reserves the "system:" prefix for automatic transitions. A detector that decides
// on its own that things look fine again is exactly what must NOT be able to
// resume trading after a safety trip.
func (g *Gate) Observe(mc *lifecyclepb.ModeChanged) {
	if g == nil || mc == nil {
		return
	}
	if mc.GetNewMode() != lifecyclepb.OperatingMode_OPERATING_MODE_NORMAL {
		g.Trip(mc.GetNewMode(), mc.GetReason())
		return
	}
	if strings.HasPrefix(mc.GetChangedBy(), "system:") {
		return // automatic recovery cannot clear a latched trip
	}
	g.Resume(mc.GetChangedBy(), mc.GetReason())
}

// Handle is the bus.EventHandler for SubjectModeChanged: it decodes the halt FACT
// and applies it. Wire it with consumer.Subscribe(ctx, translate.SubjectModeChanged, ...).
//
// A ModeChanged that will not decode TRIPS the gate. That is the deny-by-default
// rule taken seriously: an undecodable message on the halt channel means the
// system cannot establish that it is safe to trade, and "I could not read the
// brake signal" must never resolve to "keep trading". It acks rather than erroring
// so a poison message cannot spin the redelivery loop — the gate is already closed,
// which is the outcome that matters.
func (g *Gate) Handle(_ context.Context, _ *envelopepb.Envelope, payload []byte) error {
	var mc lifecyclepb.ModeChanged
	if err := proto.Unmarshal(payload, &mc); err != nil {
		g.Trip(lifecyclepb.OperatingMode_OPERATING_MODE_HALTED,
			"undecodable ModeChanged FACT on "+SubjectModeChanged+": "+err.Error())
		return nil
	}
	if mc.GetComponent() != ComponentSystem {
		return nil
	}
	g.Observe(&mc)
	return nil
}

// TripOnBusLoss is the NATS spine watchdog: hand it to bus.NATSConfig.OnDisconnect.
// Losing the spine means orders, fills, and the halt FACT itself stop flowing, so
// the system can no longer know whether it is safe to trade — and under
// deny-by-default, not knowing means not trading. There is deliberately no
// corresponding OnReconnect that reopens the gate: reconnection restores the wire,
// not the safety of the book, and an operator decides that.
func (g *Gate) TripOnBusLoss(err error) {
	reason := "NATS spine disconnected"
	if err != nil {
		reason += ": " + err.Error()
	}
	g.Trip(lifecyclepb.OperatingMode_OPERATING_MODE_HALTED, reason)
}
