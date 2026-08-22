package monitor

import (
	"testing"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	lifecyclepb "github.com/eighred/kanz/kanz-schemas-go/lifecycle/v1"
	"google.golang.org/protobuf/proto"

	comp "github.com/eighred/kanz/internal/compliance"
)

// A MANDATE THAT COULD NOT BE APPLIED IS TERMINAL FOR THE MONITOR TOO (#619).
//
// The monitor returns a lookup error to the bus so the position FACT is
// redelivered, which is right for a transient store failure and catastrophic
// here: the mandate stream is COMPACTED, so a redelivery re-reads the identical
// bytes and fails identically, forever. The monitor's own doc says what that
// costs — "a redelivery loop on a position FACT is how a monitor stops monitoring
// everything else". One broken mandate would stop every OTHER portfolio being
// evaluated.
//
// So this asserts the handler ACKS, and that it emits nothing: the portfolio must
// not be declared clean either.
func TestAnUnreadableMandateIsAckedRatherThanRedeliveredForever(t *testing.T) {
	reg := comp.NewMandateRegistry()

	// Feed the registry the way production does — a real ConfigChanged on the
	// mandate key whose value does not decode.
	ccBytes, err := proto.Marshal(&lifecyclepb.ConfigChanged{
		ConfigKey: comp.MandateConfigKey("t1", "p1"),
		NewValue:  "{not a mandate",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := comp.NewMandateConsumer(reg, nil).Handle(testCtx(), nil, ccBytes); err != nil {
		t.Fatal(err)
	}

	fb := &fakeBus{}
	m := NewMonitor(comp.NewEngine(nil), reg, nil, NewEmitter(fb), nil, nil)

	err = m.Handle(testCtx(), &envelopepb.Envelope{TenantId: "t1"},
		positionEvent(t, "AAPL", 100, 100000, t0.Add(time.Minute)))
	if err != nil {
		t.Fatalf("an unreadable mandate must be ACKED, not returned for redelivery — the stream is "+
			"compacted, so the retry can only fail the same way and it starves every other "+
			"portfolio of evaluation: %v", err)
	}
	if len(fb.events) != 0 {
		t.Fatalf("a portfolio whose mandate could not be read must not be evaluated or declared "+
			"clean, got %d event(s)", len(fb.events))
	}
}
