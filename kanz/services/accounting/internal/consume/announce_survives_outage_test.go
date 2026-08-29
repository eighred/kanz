package consume

import (
	"errors"
	"testing"
	"time"

	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

// #804'S "VERIFIED WHEN": WITH THE BROKER DOWN DURING ONE FOLD AND BACK UP
// AFTERWARDS, THE PORTFOLIO'S CASH FACT IS PUBLISHED WITHOUT ANY FURTHER FOLD.
//
// # The defect
//
// consume.Fold called store.Append and then announce, as two independent writes,
// and the announce error was deliberately discarded. That trade-off was right
// and is unchanged: the ledger is the book of record, the announcement is
// DERIVED, and nacking the fold to retry a publish would turn a broker blip into
// a stalled ledger. A consumer's staleness bound makes a lost announcement safe
// — it ages the balance out to UNKNOWN and the buying-power rule fails closed.
//
// What the trade-off left out was the RECOVERY, and there was none. The only
// thing that re-announced a portfolio was THE NEXT FOLD FOR THAT PORTFOLIO — the
// composition root says so in as many words, "every announcement is caused by a
// fold". No ticker, no compensator, no outbox.
//
// So one broker blip during one portfolio's fold refused EVERY order for that
// portfolio under a buying-power mandate, indefinitely, until unrelated activity
// happened to arrive. For a portfolio that trades a few times a day that is a
// trading outage measured in hours, produced by a transient the platform
// recovered from in seconds. Fail-closed is the right direction and was the
// wrong duration.
//
// # What this asserts
//
// The fold happens while the publisher is refusing. Nothing is published, and
// the fold still succeeds — the old guarantee, unchanged. The broker then comes
// back, and the RELAY publishes the level with no second fold anywhere. That
// last step is the whole issue.

// flakyPublisher refuses until it is opened, modelling a broker outage that ends.
//
// The refusal IS capturingPublisher.err — the package fixture already models an
// outage that way, and reusing it keeps one publisher double in this package
// rather than two that could drift about what "down" means.
type flakyPublisher struct{ capturingPublisher }

// publishNow ends the outage. Clearing err is the whole of it: capturingPublisher
// records the event only when err is nil.
func (p *flakyPublisher) publishNow() { p.err = nil }

func TestACashLevelSurvivesABrokerOutageWithNoFurtherFold(t *testing.T) {
	at := time.Unix(1_700_000_000, 0).UTC()
	pub := &flakyPublisher{}
	pub.err = errors.New("broker unavailable")

	st := ledger.NewMemoryStore()
	ann := NewAnnouncer(st, pub, "USD", testPosture, nil, func() time.Time { return at })

	var lost int
	f, err := NewFolder(testTenant, st, "USD",
		WithAnnouncer(ann),
		WithAnnounceFailureObserver(func() { lost++ }))
	if err != nil {
		t.Fatalf("NewFolder: %v", err)
	}

	// THE FOLD, WITH THE BROKER DOWN. It must still commit — that is the property
	// the discarded announce error was protecting, and it is not being traded away.
	ctx := foldCtx()
	if err := st.Append(ctx, cashEntry("c:1", "PF1", "ACC1", "USD", 1000), f.announcerFor("PF1")); err != nil {
		t.Fatalf("the fold failed because the broker was down: %v — the ledger is the book of "+
			"record and a broker blip must never stall it", err)
	}
	f.flush(ctx, "PF1")

	if len(pub.events) != 0 {
		t.Fatalf("premise broken: %d event(s) were published while the broker was down, so this "+
			"test never reproduced the outage", len(pub.events))
	}
	if lost != 1 {
		t.Fatalf("the failed announcement was counted %d time(s), want 1 — an operator learns "+
			"announcements are lagging from the counter, not from orders being refused", lost)
	}
	// The ledger really did move, so nothing below passes by not folding.
	book, _, err := ledger.MaterializeCurrent(ctx, st, "PF1")
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if got := book.CashBalance("USD"); got.RatString() != "1000" {
		t.Fatalf("cash = %s, want 1000 — the fold did not land", got.RatString())
	}

	// THE BROKER COMES BACK. No second fold, no new entry, no trade: exactly the
	// state a portfolio sits in after the blip, waiting for activity that may be
	// hours away.
	pub.publishNow()

	drained, err := f.Outbox().DrainOnce(ctx)
	if err != nil {
		t.Fatalf("relay pass: %v", err)
	}
	if drained == 0 {
		t.Fatal("the relay published nothing after the broker came back.\n\n" +
			"This is #804: the cash level for this portfolio is gone, and the ONLY thing that " +
			"will ever re-announce it is the next fold for this same portfolio. Until then every " +
			"order against it is refused under a buying-power mandate — a trading outage measured " +
			"in hours, produced by a transient the platform recovered from in seconds.")
	}

	msg := pub.last()
	if msg == nil {
		t.Fatal("the relay reported a publish and the publisher saw nothing")
	}
	if msg.GetPortfolioId() != "PF1" {
		t.Fatalf("published portfolio = %q, want PF1", msg.GetPortfolioId())
	}
	if got := msg.GetTotal(); got.GetCoefficient() == 0 {
		t.Fatal("the recovered announcement carries a zero total — a portfolio with no balance " +
			"has not been shown to be empty, and a zero reads as an account with nothing in it")
	}
}

// THE LEVEL IS THE ONE THE FOLD PRODUCED, NOT THE ONE THAT PRECEDED IT.
//
// The record is built INSIDE Append, before the commit. A balance read through
// the pool from in there would not see the uncommitted entry and would announce
// the previous level on every fold — a wrong number rather than a late one,
// which is worse than the defect being fixed. This is the arm that catches it.
func TestTheAnnouncedLevelIncludesTheEntryThatCausedIt(t *testing.T) {
	at := time.Unix(1_700_000_000, 0).UTC()
	pub := &capturingPublisher{}
	st := ledger.NewMemoryStore()
	ann := NewAnnouncer(st, pub, "USD", testPosture, nil, func() time.Time { return at })
	f, err := NewFolder(testTenant, st, "USD", WithAnnouncer(ann))
	if err != nil {
		t.Fatalf("NewFolder: %v", err)
	}

	ctx := foldCtx()
	if err := st.Append(ctx, cashEntry("c:1", "PF1", "ACC1", "USD", 400), f.announcerFor("PF1")); err != nil {
		t.Fatalf("append: %v", err)
	}
	f.flush(ctx, "PF1")

	msg := pub.last()
	if msg == nil {
		t.Fatal("nothing was published")
	}
	if got := msg.GetTotal(); got.GetCoefficient() == 0 {
		t.Fatalf("the announced level is %v — it was computed WITHOUT the entry that caused it, "+
			"so every announcement reports the balance as it was before the fold", got)
	}
}
