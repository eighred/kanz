package translate

import (
	"context"
	"sync"
	"testing"
	"time"

	lifecyclepb "github.com/kanz-eng/kanz-schemas-go/lifecycle/v1"
	"google.golang.org/protobuf/proto"
)

func modeChanged(component string, mode lifecyclepb.OperatingMode, by, reason string) []byte {
	b, err := proto.Marshal(&lifecyclepb.ModeChanged{
		Component: component,
		NewMode:   mode,
		ChangedBy: by,
		Reason:    reason,
	})
	if err != nil {
		panic(err)
	}
	return b
}

// The seam this replaced defaulted to `func(string) bool { return false }`. The
// single most important property of the replacement is that its default is the
// OPPOSITE: closed.
func TestGate_DefaultsClosed(t *testing.T) {
	if !NewGate(nil).Halted() {
		t.Fatal("a freshly constructed gate is OPEN — deny-by-default is not holding, and an unwired process would trade")
	}
	var nilGate *Gate
	if !nilGate.Halted() {
		t.Fatal("a nil gate is OPEN — a caller that forgot to wire the brake would trade")
	}
	if OpenGate(nil).Halted() {
		t.Fatal("OpenGate is closed — tests and dev binaries could never trade")
	}
}

// Only NORMAL trades. DEGRADED and MAINTENANCE are not "mostly fine".
func TestGate_OnlyNormalTrades(t *testing.T) {
	for _, mode := range []lifecyclepb.OperatingMode{
		lifecyclepb.OperatingMode_OPERATING_MODE_UNSPECIFIED,
		lifecyclepb.OperatingMode_OPERATING_MODE_DEGRADED,
		lifecyclepb.OperatingMode_OPERATING_MODE_MAINTENANCE,
		lifecyclepb.OperatingMode_OPERATING_MODE_HALTED,
	} {
		g := OpenGate(nil)
		g.Trip(mode, "test")
		if !g.Halted() {
			t.Fatalf("gate open in %s — only OPERATING_MODE_NORMAL may trade", mode)
		}
	}
}

// The latch: a trip is sticky, and the FIRST cause is the one an operator reads.
func TestGate_TripLatchesAndKeepsRootCause(t *testing.T) {
	g := OpenGate(nil)
	g.Trip(lifecyclepb.OperatingMode_OPERATING_MODE_HALTED, "root cause")
	g.Trip(lifecyclepb.OperatingMode_OPERATING_MODE_DEGRADED, "later symptom")

	if !g.Halted() {
		t.Fatal("gate reopened after a trip")
	}
	mode, reason, _ := g.State()
	if mode != lifecyclepb.OperatingMode_OPERATING_MODE_HALTED {
		t.Fatalf("mode = %s, want HALTED — a later symptom overwrote the root cause", mode)
	}
	if reason != "root cause" {
		t.Fatalf("reason = %q, want %q — the cascade buried the original cause", reason, "root cause")
	}
}

// The core of the fail-closed design: losing the spine halts trading, and getting
// the spine BACK does not resume it. Reconnection restores the wire, not the
// safety of the book.
func TestGate_BusLossLatchesAcrossReconnect(t *testing.T) {
	g := OpenGate(nil)
	g.TripOnBusLoss(context.DeadlineExceeded)

	if !g.Halted() {
		t.Fatal("NATS disconnect did not halt trading — the fund is naked on a dead spine")
	}

	// Whatever the bus does next, the gate stays shut. There is no OnReconnect path
	// that clears it; only an operator can.
	if !g.Halted() {
		t.Fatal("gate cleared itself")
	}
	g.Resume("operator:akif", "spine verified healthy")
	if g.Halted() {
		t.Fatal("an explicit operator resume did not reopen the gate")
	}
}

// A detector deciding on its own that things look fine again must NOT be able to
// resume trading after a safety trip. This is the difference between a brake and
// a suggestion.
func TestGate_SystemCannotSelfResume(t *testing.T) {
	g := OpenGate(nil)
	g.Trip(lifecyclepb.OperatingMode_OPERATING_MODE_HALTED, "risk breach")

	payload := modeChanged(ComponentSystem, lifecyclepb.OperatingMode_OPERATING_MODE_NORMAL,
		"system:health-detector", "looks fine now")
	if err := g.Handle(context.Background(), nil, payload); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !g.Halted() {
		t.Fatal("a system: ModeChanged resumed trading after a safety trip — automatic recovery must not clear a latch")
	}

	payload = modeChanged(ComponentSystem, lifecyclepb.OperatingMode_OPERATING_MODE_NORMAL,
		"operator:akif", "investigated, safe to resume")
	if err := g.Handle(context.Background(), nil, payload); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if g.Halted() {
		t.Fatal("an operator ModeChanged did not resume trading — the halt would be unrecoverable")
	}
}

// An explicit halt on the wire stops the system.
func TestGate_HaltFactTrips(t *testing.T) {
	g := OpenGate(nil)
	payload := modeChanged(ComponentSystem, lifecyclepb.OperatingMode_OPERATING_MODE_HALTED,
		"operator:akif", "kill switch")
	if err := g.Handle(context.Background(), nil, payload); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !g.Halted() {
		t.Fatal("an explicit HALTED ModeChanged did not stop trading")
	}
	if _, reason, _ := g.State(); reason != "kill switch" {
		t.Fatalf("reason = %q, want the operator's reason — the audit trail loses why we stopped", reason)
	}
}

// A single component going DEGRADED is a page, not a fund-wide stop.
func TestGate_IgnoresNonSystemComponents(t *testing.T) {
	g := OpenGate(nil)
	payload := modeChanged("market-ingest", lifecyclepb.OperatingMode_OPERATING_MODE_DEGRADED,
		"system:staleness-monitor", "feed lag")
	if err := g.Handle(context.Background(), nil, payload); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if g.Halted() {
		t.Fatal("one DEGRADED component halted the whole fund — escalation is the caller's decision, published as component=system")
	}
}

// "I could not read the brake signal" must never resolve to "keep trading".
func TestGate_UndecodableFactTrips(t *testing.T) {
	g := OpenGate(nil)
	err := g.Handle(context.Background(), nil, []byte{0xff, 0xff, 0xff, 0xff})
	if err != nil {
		t.Fatalf("Handle returned %v — a poison message must ack, not spin the redelivery loop", err)
	}
	if !g.Halted() {
		t.Fatal("an undecodable ModeChanged left the gate OPEN — a corrupt halt signal would be silently ignored")
	}
}

// The gate is read on the 100ms alpha tick and on every webhook, and written from
// a NATS callback goroutine. Run with -race.
func TestGate_ConcurrentReadersAndWriters(t *testing.T) {
	g := OpenGate(nil)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				_ = g.Halted()
				_, _, _ = g.State()
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 500; j++ {
			g.TripOnBusLoss(nil)
			g.Resume("operator:test", "flap")
		}
	}()
	wg.Wait()
}

func TestGate_StateRecordsWhen(t *testing.T) {
	at := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)
	g := OpenGate(func() time.Time { return at })
	g.Trip(lifecyclepb.OperatingMode_OPERATING_MODE_HALTED, "boom")
	if _, _, since := g.State(); !since.Equal(at) {
		t.Fatalf("since = %s, want %s — an operator cannot tell when the halt began", since, at)
	}
}
