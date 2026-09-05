package ledger

import "strconv"

// THE THIRD TEMPORAL AXIS (#1043).
//
// The journal already had two: Effective (when it economically happened) and
// Knowledge (when the book learned of it). Those are a RESTATEMENT axis — one
// event, two views of the same fact over time. Trade date versus settlement date
// is orthogonal to both: one uncontested event with TWO economic dates, the day
// it was traded and the day the asset and the cash actually change hands.
//
// Without it every fill was booked as fully settled at the instant of execution.
// There was no field anywhere that could express "I traded it, I do not yet own
// it", and every control that reads the book — NAV, exposure, leverage, mandate
// headroom, margin, and buying power through internal/cashview — inherited that
// assumption silently.

// SettlementBasis is whether an entry's legs have actually changed hands.
//
// UNKNOWN IS A REAL THIRD VALUE AND IT IS THE ZERO ONE, deliberately. A producer
// that does not assert a settlement basis must not read as "settles today": that
// is the platform rule that an empty capability set means "the venue did not
// assert support" rather than "unsupported", applied to money. An entry whose
// basis is unknown is folded into the TRADED book and kept out of the SETTLED
// one, and Book.SettlementBasisComplete reports the book as unable to answer a
// settled-basis question at all — a critical unknown fails closed rather than
// collapsing to zero and being spent.
type SettlementBasis int

const (
	// SettlementUnknown: nobody asserted when (or whether) this entry settles.
	// The zero value, so a producer that says nothing says UNKNOWN rather than
	// accidentally claiming finality.
	SettlementUnknown SettlementBasis = iota
	// SettlementSettled: the legs are final — the asset and the cash have moved.
	SettlementSettled
	// SettlementPending: traded, not yet settled. The position and cash legs are
	// contractually owed as of Event.SettlementDate and are in the traded book
	// only.
	SettlementPending

	// settlementBasisCount is the iota sentinel that makes the enumeration below
	// COUNTABLE, exactly as entryTypeCount does for EntryType: adding a basis
	// above it bumps this value and TestSettlementBasesCoversEveryDeclaredBasis
	// fails until SettlementBases and settlementBasisNames are extended too. Keep
	// it LAST.
	settlementBasisCount
)

// SettlementBases is every settlement basis the book can fold, in declaration
// order.
//
// IT EXISTS SO A CONSUMER CAN ENUMERATE THEM RATHER THAN HAND-LISTING THEM, for
// the reason EntryTypes does: the accounting composition root seeds one metric
// series per basis to say which of them anything actually PRODUCES, and a
// hand-written list there would mean a new basis shipped with no series at all —
// absent rather than zero — until its first posting. Derived from this slice, a
// new basis is seeded at 0 on the first scrape after it is declared.
var SettlementBases = []SettlementBasis{
	SettlementUnknown,
	SettlementSettled,
	SettlementPending,
}

// settlementBasisNames are the stable wire/label names for each basis. They are
// metric LABEL VALUES and a PromQL alert may name them, so they are frozen:
// renaming one silently breaks every query that used it.
var settlementBasisNames = [...]string{
	SettlementUnknown: "unknown",
	SettlementSettled: "settled",
	SettlementPending: "pending",
}

// String is the stable name of the basis. An undeclared value renders as
// settlement_basis(N) rather than panicking or rendering as empty: a metric
// label that came out blank would be indistinguishable from an unset one.
func (s SettlementBasis) String() string {
	if s < 0 || int(s) >= len(settlementBasisNames) {
		return "settlement_basis(" + strconv.Itoa(int(s)) + ")"
	}
	return settlementBasisNames[s]
}
