package ledger

import (
	"math/big"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dec"
)

// SettlementBases and settlementBasisNames are what the accounting composition
// root enumerates to seed one posture series per basis (#1043). A basis declared
// in the iota block but missing from either would ship with NO SERIES AT ALL —
// absent rather than zero, the exact failure the posture exists to remove.
// settlementBasisCount is the sentinel that makes forgetting loud.
func TestSettlementBasesCoversEveryDeclaredBasis(t *testing.T) {
	if len(SettlementBases) != int(settlementBasisCount) {
		t.Fatalf("SettlementBases has %d entries but %d bases are declared — a new basis would be "+
			"seeded into no metric series at all; extend SettlementBases",
			len(SettlementBases), settlementBasisCount)
	}
	if len(settlementBasisNames) != int(settlementBasisCount) {
		t.Fatalf("settlementBasisNames has %d entries but %d bases are declared — an unnamed basis "+
			"renders as settlement_basis(N) in a metric label",
			len(settlementBasisNames), settlementBasisCount)
	}
	for i, b := range SettlementBases {
		if int(b) != i {
			t.Fatalf("SettlementBases[%d] = %d: the slice must be in declaration order so the "+
				"index is the value", i, b)
		}
	}
}

// A blank label value is indistinguishable from an unset one, and an
// out-of-range value is reached by a decoder reading a settlement_status column
// written by a newer build.
func TestSettlementBasisNamesAreUniqueNonEmptyAndSafe(t *testing.T) {
	seen := map[string]SettlementBasis{}
	for _, b := range SettlementBases {
		name := b.String()
		if name == "" {
			t.Fatalf("settlement basis %d has an empty name", b)
		}
		if prev, dup := seen[name]; dup {
			t.Fatalf("settlement bases %d and %d both render as %q", prev, b, name)
		}
		seen[name] = b
	}
	if got := SettlementUnknown.String(); got != "unknown" {
		t.Fatalf("SettlementUnknown.String() = %q, want unknown", got)
	}
	if got := SettlementBasis(99).String(); got != "settlement_basis(99)" {
		t.Fatalf("SettlementBasis(99).String() = %q, want settlement_basis(99)", got)
	}
}

// THE ZERO VALUE MUST BE UNKNOWN, NOT SETTLED. This is the one-line inversion the
// whole axis rests on: if the zero value meant "settles today", every producer
// that says nothing — the cash consumer, a corporate action, an accrual, any
// future one — would silently assert finality it never established, and the
// settled book would be exactly the old, wrong, everything-is-settled book with a
// new name.
func TestTheZeroSettlementBasisIsUnknown(t *testing.T) {
	var e Event
	if e.SettlementBasis != SettlementUnknown {
		t.Fatalf("the zero SettlementBasis is %v, want SettlementUnknown — a producer that asserts "+
			"nothing must not be read as asserting settlement", e.SettlementBasis)
	}
	if !e.SettlementDate.IsZero() {
		t.Fatal("the zero SettlementDate must be the zero time: an unasserted settlement date " +
			"defaulted to anything is a date nobody established")
	}
}

func settledTrade(id, instrument string, qty, price, cash int64, eff time.Time) *Event {
	e := tradedEntry(id, instrument, qty, price, cash, eff)
	e.SettlementBasis = SettlementSettled
	e.SettlementDate = eff
	return e
}

func tradedEntry(id, instrument string, qty, price, cash int64, eff time.Time) *Event {
	return &Event{
		EntryID:      id,
		PortfolioID:  "PF",
		Type:         EntryTrade,
		InstrumentID: instrument,
		Quantity:     big.NewRat(qty, 1),
		Price:        big.NewRat(price, 1),
		Cash:         big.NewRat(cash, 1),
		CashCurrency: "USD",
		Effective:    eff,
		Knowledge:    eff,
		SourceRef:    id,
	}
}

// AN ENTRY THAT ASSERTS NOTHING IS TRADED, NEVER SETTLED, AND THE BOOK SAYS SO.
//
// This is the fail-closed half of the axis. An unknown basis must land in the
// traded fold (the portfolio really did trade it) and stay out of the settled
// fold (nobody established that it moved), and the book must report itself unable
// to answer a settled-basis question rather than handing back a lower bound a
// buying-power gate would spend against.
func TestAnUnassertedBasisIsTradedButNotSettled(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	b := NewBook("PF")
	b.Apply(tradedEntry("e1", "BTC-USD", 2, 100, -200, now))

	if got := b.Positions["BTC-USD"].Qty; got.Cmp(big.NewRat(2, 1)) != 0 {
		t.Fatalf("traded qty = %s, want 2", got.RatString())
	}
	if _, ok := b.SettledPositions["BTC-USD"]; ok {
		t.Fatal("an entry that asserted NO settlement basis reached the settled book — the " +
			"unknown third value collapsed to 'settled', which is the defect the axis exists to " +
			"remove: the fund is reported as owning a position nobody said had moved")
	}
	if got := b.SettledCash["USD"]; got != nil {
		t.Fatalf("settled cash = %s for an entry with no asserted basis, want absent", got.RatString())
	}
	if b.SettlementBasisComplete() {
		t.Fatal("SettlementBasisComplete() = true with an unasserted entry folded — a caller would " +
			"read the empty settled book as 'nothing has settled' when the truth is 'nobody said'")
	}
	if got := b.UnknownSettlementEntries(); got != 1 {
		t.Fatalf("UnknownSettlementEntries() = %d, want 1", got)
	}
}

// A PENDING ENTRY IS AN ANSWER, NOT A GAP. It stays out of the settled book like
// an unknown one, but it does NOT make the book incomplete: the difference
// between "traded, settles Thursday" and "nobody said" is the difference between
// a number a control can act on and one it must refuse.
func TestAPendingEntryIsTradedNotSettledAndDoesNotBreakCompleteness(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	b := NewBook("PF")
	pending := tradedEntry("e1", "BTC-USD", 2, 100, -200, now)
	pending.SettlementBasis = SettlementPending
	pending.SettlementDate = now.Add(48 * time.Hour)
	b.Apply(pending)
	b.Apply(settledTrade("e2", "ETH-USD", 3, 10, -30, now))

	if _, ok := b.SettledPositions["BTC-USD"]; ok {
		t.Fatal("a PENDING position reached the settled book: the fund does not own it yet")
	}
	if got := b.SettledPositions["ETH-USD"].Qty; got.Cmp(big.NewRat(3, 1)) != 0 {
		t.Fatalf("settled qty for the settled entry = %s, want 3", got.RatString())
	}
	if got := b.SettledCash["USD"]; got == nil || got.Cmp(big.NewRat(-30, 1)) != 0 {
		t.Fatalf("settled cash = %v, want -30 (only the settled leg)", got)
	}
	if got := b.Cash["USD"]; got.Cmp(big.NewRat(-230, 1)) != 0 {
		t.Fatalf("traded cash = %s, want -230 (both legs)", got.RatString())
	}
	if !b.SettlementBasisComplete() {
		t.Fatal("SettlementBasisComplete() = false with every entry asserting a basis — pending is " +
			"an answer, and treating it as a gap would make the settled read permanently unusable")
	}
	if got := b.PendingSettlementEntries(); got != 1 {
		t.Fatalf("PendingSettlementEntries() = %d, want 1", got)
	}
}

// The corporate-action fold runs against the basis the entry belongs to, not
// against the traded book twice. A split of a SETTLED holding scales both views;
// the same split on an unasserted holding scales only the traded one.
func TestCorporateActionFoldsIntoTheBasisItBelongsTo(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	b := NewBook("PF")
	b.Apply(settledTrade("e1", "AAPL", 10, 150, -1500, now))

	split := &Event{
		EntryID:         "ca1",
		PortfolioID:     "PF",
		Type:            EntryCorporateAction,
		InstrumentID:    "AAPL",
		Action:          &Action{Kind: CorpActSplit, Ratio: big.NewRat(2, 1)},
		Effective:       now.Add(time.Hour),
		Knowledge:       now.Add(time.Hour),
		SettlementBasis: SettlementSettled,
	}
	b.Apply(split)

	if got := b.Positions["AAPL"].Qty; got.Cmp(big.NewRat(20, 1)) != 0 {
		t.Fatalf("traded qty after split = %s, want 20", got.RatString())
	}
	if got := b.SettledPositions["AAPL"].Qty; got.Cmp(big.NewRat(20, 1)) != 0 {
		t.Fatalf("settled qty after split = %s, want 20 — the settled book must see the same "+
			"action, folded against its own holding", got.RatString())
	}
	if got := b.SettledPositions["AAPL"].AvgCost; got.Cmp(big.NewRat(75, 1)) != 0 {
		t.Fatalf("settled avg cost after split = %s, want 75", got.RatString())
	}
}

// An accrual has no settled twin: accrued income is earned-not-received, so
// folding it into a settled balance would assert cash the fund has not been paid.
func TestAccrualNeverReachesTheSettledCash(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	b := NewBook("PF")
	b.Apply(&Event{
		EntryID:         "acc1",
		PortfolioID:     "PF",
		Type:            EntryAccrual,
		Cash:            big.NewRat(25, 1),
		CashCurrency:    "USD",
		Effective:       now,
		Knowledge:       now,
		SettlementBasis: SettlementSettled, // even asserting settled, an accrual is not received
	})
	if got := b.AccruedBalance("USD"); got.Cmp(big.NewRat(25, 1)) != 0 {
		t.Fatalf("accrued = %s, want 25", got.RatString())
	}
	if got := b.SettledCash["USD"]; got != nil {
		t.Fatalf("settled cash = %s for an ACCRUAL — earned-not-received cash was reported as "+
			"money in the account", got.RatString())
	}
}

// FromFill is the one place the venue convention is asserted, and it must assert
// it rather than leave the fill unknown: an unknown fill would put every trade
// this platform books outside the settled view, which is a different wrong answer
// from the one being fixed.
func TestFromFillAssertsTheVenueSettlementConvention(t *testing.T) {
	executed := time.Unix(1_700_000_000, 0).UTC()
	fill := &orderpb.Fill{
		FillId:       "F-1",
		Venue:        "BINANCE",
		OrderId:      "O-1",
		InstrumentId: "BTC-USD",
		Side:         orderpb.Side_SIDE_BUY,
		Quantity:     dec.ToProto(big.NewRat(1, 1)),
		Price:        dec.ToProto(big.NewRat(50000, 1)),
		ExecutedAt:   timestamppb.New(executed),
	}
	e, err := FromFill("PF", fill, "USD", executed.Add(time.Second))
	if err != nil {
		t.Fatalf("FromFill: %v", err)
	}
	if e.SettlementBasis != SettlementSettled {
		t.Fatalf("fill settlement basis = %v, want SettlementSettled — spot on both wired venues "+
			"settles atomically with the match, and fillSettlement is where that is asserted",
			e.SettlementBasis)
	}
	if !e.SettlementDate.Equal(executed) {
		t.Fatalf("fill settlement date = %s, want the execution time %s", e.SettlementDate, executed)
	}
}

// A checkpoint must carry BOTH bases. Without this, MaterializeCurrent would
// restore a book whose settled balances are short by everything the checkpoint
// absorbed — a wrong number that looks exactly like a portfolio whose trades had
// genuinely not settled.
func TestSnapshotRoundTripCarriesTheSettledFold(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	b := NewBook("PF")
	b.Apply(settledTrade("e1", "BTC-USD", 2, 100, -200, now))
	b.Apply(tradedEntry("e2", "ETH-USD", 5, 10, -50, now)) // unasserted

	restored := RestoreBook(b.Snapshot(now))
	if got := restored.SettledPositions["BTC-USD"].Qty; got.Cmp(big.NewRat(2, 1)) != 0 {
		t.Fatalf("restored settled qty = %s, want 2", got.RatString())
	}
	if _, ok := restored.SettledPositions["ETH-USD"]; ok {
		t.Fatal("the unasserted holding came back inside the settled view")
	}
	if restored.SettlementBasisComplete() {
		t.Fatal("the restored book claims a complete settled basis, but the checkpoint carries an " +
			"unasserted entry — the count did not survive the round trip, so every restart would " +
			"launder an unknown into a clean bill of health")
	}
	if got := restored.UnknownSettlementEntries(); got != 1 {
		t.Fatalf("restored UnknownSettlementEntries() = %d, want 1", got)
	}

	// The copy must be deep on both bases: mutating the book afterwards must not
	// reach through into the restored one.
	b.SettledPositions["BTC-USD"].Qty.SetInt64(99)
	if got := restored.SettledPositions["BTC-USD"].Qty; got.Cmp(big.NewRat(2, 1)) != 0 {
		t.Fatalf("restored settled qty = %s after mutating the source book — the settled fold "+
			"shares a *big.Rat with the book it was detached from", got.RatString())
	}
}

// A CHECKPOINT WRITTEN BEFORE THE AXIS EXISTED STATES NOTHING, AND A BOOK
// RESTORED FROM IT MUST NOT ANSWER A SETTLED QUESTION. Its settled maps are empty
// because nobody wrote them, not because nothing settled — the same distinction
// MaxEffective already draws for a fenceless checkpoint.
func TestABookRestoredFromAnUnstatedCheckpointFailsClosed(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	legacy := &Snapshot{
		PortfolioID:  "PF",
		Positions:    map[string]*Position{"BTC-USD": {Qty: big.NewRat(2, 1), AvgCost: big.NewRat(100, 1), Realized: new(big.Rat)}},
		Cash:         map[string]*big.Rat{"USD": big.NewRat(-200, 1)},
		Accrued:      map[string]*big.Rat{},
		Through:      now,
		MaxEffective: now,
		// SettlementStated deliberately false: this is what LoadSnapshot produces
		// for a row whose settled_positions column is NULL.
	}
	restored := RestoreBook(legacy)
	if restored.SettlementBasisComplete() {
		t.Fatal("a book restored from a checkpoint that states no settled view reports a COMPLETE " +
			"settled basis. Its settled maps are empty because the row predates the axis, so any " +
			"control reading them would be told the portfolio has settled nothing at all — and " +
			"would be told it with the same confidence as a real answer")
	}
	if got := restored.Positions["BTC-USD"].Qty; got.Cmp(big.NewRat(2, 1)) != 0 {
		t.Fatalf("the traded fold must survive unchanged: qty = %s, want 2", got.RatString())
	}
}
