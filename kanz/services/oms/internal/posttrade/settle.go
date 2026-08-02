package posttrade

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dec"
)

// Status is the settlement lifecycle state (mirrors settlement.v1.SettlementStatus).
type Status int

const (
	StatusUnspecified Status = iota
	StatusMatched
	StatusAffirmed
	StatusInstructed
	StatusSettled
	StatusFailed
)

func (s Status) String() string {
	switch s {
	case StatusMatched:
		return "matched"
	case StatusAffirmed:
		return "affirmed"
	case StatusInstructed:
		return "instructed"
	case StatusSettled:
		return "settled"
	case StatusFailed:
		return "failed"
	default:
		return "unspecified"
	}
}

// ErrIllegalTransition is returned when a settlement is moved between states the
// lifecycle does not allow.
var ErrIllegalTransition = errors.New("posttrade: illegal settlement transition")

// Settlement is the post-trade aggregate for one trade — the working shape behind
// settlement.v1.SettlementInstruction. It moves MATCHED → AFFIRMED → INSTRUCTED →
// SETTLED, or to the terminal FAILED. Quantity/Amount are exact (*big.Rat).
type Settlement struct {
	InstructionID string
	FillID        string
	InstrumentID  string
	Side          orderpb.Side
	Quantity      *big.Rat
	// Amount is the UNSIGNED cash that changes hands in Currency, fee included on
	// the side that bears it (buyer pays notional+fee, seller receives
	// notional−fee). Side, not the sign, says which way it moves.
	Amount         *big.Rat
	Currency       string
	Counterparty   string
	Custodian      string
	SettlementDate time.Time
	Status         Status
	FailReason     string
}

// NewInstruction builds the settlement instruction for an affirmed fill: it
// computes the cash leg and starts in AFFIRMED, ready to be instructed to the
// settlement venue. The settlement date is the agreed T+N date (from the matched
// confirmation).
//
// Amount is the CASH THAT CHANGES HANDS, unsigned; Side says which way. A buyer
// pays the notional PLUS the fee, a seller nets the fee OUT of the proceeds — so
// the fee's sign follows the side. It used to be added on both sides, which for a
// SELL of 100 @ 150 with a 7.50 fee instructed 15007.50 while the ledger booked
// +14992.50: a 15.00 disagreement between the settlement instruction and the book
// of record, i.e. a break the custodian raises and nobody can reconcile.
//
// A FEE NOT DENOMINATED IN currency IS REFUSED (#221). currency is the settlement
// currency; order.v1.Fill.fee carries its own, and a crypto venue charges in the
// base asset (OKX) or a discount token (Binance BNB). Netting those into a USD
// instruction would instruct a cash amount the counterparty never agreed to.
// Settling the fee in its own asset needs a second cash leg on the instruction —
// settlement.v1.SettlementInstruction has one Amount/Currency pair, so that is a
// schema change, not a code change.
func NewInstruction(instructionID string, fill *orderpb.Fill, counterparty, custodian, currency string, settlementDate time.Time) (*Settlement, error) {
	qty := dec.FromProto(fill.GetQuantity())
	price := dec.FromProto(fill.GetPrice())
	fee, err := dec.MoneyIn(fill.GetFee(), currency)
	if err != nil {
		return nil, fmt.Errorf("posttrade: fill %s fee is not in the settlement currency, so it cannot be netted into the instruction: %w",
			fill.GetFillId(), err)
	}
	amount := new(big.Rat).Mul(qty, price)
	if fill.GetSide() == orderpb.Side_SIDE_SELL {
		amount.Sub(amount, fee) // seller receives the notional net of the fee
	} else {
		amount.Add(amount, fee) // buyer pays the notional plus the fee
	}
	return &Settlement{
		InstructionID:  instructionID,
		FillID:         fill.GetFillId(),
		InstrumentID:   fill.GetInstrumentId(),
		Side:           fill.GetSide(),
		Quantity:       qty,
		Amount:         amount,
		Currency:       currency,
		Counterparty:   counterparty,
		Custodian:      custodian,
		SettlementDate: settlementDate,
		Status:         StatusAffirmed,
	}, nil
}

// Affirm moves a MATCHED settlement to AFFIRMED.
func (s *Settlement) Affirm() error { return s.transition(StatusMatched, StatusAffirmed) }

// Instruct moves an AFFIRMED settlement to INSTRUCTED (sent to the venue).
func (s *Settlement) Instruct() error { return s.transition(StatusAffirmed, StatusInstructed) }

// Settle moves an INSTRUCTED settlement to the terminal SETTLED.
func (s *Settlement) Settle() error { return s.transition(StatusInstructed, StatusSettled) }

// Fail moves a non-terminal settlement to the terminal FAILED with a reason. A
// fail may occur from any non-terminal state (a rejected affirmation, a
// custodian rejection, or an aged-out instruction).
func (s *Settlement) Fail(reason string) error {
	if s.Status == StatusSettled || s.Status == StatusFailed {
		return fmt.Errorf("%w: %s -> failed", ErrIllegalTransition, s.Status)
	}
	s.Status = StatusFailed
	s.FailReason = reason
	return nil
}

func (s *Settlement) transition(from, to Status) error {
	if s.Status != from {
		return fmt.Errorf("%w: %s -> %s (expected from %s)", ErrIllegalTransition, s.Status, to, from)
	}
	s.Status = to
	return nil
}

// SettlementVenue is the seam to the settlement system (DTCC/CSD/SWIFT). The
// SimSettlementVenue here settles an instruction once its settlement date is
// reached, so the whole affirm→instruct→settle path runs with no external
// dependency; a real DTCC/SWIFT adapter implements the same interface and is
// wired at the composition root (the OMS-01c Venue/DEBT-02 stance).
type SettlementVenue interface {
	// Name identifies the venue for FACTs/logs.
	Name() string
	// Submit instructs the settlement of s as of asOf and returns the resulting
	// status: SETTLED once the settlement date is reached, otherwise INSTRUCTED
	// (still pending). A transient venue error is returned for retry.
	Submit(ctx context.Context, s *Settlement, asOf time.Time) (Status, error)
}

// SimSettlementVenue is the in-process settlement simulation. It settles an
// instruction when asOf is at or after the settlement date, else leaves it
// INSTRUCTED (pending). Deterministic given asOf.
type SimSettlementVenue struct{ name string }

// NewSimSettlementVenue returns a simulation venue with the given name.
func NewSimSettlementVenue(name string) *SimSettlementVenue {
	if name == "" {
		name = "SIM"
	}
	return &SimSettlementVenue{name: name}
}

// Name returns the venue name.
func (v *SimSettlementVenue) Name() string { return v.name }

// Submit settles when the settlement date is reached, else stays pending.
func (v *SimSettlementVenue) Submit(_ context.Context, s *Settlement, asOf time.Time) (Status, error) {
	if s == nil {
		return StatusUnspecified, errors.New("posttrade: nil settlement")
	}
	if !asOf.Before(s.SettlementDate) {
		return StatusSettled, nil
	}
	return StatusInstructed, nil
}

// Instruct sends an AFFIRMED settlement to the venue and advances its status to
// the venue's result (INSTRUCTED while pending, SETTLED once the date is
// reached). It is the POST-01c driver tying the aggregate to the venue seam.
func Instruct(ctx context.Context, venue SettlementVenue, s *Settlement, asOf time.Time) error {
	if s.Status == StatusAffirmed {
		if err := s.Instruct(); err != nil {
			return err
		}
	}
	if s.Status != StatusInstructed {
		return fmt.Errorf("%w: cannot instruct from %s", ErrIllegalTransition, s.Status)
	}
	result, err := venue.Submit(ctx, s, asOf)
	if err != nil {
		return err
	}
	if result == StatusSettled {
		return s.Settle()
	}
	return nil
}
