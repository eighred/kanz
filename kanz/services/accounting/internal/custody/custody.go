// Package custody is the custody reconciliation CONTROL (#962): the scheduled
// run, the break lifecycle and the telemetry around the comparison engine in
// services/accounting/internal/recon.
//
// # What was here before, and what was missing
//
// recon.Reconcile has compared the folded IBOR book against a custodian
// statement since IBOR-01e. The comparison is real, tested and correct. Its ONLY
// caller was an HTTP handler whose statement arrived in the REQUEST BODY, so the
// platform had a reconciliation ENGINE and no reconciliation CONTROL: an operator
// obtained the custodian's positions by hand, transcribed them into JSON and
// posted them — per portfolio, per custodian, per day — or did not, and nothing
// anywhere said so.
//
// # Why this is the control that guards every other number
//
// Custody reconciliation detects what nothing else can. Every other control on
// this platform READS THE BOOK — exposure, VaR, mandate limits, margin, NAV — so
// each one inherits whatever the book is wrong about, confidently and in the same
// direction. And the error is silent by construction: a position missing from the
// book contributes nothing to any measure, so no threshold is crossed and no
// alert fires. The custodian's statement is the only independent view of what is
// actually held.
//
// # The three things this package adds
//
//  1. A run happens on a SCHEDULE and emits a FACT whether or not it found
//     anything — including when no statement arrived at all (OutcomeNoStatement).
//     Before this, "reconciled clean today" and "nobody ran it" produced
//     identical telemetry, which is the "nothing configured and checked-and-fine
//     must never look the same" rule broken on the book of record itself.
//  2. A break is a WORKING ITEM with a stable identity, an owner and an age,
//     rather than a line in a response body. A break nobody can age is a break
//     nobody chases.
//  3. Both are observable: runs by outcome, open breaks by kind, and the age of
//     the newest run per (portfolio, custodian) — the series the staleness alert
//     reads, and the one that catches a custodian that quietly stopped sending.
//
// # What this package is NOT
//
// It is not a custodian feed adapter. Translating SWIFT MT535/MT940, a prime
// broker's SFTP CSV or a custody API into a canonical statement belongs in an
// adapter, for the same reason exchange protocol lives in the venue adapter and
// nowhere else — and credentials terminate there for the same reason too. Those
// are deferred to #105/#106, which have real statement feeds to develop against.
// This package takes canonical statements off the bus and does the control work.
package custody

import (
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"

	"github.com/eighred/kanz/services/accounting/internal/recon"
)

// SubjectStatement is where a custodian feed announces a statement, and
// SubjectRun is where this service records that a reconciliation happened.
// {domain}.{entity}.{event_type}, so each archives to an accounting.* Kafka topic
// alongside the balance and cash spaces that already exist.
const (
	SubjectStatement = "accounting.custody.statement"
	SubjectRun       = "accounting.custody.reconciled"
)

const (
	schemaRefStatement = "accounting.v1.CustodianStatement:1"
	schemaRefRun       = "accounting.v1.ReconciliationRun:1"
)

// Outcome is what a run concluded. It mirrors
// accounting.v1.ReconciliationOutcome; wireOutcome below is the one place the two
// are joined.
type Outcome int

const (
	// OutcomeUnspecified is the zero value and is never a legitimate conclusion.
	// It exists so a run that was never given one is detectable rather than
	// defaulting to the reassuring answer.
	OutcomeUnspecified Outcome = iota
	// OutcomeClean: the book and the statement agreed within tolerance.
	OutcomeClean
	// OutcomeBreaks: the run completed and found discrepancies.
	OutcomeBreaks
	// OutcomeNoStatement: the run was due and the custodian had said nothing for
	// that business date.
	//
	// THIS IS THE OUTCOME THAT MAKES ABSENCE LOUD. A custodian that stops sending
	// produced no event of any kind before #962, so the estate looked exactly
	// like one reconciling cleanly every day. Emitting this instead of staying
	// silent gives the gap a timestamp, a counter and an alert.
	OutcomeNoStatement
	// OutcomeFailed: the run could not complete. NOT a clean book, and the
	// FailureReason says why.
	OutcomeFailed
)

func (o Outcome) String() string {
	switch o {
	case OutcomeClean:
		return "clean"
	case OutcomeBreaks:
		return "breaks"
	case OutcomeNoStatement:
		return "no_statement"
	case OutcomeFailed:
		return "failed"
	default:
		return "unspecified"
	}
}

// Outcomes returns every legitimate outcome, in wire order.
//
// IT IS DERIVED-FROM RATHER THAN COPIED-INTO the metric registration (#806's
// lesson): a counter that only ever exports the labels it has actually seen makes
// "no failed runs" and "the failed path is unreachable" the same empty series, so
// the metric is pre-seeded from this list. A new outcome added above therefore
// gets its series without anybody remembering to add it in a second place.
func Outcomes() []Outcome {
	return []Outcome{OutcomeClean, OutcomeBreaks, OutcomeNoStatement, OutcomeFailed}
}

// wireOutcome renders an Outcome onto the wire.
func wireOutcome(o Outcome) accountingpb.ReconciliationOutcome {
	switch o {
	case OutcomeClean:
		return accountingpb.ReconciliationOutcome_RECONCILIATION_OUTCOME_CLEAN
	case OutcomeBreaks:
		return accountingpb.ReconciliationOutcome_RECONCILIATION_OUTCOME_BREAKS
	case OutcomeNoStatement:
		return accountingpb.ReconciliationOutcome_RECONCILIATION_OUTCOME_NO_STATEMENT
	case OutcomeFailed:
		return accountingpb.ReconciliationOutcome_RECONCILIATION_OUTCOME_FAILED
	default:
		return accountingpb.ReconciliationOutcome_RECONCILIATION_OUTCOME_UNSPECIFIED
	}
}

// wireKind renders a recon.BreakKind onto the wire.
//
// THE ENGINE'S SET AND THE WIRE'S SET ARE THE SAME SET, and this function is the
// single join between them. An unmapped kind returns ok=false rather than
// UNSPECIFIED, so a kind added to the engine and forgotten here surfaces as a
// refusal to publish rather than as a break that reaches an operator classified
// as nothing in particular. test/arch derives one set from the other so the
// omission is caught before it can happen at all.
func wireKind(k recon.BreakKind) (accountingpb.ReconciliationBreakKind, bool) {
	switch k {
	case recon.BreakQuantity:
		return accountingpb.ReconciliationBreakKind_RECONCILIATION_BREAK_KIND_QUANTITY, true
	case recon.BreakMissingAtCustodian:
		return accountingpb.ReconciliationBreakKind_RECONCILIATION_BREAK_KIND_MISSING_AT_CUSTODIAN, true
	case recon.BreakMissingInIBOR:
		return accountingpb.ReconciliationBreakKind_RECONCILIATION_BREAK_KIND_MISSING_IN_IBOR, true
	case recon.BreakCash:
		return accountingpb.ReconciliationBreakKind_RECONCILIATION_BREAK_KIND_CASH, true
	case recon.BreakExecutionMissingAtCustodian:
		return accountingpb.ReconciliationBreakKind_RECONCILIATION_BREAK_KIND_EXECUTION_MISSING_AT_CUSTODIAN, true
	case recon.BreakExecutionMissingInIBOR:
		return accountingpb.ReconciliationBreakKind_RECONCILIATION_BREAK_KIND_EXECUTION_MISSING_IN_IBOR, true
	default:
		return accountingpb.ReconciliationBreakKind_RECONCILIATION_BREAK_KIND_UNSPECIFIED, false
	}
}

// wireGrain renders a statement grain onto the wire, and grainFromWire reads one
// back. They are the ONE join between recon.Grain and
// accounting.v1.StatementGrain, for wireKind's reason.
//
// AN UNRECOGNISED WIRE VALUE BECOMES GrainUnknown RATHER THAN A GUESS. A newer
// producer asserting a grain this build does not know about must leave the
// transaction leg unrun and say so, not have its statement read at whichever
// grain happens to be adjacent — that is the "empty capability set is not
// unsupported" rule for a value nobody here can interpret.
func wireGrain(g recon.Grain) accountingpb.StatementGrain {
	switch g {
	case recon.GrainBalancesOnly:
		return accountingpb.StatementGrain_STATEMENT_GRAIN_BALANCES_ONLY
	case recon.GrainTransactions:
		return accountingpb.StatementGrain_STATEMENT_GRAIN_TRANSACTIONS
	default:
		return accountingpb.StatementGrain_STATEMENT_GRAIN_UNSPECIFIED
	}
}

func grainFromWire(g accountingpb.StatementGrain) recon.Grain {
	switch g {
	case accountingpb.StatementGrain_STATEMENT_GRAIN_BALANCES_ONLY:
		return recon.GrainBalancesOnly
	case accountingpb.StatementGrain_STATEMENT_GRAIN_TRANSACTIONS:
		return recon.GrainTransactions
	default:
		return recon.GrainUnknown
	}
}

// Statement is a custodian's asserted holdings for one portfolio on one business
// date — the independent side of the comparison.
//
// IT IS A LEVEL, exactly as PortfolioCashBalance is. The custodian states what it
// holds rather than what changed, so a redelivery is idempotent and a gap is
// self-correcting on the next statement. An instrument absent from Positions is
// a positive claim that the custodian does NOT hold it, which is what makes
// BreakMissingAtCustodian detectable at all.
type Statement struct {
	StatementID  string
	CustodianID  string
	PortfolioID  string
	BusinessDate time.Time
	Positions    map[string]*big.Rat // instrument -> quantity
	Cash         map[string]*big.Rat // currency -> balance
	ReceivedAt   time.Time

	// Transactions are the custodian's trade lines for the business date, and
	// Grain is what makes an EMPTY list readable (#1049).
	//
	// THEY ARE recon's TYPES AND NOT A THIRD COPY. The statement's grain, the
	// engine's grain and the wire's grain are one concept; a custody.Grain beside
	// recon.Grain would be the copied helper this repository keeps paying for,
	// and the two would drift the first time a value is added to one of them.
	Transactions []recon.Transaction
	Grain        recon.Grain
}

// Subject is the triple a scheduled reconciliation is defined over.
type Subject struct {
	PortfolioID  string
	CustodianID  string
	BusinessDate time.Time
}

// BusinessDay normalizes t to the UTC midnight that names its business date.
//
// EVERY DATE IN THIS PACKAGE GOES THROUGH HERE. The business date is an identity
// component — it keys the statement, the run and the break — so two spellings of
// the same day (one with a wall-clock time, one without) would reconcile the same
// day twice and age the breaks from the wrong instant. A comparison on
// time.Time.Equal is only safe because the values are normalized on the way in.
func BusinessDay(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// Key identifies the subject for storage and for a run id.
func (s Subject) Key() string {
	return s.PortfolioID + "|" + s.CustodianID + "|" + BusinessDay(s.BusinessDate).Format("2006-01-02")
}

// IDSeparator joins the components of a break id and a subject key.
//
// NOTHING THAT GOES INTO EITHER MAY CONTAIN IT, and that is CHECKED rather than
// assumed — see idComponent. A comment claiming a property the code does not
// enforce is how an identity bug survives review: with a portfolio "a|b" and a
// custodian "c", and another portfolio "a" with custodian "b|c", both produce the
// same break id prefix, so two funds' breaks would share one row and one age.
const IDSeparator = "|"

// idComponent refuses a value that would corrupt a derived identity.
func idComponent(field, value string) error {
	if strings.Contains(value, IDSeparator) {
		return fmt.Errorf("custody: %s %q contains %q, which is the id separator — "+
			"two different subjects would derive the same break id", field, value, IDSeparator)
	}
	return nil
}

// Validate refuses a subject that cannot identify a reconciliation.
func (s Subject) Validate() error {
	switch {
	case strings.TrimSpace(s.PortfolioID) == "":
		return errors.New("custody: subject has no portfolio_id")
	case strings.TrimSpace(s.CustodianID) == "":
		return errors.New("custody: subject has no custodian_id")
	case s.BusinessDate.IsZero():
		return errors.New("custody: subject has no business_date")
	}
	if err := idComponent("portfolio_id", s.PortfolioID); err != nil {
		return err
	}
	return idComponent("custodian_id", s.CustodianID)
}

// Validate refuses a statement the control cannot act on.
//
// IT REFUSES RATHER THAN REPAIRS, and specifically it does not substitute a
// received_at or a business date it could plausibly infer. A statement whose
// business date had to be guessed would reconcile a book against holdings from a
// day nobody can name, and the resulting run would be evidence of the wrong
// thing. Fail-closed on the identity of the comparison.
func (s Statement) Validate() error {
	if err := (Subject{PortfolioID: s.PortfolioID, CustodianID: s.CustodianID, BusinessDate: s.BusinessDate}).Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(s.StatementID) == "" {
		return errors.New("custody: statement has no statement_id")
	}
	if s.ReceivedAt.IsZero() {
		return errors.New("custody: statement has no received_at")
	}
	for instrument, qty := range s.Positions {
		if qty == nil {
			return fmt.Errorf("custody: statement position %q has no quantity", instrument)
		}
		// The instrument becomes a break's key, which becomes part of its derived
		// id. A separator here would collide two instruments onto one break — one
		// row, one age, one operator's investigation covering two differences.
		if err := idComponent("instrument_id", instrument); err != nil {
			return err
		}
	}
	for ccy, bal := range s.Cash {
		if bal == nil {
			return fmt.Errorf("custody: statement cash %q has no balance", ccy)
		}
		if err := idComponent("currency_code", ccy); err != nil {
			return err
		}
	}
	return s.validateTransactions()
}

// validateTransactions refuses a statement whose transaction grain cannot be
// acted on (#1049).
//
// THE CONTRADICTIONS ARE REFUSED RATHER THAN RESOLVED, and the reason is the same
// one Validate gives for the business date: a statement whose grain had to be
// guessed would reconcile executions against a claim nobody made. A producer that
// sends trade lines while asserting BALANCES_ONLY, or while asserting nothing at
// all, would have every one of those lines SILENTLY IGNORED by the transaction
// leg — a feed that looks wired, a control that compares nothing, and no
// difference visible from outside. That is precisely what this issue exists to
// abolish, so it is a refusal and therefore a DLQ, not a dropped field.
func (s Statement) validateTransactions() error {
	if len(s.Transactions) > 0 && s.Grain != recon.GrainTransactions {
		return fmt.Errorf("custody: statement %s carries %d transaction line(s) and declares grain %q — "+
			"the transaction leg runs ONLY on %q, so every one of those lines would be ignored with "+
			"nothing saying so. A feed that supplies trade lines must declare it",
			s.StatementID, len(s.Transactions), s.Grain, recon.GrainTransactions)
	}
	seen := make(map[string]struct{}, len(s.Transactions))
	for _, tx := range s.Transactions {
		ref := strings.TrimSpace(tx.ExternalRef)
		if ref == "" {
			return fmt.Errorf("custody: statement %s has a transaction with no external_ref — "+
				"there is nothing to match it against, and a feed that omits the field would "+
				"reconcile as though every trade in the book were missing", s.StatementID)
		}
		if _, dup := seen[ref]; dup {
			// TWO LINES FOR ONE REFERENCE IS AMBIGUOUS, exactly as two rows for
			// one instrument are: the pass matches on identity, and a reference
			// the custodian used twice makes "did both sides see this execution"
			// unanswerable rather than doubly answered.
			return fmt.Errorf("custody: statement %s names transaction reference %q twice", s.StatementID, ref)
		}
		seen[ref] = struct{}{}
		// The reference becomes a break's Key, which becomes part of its derived
		// id. A separator here would collide two executions onto one break — one
		// row, one age, one operator's investigation covering two differences.
		if err := idComponent("external_ref", ref); err != nil {
			return err
		}
	}
	return nil
}

// Subject returns the reconciliation subject this statement belongs to.
func (s Statement) Subject() Subject {
	return Subject{PortfolioID: s.PortfolioID, CustodianID: s.CustodianID, BusinessDate: BusinessDay(s.BusinessDate)}
}

// Run is the record that a reconciliation happened.
//
// EMITTED FOR EVERY OUTCOME, which is the entire reason it exists. The pre-#962
// design produced output only when a human ran the comparison AND it found
// something, so a clean book and an unreconciled one were the same observable
// event. The age of the newest run per (portfolio, custodian) is what the
// staleness alert reads.
type Run struct {
	RunID         string
	Subject       Subject
	Outcome       Outcome
	StatementID   string
	Breaks        []Break
	Tolerance     *big.Rat
	CompletedAt   time.Time
	FailureReason string
}

// Break is one discrepancy, carried as a working item rather than a log line.
type Break struct {
	// BreakID is stable ACROSS RUNS, derived from (portfolio, custodian, kind,
	// key) — see BreakID. The age depends on it: a break given a fresh id every
	// day would be permanently one day old.
	BreakID   string
	Kind      recon.BreakKind
	Key       string
	IBOR      *big.Rat
	Custodian *big.Rat
	Diff      *big.Rat

	Status      BreakStatus
	Assignee    string
	Explanation string
	Revision    int64

	// FirstSeenAt is when this break was first detected and is NEVER advanced by
	// a redetection; LastSeenAt is the completed_at of the most recent run that
	// still found it. Together they separate "disagreed once, three weeks ago"
	// from "has disagreed every day for three weeks", which are different
	// problems with different owners.
	FirstSeenAt     time.Time
	LastSeenAt      time.Time
	StatusChangedAt time.Time
}

// BreakID derives a break's stable cross-run identity.
//
// THE KIND IS PART OF THE IDENTITY, not just the key. A position that goes from
// "quantity disagrees by 5" to "the custodian does not hold it at all" is a
// materially different break about the same instrument, and collapsing the two
// would silently carry the age of the smaller problem onto the larger one.
//
// THE SEPARATOR IS ENFORCED, NOT ASSUMED. Subject.Validate and
// Statement.Validate both refuse a component containing IDSeparator, because a
// portfolio "a|b" with custodian "c" and a portfolio "a" with custodian "b|c"
// would otherwise derive the same id — two funds' breaks sharing one row, one
// age and one operator's investigation.
func BreakID(portfolioID, custodianID string, kind recon.BreakKind, key string) string {
	return strings.Join([]string{portfolioID, custodianID, kind.String(), key}, IDSeparator)
}

// FromRecon builds the working items for a run's detected discrepancies. now is
// the run's completion instant: a newly detected break is first seen then, and
// every break in the slice was last seen then.
//
// EVERY BREAK STARTS OPEN. Carrying a status forward from a previous run is the
// STORE's job (see Store.UpsertBreaks), because only the store knows whether this
// break has been seen before — and getting that backwards would reset an
// operator's assignment and explanation on every daily run, which is the failure
// that makes a break list unusable.
func FromRecon(subject Subject, detected []recon.Break, now time.Time) []Break {
	out := make([]Break, 0, len(detected))
	for _, b := range detected {
		out = append(out, Break{
			BreakID:         BreakID(subject.PortfolioID, subject.CustodianID, b.Kind, b.Key),
			Kind:            b.Kind,
			Key:             b.Key,
			IBOR:            b.IBOR,
			Custodian:       b.Custodian,
			Diff:            b.Diff,
			Status:          BreakOpen,
			FirstSeenAt:     now,
			LastSeenAt:      now,
			StatusChangedAt: now,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// Age is how long a break has been outstanding at now.
func (b Break) Age(now time.Time) time.Duration {
	if b.FirstSeenAt.IsZero() {
		return 0
	}
	return now.Sub(b.FirstSeenAt)
}
