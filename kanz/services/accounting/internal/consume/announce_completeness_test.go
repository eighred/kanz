package consume

import (
	"math/big"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

// AN ANNOUNCED LEVEL MUST SAY WHAT IT CONTAINS (#614).
//
// The ruling that only accounting may compute a balance is right and stands. But
// one of the three inputs it names has never arrived: nothing on this platform
// publishes a corporate action (#588), so every level this Announcer sends is
// missing the cash effect of every dividend, coupon and merger the fund has been
// party to. A consumer given only the number cannot tell it from a whole one,
// and the pre-trade gate downstream converts the gap into a refused order that
// reads as a spending limit.

func corpActDividend(id, portfolio, instrument, ccy string, perUnit *big.Rat, at time.Time) *ledger.Event {
	// Built as a ledger.Event rather than through corpact.CorporateAction.ToEntry
	// ON PURPOSE. corpact has NO importer anywhere in the module — that is the
	// whole of #588 — and it is tracked by darkPackageExempt, whose dead-entry arm
	// fires the moment anything imports it, tests included. A test import would
	// clear the exemption without wiring a feed, and take five claim-site
	// assertions down with it. This is the same entry that package builds.
	return &ledger.Event{
		EntryID: id, PortfolioID: portfolio, Type: ledger.EntryCorporateAction,
		InstrumentID: instrument,
		Action: &ledger.Action{
			Kind: ledger.CorpActDividend, PerUnit: perUnit, Currency: ccy,
		},
		Effective: at, Knowledge: at,
	}
}

func positionEntry(id, portfolio, instrument string, qty int64, at time.Time) *ledger.Event {
	return &ledger.Event{
		EntryID: id, PortfolioID: portfolio, Type: ledger.EntryTrade,
		InstrumentID: instrument, Quantity: big.NewRat(qty, 1), Price: big.NewRat(10, 1),
		Effective: at, Knowledge: at,
	}
}

// THE MUTATION #614 ASKS FOR: fold a dividend into a portfolio's ledger and
// assert the number the buying-power gate reads CHANGES.
//
// If it did not, this issue would be about a gate that reads something else and
// the whole chain would need re-tracing. It does: foldCorpAct pays the cash, the
// announcement carries it, and the only reason no production balance has ever
// moved this way is that nothing constructs the entry.
//
// Expected values derived independently of this code, from the definition
// (cash delta = quantity x per-unit, signed):
//
//	long  100 x 0.25 = +25  ⇒ 1000 → 1025
//	short −100 x 0.25 = −25 ⇒ 1000 →  975
func TestAnnounce_ADividendMovesTheAnnouncedBalance(t *testing.T) {
	for _, tc := range []struct {
		name string
		qty  int64
		want *big.Rat
	}{
		{"long is paid the dividend", 100, big.NewRat(1025, 1)},
		// AND THE SIGN FOLLOWS THE HOLDING, which #614's framing does not: a short
		// book PAYS the dividend, so the unfolded balance is OVERSTATED and the
		// gate fails OPEN on it. That is why no consumer may correct the number by
		// the omission — only report it.
		{"short pays the dividend", -100, big.NewRat(975, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pub := &capturingPublisher{}
			a, st := announcerOver(t, pub)
			ctx := foldCtx()
			at := time.Unix(1_700_000_000, 0).UTC()

			for _, e := range []*ledger.Event{
				positionEntry("t:1", "PF1", "AAPL", tc.qty, at),
				cashEntry("c:1", "PF1", "", "USD", 1000),
			} {
				if err := st.Append(ctx, e, nil); err != nil {
					t.Fatalf("append: %v", err)
				}
			}
			if err := announceVia(t, ctx, a, st, "PF1"); err != nil {
				t.Fatalf("Announce: %v", err)
			}
			before := dec.FromProto(pub.last().GetTotal())
			if before.Cmp(big.NewRat(1000, 1)) != 0 {
				t.Fatalf("pre-dividend total = %s, want 1000", before.RatString())
			}

			div := corpActDividend("ca:1", "PF1", "AAPL", "USD", big.NewRat(1, 4),
				at.Add(24*time.Hour))
			if err := st.Append(ctx, div, nil); err != nil {
				t.Fatalf("append dividend: %v", err)
			}
			if err := announceVia(t, ctx, a, st, "PF1"); err != nil {
				t.Fatalf("Announce: %v", err)
			}
			after := dec.FromProto(pub.last().GetTotal())
			if after.Cmp(tc.want) != 0 {
				t.Fatalf("post-dividend total = %s, want %s — the announced balance did not move "+
					"with the corporate action, so the gate is not reading what #614 claims it "+
					"reads", after.RatString(), tc.want.RatString())
			}
		})
	}
}

// AND THE ANNOUNCEMENT SAYS THE DIVIDEND WAS NEVER GOING TO ARRIVE (#614). The
// test above proves the fold works; this one proves the consumer is told it is
// never fed.
func TestAnnounce_CarriesTheDeploymentsEntrySourcePosture(t *testing.T) {
	pub := &capturingPublisher{}
	a, st := announcerOver(t, pub)
	ctx := foldCtx()
	if err := st.Append(ctx, cashEntry("c:1", "PF1", "", "USD", 1000), nil); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := announceVia(t, ctx, a, st, "PF1"); err != nil {
		t.Fatalf("Announce: %v", err)
	}
	cp := pub.last().GetCompleteness()
	if cp == nil {
		t.Fatal("the announcement carries no completeness — a consumer cannot tell this level " +
			"from a whole one, which is the whole of #614")
	}
	// Sorted, like by_venue_account, so the FACT is byte-stable for a given book:
	// an unsorted list makes every republish look like a change to anything
	// diffing them. testPosture is deliberately declared UNSORTED.
	if got := cp.GetProducedEntryTypes(); !sortedEqual(got, []string{"cash", "fee", "trade"}) {
		t.Errorf("produced_entry_types = %v, want [cash fee trade] sorted", got)
	}
	if got := cp.GetUnproducedEntryTypes(); !sortedEqual(got, []string{"accrual", "corporate_action"}) {
		t.Errorf("unproduced_entry_types = %v, want [accrual corporate_action] sorted", got)
	}
}

// A PRODUCER THAT WAS GIVEN NO POSTURE PUBLISHES NO STATEMENT, rather than an
// empty one that a consumer would read as a clean bill of health. proto3 cannot
// tell an empty repeated field from an absent one, so the message is omitted
// entirely and the consumer lands on "unstated".
func TestAnnounce_AnUnstatedPostureIsNotPublishedAsComplete(t *testing.T) {
	pub := &capturingPublisher{}
	st := ledger.NewMemoryStore()
	at := time.Unix(1_700_000_000, 0).UTC()
	a := NewAnnouncer(st, pub, "USD", EntrySourcePosture{}, nil, func() time.Time { return at })
	ctx := foldCtx()
	if err := st.Append(ctx, cashEntry("c:1", "PF1", "", "USD", 1000), nil); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := announceVia(t, ctx, a, st, "PF1"); err != nil {
		t.Fatalf("Announce: %v", err)
	}
	if cp := pub.last().GetCompleteness(); cp != nil {
		t.Fatalf("an unstated posture was published as %v — an empty statement reads to a "+
			"consumer as \"nothing is missing\", which is the false assurance the field exists "+
			"to abolish", cp)
	}
}

func sortedEqual(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
