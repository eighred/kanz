package consume

import (
	"errors"
	"testing"
)

// A LOST CASH-BALANCE ANNOUNCEMENT MUST BE COUNTED, NOT ONLY LOGGED (#622).
//
// Folder.announce swallows the publish error deliberately, and the reasoning
// holds: the ledger write has committed and is the book of record, the
// announcement is derived, and returning the error would nack the message and
// re-fold it — turning a broker blip into a stalled ledger.
//
// That comment ends "It is LOUD instead." Loud was a log line, so the claim
// rested on somebody scraping logs. Every sibling that made the same call added a
// counter.
//
// It matters because the downstream fallback is a REFUSAL. A consumer's
// staleness bound ages the balance out to UNKNOWN, and the buying-power rule
// fails closed on it — so the visible symptom of lost announcements is ORDERS
// BEING DENIED, and an operator should not have to work backwards from that to a
// broker problem.

func TestALostAnnouncementIsCounted(t *testing.T) {
	// The package fixture already models a broker outage through its err field.
	pub := &capturingPublisher{err: errors.New("broker unavailable")}
	ann, st := announcerOver(t, pub)

	var lost int
	f, err := NewFolder("t1", st, "USD",
		WithAnnouncer(ann),
		WithAnnounceFailureObserver(func() { lost++ }))
	if err != nil {
		t.Fatal(err)
	}

	// A fold that changes cash triggers the announcement.
	if err := st.Append(foldCtx(), cashEntry("e1", "PF1", "ACC1", "USD", 500), f.announcerFor("PF1")); err != nil {
		t.Fatal(err)
	}
	f.flush(foldCtx(), "PF1")

	if len(pub.events) != 0 {
		t.Fatal("premise broken: the publisher accepted the announcement, so nothing was lost")
	}
	if lost != 1 {
		t.Fatalf("announce-failure observer fired %d time(s), want 1 — a lost announcement ages a "+
			"portfolio's balance out to UNKNOWN and its orders start being refused", lost)
	}
}

// A SUCCESSFUL ANNOUNCEMENT COUNTS NOTHING.
func TestASuccessfulAnnouncementIsNotCountedAsLost(t *testing.T) {
	pub := &capturingPublisher{}
	ann, st := announcerOver(t, pub)

	var lost int
	f, err := NewFolder("t1", st, "USD",
		WithAnnouncer(ann),
		WithAnnounceFailureObserver(func() { lost++ }))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Append(foldCtx(), cashEntry("e1", "PF1", "ACC1", "USD", 500), f.announcerFor("PF1")); err != nil {
		t.Fatal(err)
	}
	f.flush(foldCtx(), "PF1")

	if lost != 0 {
		t.Fatalf("a successful announcement was counted as lost %d time(s)", lost)
	}
}

// The seam is optional: a deployment that forgets it still folds and still logs.
func TestTheAnnounceFailureObserverIsOptional(t *testing.T) {
	ann, st := announcerOver(t, &capturingPublisher{err: errors.New("broker unavailable")})
	f, err := NewFolder("t1", st, "USD", WithAnnouncer(ann))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Append(foldCtx(), cashEntry("e1", "PF1", "ACC1", "USD", 500), f.announcerFor("PF1")); err != nil {
		t.Fatal(err)
	}
	f.flush(foldCtx(), "PF1") // must not panic
}
