package order

import (
	"strings"
	"testing"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// HOW LONG HAS THIS ORDER BEEN FROZEN?
//
// OrderQuarantine.at is documented Required, and — like its sibling
// last_query_at, whose own comment says it exists "so an operator can tell a
// fresh contradiction from a stale one" — nothing read it back. A cancel refused
// on an order frozen thirty seconds ago and one frozen three weeks ago produced
// the SAME message.
//
// The difference is the whole triage. A freeze minutes old is probably the
// incident in progress; one weeks old is a position whose true size nobody has
// established since, in a book that has been traded around it.

var qNow = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

func TestQuarantineAge_RendersHowLongItHasBeenFrozen(t *testing.T) {
	q := &orderpb.OrderQuarantine{At: timestamppb.New(qNow.Add(-90 * time.Minute))}

	got := quarantineAge(q, qNow)
	if got != "1h30m0s" {
		t.Fatalf("age = %q, want 1h30m0s", got)
	}
}

// AN UNSET TIMESTAMP IS "unknown", NEVER A DURATION FROM THE EPOCH.
//
// A quarantine whose timestamp the producer never set would otherwise report
// itself as fifty-six years old — which is worse than saying nothing, because it
// is a number and numbers get believed. Same rule as the arrival mark and the
// trade count: unknown is not zero, and it is certainly not 1970.
func TestQuarantineAge_UnsetIsUnknownNotFiftySixYears(t *testing.T) {
	if got := quarantineAge(&orderpb.OrderQuarantine{}, qNow); got != "unknown" {
		t.Fatalf("age = %q for an unset timestamp, want \"unknown\" — an epoch-derived duration "+
			"reads as a real age", got)
	}
}

// A FREEZE STAMPED IN THE FUTURE SAYS SO. Clock skew between pods is real, and a
// negative duration is something an operator has to stop and interpret.
func TestQuarantineAge_AFutureStampIsReportedNotNegative(t *testing.T) {
	q := &orderpb.OrderQuarantine{At: timestamppb.New(qNow.Add(time.Hour))}

	got := quarantineAge(q, qNow)
	if !strings.Contains(got, "unknown") || !strings.Contains(got, "future") {
		t.Fatalf("age = %q, want it to name the skew rather than render a negative duration", got)
	}
}

// THE REFUSAL AN OPERATOR READS CARRIES BOTH HALVES.
//
// The message already carried the reason, deliberately — "so the operator does
// not need a second lookup to learn what to do next". The age is the other half
// of that sentence, and without it the lookup is still required.
func TestQuarantineRefusal_CarriesReasonAndAge(t *testing.T) {
	// Rendered the way the refusal does, to pin the pair rather than the wording.
	q := &orderpb.OrderQuarantine{
		At:     timestamppb.New(qNow.Add(-72 * time.Hour)),
		Reason: "venue denied an order it had acknowledged",
	}
	age := quarantineAge(q, qNow)
	if age != "72h0m0s" {
		t.Fatalf("age = %q, want 72h0m0s", age)
	}
	if q.GetReason() == "" {
		t.Fatal("the reason is empty — the refusal would name neither what happened nor when")
	}
}
