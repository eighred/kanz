package consume

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

// THE BOOK OF RECORD ANNOUNCES WHAT A PORTFOLIO CAN SPEND (#450).
//
// The pre-trade buying-power gate (#415) and exchange balance reconciliation
// (#418) both need a cash balance, and neither may compute one: the ledger is
// the only component that folds trade legs, cash movements and corporate actions
// bitemporally with restatements. Two independent computations of one number
// drift, and the drift surfaces as a trading control that refuses or admits
// wrongly.

type capturingPublisher struct {
	events []bus.Event
	err    error
}

func (p *capturingPublisher) Publish(_ context.Context, e bus.Event) error {
	if p.err != nil {
		return p.err
	}
	p.events = append(p.events, e)
	return nil
}

func (p *capturingPublisher) last() *accountingpb.PortfolioCashBalance {
	if len(p.events) == 0 {
		return nil
	}
	msg, _ := p.events[len(p.events)-1].Payload.(*accountingpb.PortfolioCashBalance)
	return msg
}

// testPosture is what a deployment with the shipped feeds looks like: fills,
// cash and fees are fed; corporate actions and accruals have no producer
// ANYWHERE in this platform (#588). Every announcer under test states it,
// because a test whose producer says nothing would exercise the one path
// production must never take.
var testPosture = EntrySourcePosture{
	Produced:   []string{"trade", "cash", "fee"},
	Unproduced: []string{"corporate_action", "accrual"},
}

func announcerOver(t *testing.T, pub Publisher) (*Announcer, ledger.Store) {
	t.Helper()
	st := ledger.NewMemoryStore()
	at := time.Unix(1_700_000_000, 0).UTC()
	return NewAnnouncer(st, pub, "USD", testPosture, nil, func() time.Time { return at }), st
}

func cashEntry(id, portfolio, account, ccy string, amount int64) *ledger.Event {
	at := time.Unix(1_700_000_000, 0).UTC()
	return &ledger.Event{
		EntryID: id, PortfolioID: portfolio, VenueAccountID: account,
		Type: ledger.EntryCash, Cash: big.NewRat(amount, 1), CashCurrency: ccy,
		Effective: at, Knowledge: at,
	}
}

func TestAnnounce_PublishesTheSpendableTotal(t *testing.T) {
	pub := &capturingPublisher{}
	a, st := announcerOver(t, pub)
	ctx := context.Background()

	for _, e := range []*ledger.Event{
		cashEntry("c:1", "PF1", "", "USD", 1000),
		cashEntry("c:2", "PF1", "", "USD", -250),
	} {
		if err := st.Append(ctx, e); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if err := a.Announce(ctx, "PF1"); err != nil {
		t.Fatalf("Announce: %v", err)
	}

	msg := pub.last()
	if msg == nil {
		t.Fatal("nothing was published")
	}
	if got := dec.FromProto(msg.GetTotal()); got.Cmp(big.NewRat(750, 1)) != 0 {
		t.Fatalf("total = %s, want 750 (1000 − 250)", got.RatString())
	}
	if msg.GetBaseCurrency() != "USD" || msg.GetPortfolioId() != "PF1" {
		t.Fatalf("wrong identity on the FACT: %+v", msg)
	}
	if msg.GetAsOf() == nil || msg.GetKnowledgeTime() == nil {
		t.Fatal("no as_of/knowledge_time — a consumer cannot age an unbounded balance out to UNKNOWN")
	}
	if got := pub.events[0].Subject; got != SubjectPortfolioCash {
		t.Fatalf("subject = %q, want %q", got, SubjectPortfolioCash)
	}
	// PARTITIONED BY PORTFOLIO, so a portfolio's announcements stay ordered and a
	// consumer never folds an older level over a newer one.
	if got := pub.events[0].PartitionKey; got != "PF1" {
		t.Fatalf("partition key = %q, want the portfolio id", got)
	}
}

// THE PER-ACCOUNT HALF IS CARRIED ON THE SAME FACT (#418). An exchange margins
// and liquidates per ACCOUNT, so the total is the wrong number to reconcile
// against a venue — it includes the fund's own bank and merges accounts the
// exchange treats separately.
func TestAnnounce_CarriesPerVenueAccountBalances(t *testing.T) {
	pub := &capturingPublisher{}
	a, st := announcerOver(t, pub)
	ctx := context.Background()

	for _, e := range []*ledger.Event{
		cashEntry("c:1", "PF1", "okx-sub-1", "USDT", 140),
		cashEntry("c:2", "PF1", "binance-alpha", "USDT", 700),
		// Declares it touched NO exchange account: the fund's own bank.
		cashEntry("c:3", "PF1", "", "USD", 1_000_000),
	} {
		if err := st.Append(ctx, e); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if err := a.Announce(ctx, "PF1"); err != nil {
		t.Fatalf("Announce: %v", err)
	}

	got := map[string]string{}
	for _, b := range pub.last().GetByVenueAccount() {
		got[b.GetVenueAccountId()+"/"+b.GetAsset()] = dec.FromProto(b.GetAmount()).RatString()
	}
	if got["okx-sub-1/USDT"] != "140" || got["binance-alpha/USDT"] != "700" {
		t.Fatalf("per-account balances = %v, want okx-sub-1/USDT=140 and binance-alpha/USDT=700", got)
	}
	if _, ok := got["/USD"]; ok {
		t.Error(`the unscoped entry appeared as an exchange account — it declared it touched none, ` +
			`and inventing one would report the fund's uninvested cash as exchange collateral`)
	}
	// SORTED, so a republish of an unchanged book is byte-stable rather than
	// looking like a change to anything diffing announcements.
	accounts := pub.last().GetByVenueAccount()
	for i := 1; i < len(accounts); i++ {
		if accounts[i-1].GetVenueAccountId() > accounts[i].GetVenueAccountId() {
			t.Fatalf("by_venue_account is not sorted: %v", got)
		}
	}
}

// A NIL PUBLISHER ANNOUNCES NOTHING AND IS NOT AN ERROR. The ledger is still
// correct; downstream simply has no balance, which the buying-power rule fails
// closed on. The composition root reports the posture at startup.
func TestAnnounce_NilPublisherIsInert(t *testing.T) {
	a, st := announcerOver(t, nil)
	ctx := context.Background()
	if err := st.Append(ctx, cashEntry("c:1", "PF1", "", "USD", 100)); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := a.Announce(ctx, "PF1"); err != nil {
		t.Fatalf("Announce with no publisher = %v, want nil", err)
	}
}

// A FAILED ANNOUNCEMENT MUST NOT FAIL THE FOLD.
//
// This is the property that keeps a broker blip from stalling the book of
// record. The ledger write has already committed and is authoritative; the
// announcement is derived. Returning the error to the bus would nack the message
// and re-fold it, forever, while the broker is unhappy.
func TestFolderKeepsFoldingWhenTheAnnouncementFails(t *testing.T) {
	st := ledger.NewMemoryStore()
	at := time.Unix(1_700_000_000, 0).UTC()
	pub := &capturingPublisher{err: errors.New("broker unhappy")}
	ann := NewAnnouncer(st, pub, "USD", testPosture, nil, func() time.Time { return at })
	f, err := NewFolder(testTenant, st, "USD", WithAnnouncer(ann))
	if err != nil {
		t.Fatalf("NewFolder: %v", err)
	}

	payload := cashPayload(t, "cash:S1", "PORT-1", accountingpb.EntryType_ENTRY_TYPE_CASH,
		decv(100, 0), "USD", at)
	env := &envelopepb.Envelope{
		EventType:     cashEventSubscription,
		TenantId:      testTenant,
		IngestionTime: timestamppb.New(at),
	}
	if err := f.HandleCash(context.Background(), env, payload); err != nil {
		t.Fatalf("HandleCash returned %v — a failed ANNOUNCEMENT nacked the message, so a broker "+
			"blip stalls the ledger. The fold is the thing that must not stop.", err)
	}

	// And the entry really did land, so this is not passing by not folding.
	book, _, err := ledger.MaterializeCurrent(context.Background(), st, "PORT-1")
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if got := book.CashBalance("USD"); got.Cmp(big.NewRat(100, 1)) != 0 {
		t.Fatalf("cash = %s, want 100 — the fold was skipped", got.RatString())
	}
}

// AND THE FOLD ANNOUNCES WHEN IT SUCCEEDS. Without this, "never fail the fold"
// is satisfied by never announcing at all.
func TestFolderAnnouncesAfterASuccessfulFold(t *testing.T) {
	st := ledger.NewMemoryStore()
	at := time.Unix(1_700_000_000, 0).UTC()
	pub := &capturingPublisher{}
	ann := NewAnnouncer(st, pub, "USD", testPosture, nil, func() time.Time { return at })
	f, err := NewFolder(testTenant, st, "USD", WithAnnouncer(ann))
	if err != nil {
		t.Fatalf("NewFolder: %v", err)
	}

	payload := cashPayload(t, "cash:S1", "PORT-1", accountingpb.EntryType_ENTRY_TYPE_CASH,
		decv(100, 0), "USD", at)
	env := &envelopepb.Envelope{
		EventType:     cashEventSubscription,
		TenantId:      testTenant,
		IngestionTime: timestamppb.New(at),
	}
	if err := f.HandleCash(context.Background(), env, payload); err != nil {
		t.Fatalf("HandleCash: %v", err)
	}
	msg := pub.last()
	if msg == nil {
		t.Fatal("a successful fold announced nothing — downstream never learns the balance changed")
	}
	if got := dec.FromProto(msg.GetTotal()); got.Cmp(big.NewRat(100, 1)) != 0 {
		t.Fatalf("announced total = %s, want 100", got.RatString())
	}
}
