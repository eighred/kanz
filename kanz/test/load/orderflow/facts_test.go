package main

// THE EXACTLY-ONCE ACCOUNTING, WHICH IS THE HALF A LATENCY HARNESS WOULD MISS.
//
// #865's point: none of the write path's failure modes is an error the client
// sees. The gateway answers 202 when the COMMAND is published, so an order that
// is never admitted, and an order that is announced TWICE because a redelivery
// overtook a still-running handler, both look identical in every HTTP-level
// metric. These are the four outcomes this harness must be able to tell apart.

import (
	"context"
	"testing"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
)

func newLedger(tag string) *ledger {
	return &ledger{seen: map[string]*factRec{}, runTag: tag}
}

func (l *ledger) fold(t *testing.T, kind, orderID string) {
	t.Helper()
	if err := l.handler(kind)(context.Background(),
		&envelopepb.Envelope{PartitionKey: orderID}, nil); err != nil {
		t.Fatalf("fold %s/%s: %v", kind, orderID, err)
	}
}

func TestTheLedgerTellsTheFourOutcomesApart(t *testing.T) {
	l := newLedger("aaaaaaaa")
	subs := []submission{
		{id: "aaaaaaaa-admitted", at: time.Now()},
		{id: "aaaaaaaa-refused", at: time.Now()},
		{id: "aaaaaaaa-twice", at: time.Now()},
		{id: "aaaaaaaa-silent", at: time.Now()},
	}
	l.fold(t, subjectAccepted, "aaaaaaaa-admitted")
	l.fold(t, subjectRejected, "aaaaaaaa-refused")
	l.fold(t, subjectAccepted, "aaaaaaaa-twice")
	l.fold(t, subjectAccepted, "aaaaaaaa-twice") // the redelivery-overtake shape

	st := l.await(context.Background(), subs, time.Now())
	if st.admitted != 2 {
		t.Errorf("admitted = %d, want 2", st.admitted)
	}
	if st.refused != 1 {
		t.Errorf("refused = %d, want 1 — a REFUSED order is not an admitted one, and a stage where "+
			"every order is refused measured the refusal path", st.refused)
	}
	if st.duplicated != 1 {
		t.Errorf("duplicated = %d, want 1. One intent that produced two announcements is the shape a "+
			"redelivery overtaking a running handler leaves behind, and it is the finding #865 says "+
			"a latency-only harness would miss", st.duplicated)
	}
	if st.missing != 1 {
		t.Errorf("missing = %d, want 1. A submission the gateway accepted and nothing ever announced "+
			"is invisible to an HTTP-only load test: every request succeeded and the platform "+
			"admitted nothing", st.missing)
	}
}

// ONLY THIS RUN'S ORDERS COUNT TOWARD THE OUTSTANDING DEPTH. The FACT stream is
// shared and retained for 24h, so a re-run replays the previous run's
// announcements; counting those would make the outstanding depth go NEGATIVE and
// hide a real backlog.
func TestAnotherRunsFactsDoNotCountAsThisRunsAnnouncements(t *testing.T) {
	l := newLedger("aaaaaaaa")
	l.fold(t, subjectAccepted, "aaaaaaaa-mine")
	l.fold(t, subjectAccepted, "bbbbbbbb-theirs")
	l.fold(t, subjectRejected, "cccccccc-older-still")

	if got := l.announced(); got != 1 {
		t.Errorf("announced = %d, want 1 — only the order carrying this run's tag is this run's", got)
	}
	// The other two are still FOLDED, because await must be able to resolve any id
	// it is handed; they are simply not counted as this run's.
	if _, ok := l.lookup("bbbbbbbb-theirs"); !ok {
		t.Error("a foreign order was dropped rather than folded-and-not-counted")
	}
}

// A FACT WITH NO ORDER ID IS IGNORED RATHER THAN COUNTED AS AN ANNOUNCEMENT. The
// emitter keys every lifecycle FACT's partition key on the order id, so an empty
// one is a malformed envelope — counting it would credit an announcement to
// nothing and hide a missing one.
func TestAFactCarryingNoOrderIDIsNotAnAnnouncement(t *testing.T) {
	l := newLedger("aaaaaaaa")
	l.fold(t, subjectAccepted, "")
	if got := l.announced(); got != 0 {
		t.Errorf("announced = %d, want 0", got)
	}
}
