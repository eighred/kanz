package compliance

import (
	"context"
	"errors"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// AN ORDER THE GATE ADMITS MUST LEAVE A COMPLIANCE DECISION RECORD (#797).
//
// # What was wrong
//
// Evaluate had ten terminal return paths and exactly ONE g.record, on the
// full-evaluation path. Nine recorded nothing, and TWO of those returned
// Allowed: true — the ungoverned portfolio and the mandate that constrains
// nothing. For an order admitted that way the platform could not answer
// AGENTS.md's attributability requirement, "which mandate permitted it": nothing
// anywhere said a compliance decision had been made at all. The refusal paths
// were quieter but not better — an ORDER_REJECTED FACT says the ORDER was
// refused, not that the enforcement point decided anything.
//
// kanz_compliance_ungoverned_orders_total gives an AGGREGATE. A reviewer or a
// regulator holds ONE ORDER ID.
//
// # Why the table drives every path rather than the two the issue named
//
// Fixing the two admissions would have left seven refusals silent and, worse,
// left the NEXT short-circuit silent too. Recording now happens in a wrapper
// that owns the only exit, so this table's job is to prove that every path
// actually reaches it — including the two that must NOT (a transient failure
// decided nothing, and recording one would put a decision in the audit trail
// that no enforcement point ever made).

// erroringMandates returns a fixed error from the mandate lookup.
type erroringMandates struct{ err error }

func (m erroringMandates) Mandate(context.Context, string, string, time.Time) (*compliancepb.Mandate, Governance, error) {
	return nil, GovernanceUnspecified, m.err
}

// erroringBooks fails the book load — the transient shape.
type erroringBooks struct{ err error }

func (b erroringBooks) Book(context.Context, string) (*Book, error) { return nil, b.err }

// outOfDomainBook is a book whose own numbers cannot be valued. It reaches
// bookInDomain rather than the delta check, which is a different refusal.
func outOfDomainBook() *Book {
	b := currentBook()
	b.Positions[0].Quantity = &commonpb.Decimal{Coefficient: 1, Exponent: 2000000000}
	return b
}

// zeroRuleMandate is a mandate somebody published that constrains nothing —
// a DECISION, and deliberately not the same state as no mandate at all.
func zeroRuleMandate() *compliancepb.Mandate {
	m := mandate()
	m.EffectiveAt = timestamppb.New(t0)
	return m
}

func okDelta() OrderDelta {
	return OrderDelta{
		TenantID: "t1", PortfolioID: "p1", InstrumentID: "AAPL",
		SignedQuantity: dec(1, 0), Price: dec(1000, 0), Currency: "USD",
		OrderID: "o-797", Issuer: "user:alice", AsOf: t0,
	}
}

// gateWith wires a recording gate over whatever sources a case needs.
func gateWith(t *testing.T, books BookSource, mandates MandateSource, opts ...PreTradeOption) (*PreTradeGate, *recordingRecorder) {
	t.Helper()
	rec := &recordingRecorder{}
	return NewPreTradeGate(NewEngine(nil), books, mandates, nil, rec, nil, opts...), rec
}

// registryWith is the ordinary mandate source, optionally holding one mandate.
func registryWith(t *testing.T, m *compliancepb.Mandate) MandateSource {
	t.Helper()
	reg := NewMandateRegistry()
	if m != nil {
		mustPut(t, reg, m)
	}
	return reg
}

func TestEveryTerminalDecisionIsRecorded(t *testing.T) {
	books := MapBookSource{"p1": currentBook()}

	for _, tc := range []struct {
		name         string
		books        BookSource
		mandates     func(*testing.T) MandateSource
		opts         []PreTradeOption
		delta        func() OrderDelta
		wantAllowed  bool
		wantNotEval  string
		wantStatus   compliancepb.ComplianceStatus
		wantViolated bool // the mandate actually ran
	}{
		{
			name:     "an ungoverned portfolio is ADMITTED and the admission is recorded",
			books:    books,
			mandates: func(t *testing.T) MandateSource { return registryWith(t, nil) },
			delta:    okDelta,
			// THE HEADLINE. OMS_REQUIRE_MANDATE ships false, so this is the shipped
			// behaviour for every portfolio nobody has run kanz-mandate for.
			wantAllowed: true,
			wantNotEval: "MANDATE_MISSING",
			wantStatus:  compliancepb.ComplianceStatus_COMPLIANCE_STATUS_WARN,
		},
		{
			name:        "a mandate that constrains nothing ADMITS, and says which state it was",
			books:       books,
			mandates:    func(t *testing.T) MandateSource { return registryWith(t, zeroRuleMandate()) },
			delta:       okDelta,
			wantAllowed: true,
			// NOT "MANDATE_MISSING". Somebody decided to constrain nothing; nobody
			// deciding at all is a different state, and collapsing the two is the
			// EXEC-M14 failure this whole distinction exists for.
			wantNotEval: "MANDATE_HAS_NO_RULES",
			wantStatus:  compliancepb.ComplianceStatus_COMPLIANCE_STATUS_WARN,
		},
		{
			name:        "an ungoverned portfolio under a deny-by-default posture is REFUSED, and recorded",
			books:       books,
			mandates:    func(t *testing.T) MandateSource { return registryWith(t, nil) },
			opts:        []PreTradeOption{WithRequireMandate(true)},
			delta:       okDelta,
			wantAllowed: false,
			wantNotEval: "MANDATE_MISSING",
			wantStatus:  compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH,
		},
		{
			name:  "an out-of-domain order is refused before the mandate lookup, and recorded",
			books: books,
			mandates: func(t *testing.T) MandateSource {
				return erroringMandates{err: errors.New("must not be reached")}
			},
			delta: func() OrderDelta {
				d := okDelta()
				d.SignedQuantity = &commonpb.Decimal{Coefficient: 1, Exponent: 2000000000}
				return d
			},
			wantAllowed: false,
			wantNotEval: "NOTIONAL_UNREPRESENTABLE",
			wantStatus:  compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH,
		},
		{
			name:  "an unresolvable mandate tenant is refused, and recorded",
			books: books,
			mandates: func(t *testing.T) MandateSource {
				return erroringMandates{err: ErrMandateTenantUnresolved}
			},
			delta:       okDelta,
			wantAllowed: false,
			wantNotEval: "MANDATE_TENANT_UNRESOLVED",
			wantStatus:  compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH,
		},
		{
			name:  "an unreadable mandate is refused, and recorded",
			books: books,
			mandates: func(t *testing.T) MandateSource {
				return erroringMandates{err: ErrMandateUnreadable}
			},
			delta:       okDelta,
			wantAllowed: false,
			wantNotEval: "MANDATE_UNREADABLE",
			wantStatus:  compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH,
		},
		{
			name:     "an unpriced order is refused, and recorded",
			books:    books,
			mandates: func(t *testing.T) MandateSource { return registryWith(t, concentrationMandate(60)) },
			delta: func() OrderDelta {
				d := okDelta()
				d.Price = dec(0, 0)
				return d
			},
			wantAllowed: false,
			wantNotEval: "PRICE_UNAVAILABLE",
			wantStatus:  compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH,
		},
		{
			name:     "a book that cannot be valued is refused, and recorded",
			books:    MapBookSource{"p1": outOfDomainBook()},
			mandates: func(t *testing.T) MandateSource { return registryWith(t, concentrationMandate(60)) },
			delta:    okDelta,
			// The ORDER was fine and the BOOK was not — recorded under the same code
			// as any other valuation failure, because the operator action is the same.
			wantAllowed: false,
			wantNotEval: "NOTIONAL_UNREPRESENTABLE",
			wantStatus:  compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH,
		},
		{
			name:         "a fully evaluated order records the ENGINE's result, with no not-evaluated code",
			books:        books,
			mandates:     func(t *testing.T) MandateSource { return registryWith(t, concentrationMandate(60)) },
			delta:        okDelta,
			wantAllowed:  true,
			wantNotEval:  "",
			wantStatus:   compliancepb.ComplianceStatus_COMPLIANCE_STATUS_PASS,
			wantViolated: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, rec := gateWith(t, tc.books, tc.mandates(t), tc.opts...)

			got, err := g.Evaluate(context.Background(), tc.delta())
			if err != nil {
				t.Fatalf("Evaluate: %v — this case is supposed to be TERMINAL", err)
			}
			if got.Allowed != tc.wantAllowed {
				t.Fatalf("Allowed = %v, want %v", got.Allowed, tc.wantAllowed)
			}
			if len(rec.records) != 1 {
				t.Fatalf("the enforcement point that decides whether capital moves left %d "+
					"records, want 1.\n\nAn order the gate answered for and did not record is one "+
					"the platform cannot say it checked. For an ADMISSION that is AGENTS.md's "+
					"attributability requirement unanswerable: a reviewer holding this order id "+
					"asking which mandate permitted it gets silence.", len(rec.records))
			}
			r := rec.records[0]
			if r.NotEvaluated != tc.wantNotEval {
				t.Errorf("NotEvaluated = %q, want %q — the record has to say WHY nothing was "+
					"evaluated, or an admission nobody checked reads exactly like one that "+
					"passed every rule", r.NotEvaluated, tc.wantNotEval)
			}
			if r.Result.GetStatus() != tc.wantStatus {
				t.Errorf("status = %v, want %v", r.Result.GetStatus(), tc.wantStatus)
			}
			if r.Allowed != tc.wantAllowed {
				t.Errorf("the record says Allowed = %v while the gate answered %v", r.Allowed, tc.wantAllowed)
			}
			if r.Phase != PhasePreTrade {
				t.Errorf("phase = %q, want %q", r.Phase, PhasePreTrade)
			}
			if r.TenantID != "t1" {
				t.Errorf("tenant = %q, want t1 — a decision about one tenant's portfolio filed "+
					"under another is a value that is valid, not theirs, and undetectable "+
					"downstream", r.TenantID)
			}
			if r.OrderID != "o-797" {
				t.Errorf("order_id = %q, want o-797 — a decision that names no order cannot be "+
					"found by the reviewer who holds one", r.OrderID)
			}
			if r.Issuer != "user:alice" {
				t.Errorf("issuer = %q, want user:alice", r.Issuer)
			}
			// AsyncRecorder DROPS a record with no Result, silently but for one log
			// line, so a synthesised result is not cosmetic: without it these
			// records would be built and then thrown away at the recorder.
			if r.Result == nil {
				t.Fatal("the record carries no Result — AsyncRecorder refuses it and the decision " +
					"never reaches the audit trail")
			}
			if r.Result.GetPortfolioId() != "p1" {
				t.Errorf("result portfolio = %q, want p1", r.Result.GetPortfolioId())
			}
			if r.Result.GetEvaluatedAt() == nil {
				t.Error("the record carries no evaluated_at — AsyncRecorder stamps the event time " +
					"from it and the broker refuses an event without one")
			}
			// A SYNTHESISED RESULT MUST NOT CLAIM A RULE FIRED. Violations means "a
			// rule did not pass"; naming one here would put a rule id an examiner
			// can look up into a record where no rule ran.
			if !tc.wantViolated && len(r.Result.GetViolations()) != 0 {
				t.Errorf("a record for a path where NO rule ran carries %d violation(s) — that "+
					"names a rule in the audit trail that never executed",
					len(r.Result.GetViolations()))
			}
		})
	}
}

// A TRANSIENT FAILURE RECORDS NOTHING, and this is the arm that keeps the fix
// honest in the other direction.
//
// Nothing was decided: the book could not be read, the order will be
// redelivered, and it will be evaluated again. A record here would put a
// decision in the audit trail that no enforcement point ever made — and it would
// also make "recorded" trivially true for every call, which would leave the
// table above proving nothing about routing.
func TestATransientFailureRecordsNoDecision(t *testing.T) {
	boom := errors.New("book store unreachable")
	g, rec := gateWith(t, erroringBooks{err: boom}, registryWith(t, concentrationMandate(60)))

	got, err := g.Evaluate(context.Background(), okDelta())
	if !errors.Is(err, boom) {
		t.Fatalf("Evaluate = %v, want the transient error so the order is redelivered", err)
	}
	if got.Allowed {
		t.Error("a decision that could not be made reported Allowed")
	}
	if len(rec.records) != 0 {
		t.Fatalf("%d decision(s) were recorded for a check that never completed — the audit trail "+
			"now holds a verdict no enforcement point reached", len(rec.records))
	}
}

// EVERY FLAG THAT MEANS "NO RULE RAN" MUST HAVE A CODE.
//
// notEvaluatedCode reads the Decision's flags, and a flag it does not name falls
// into the UNCLASSIFIED arm. That arm exists so a new short-circuit is not filed
// under an existing reason — an audit trail that answers confidently and wrongly
// is worse than one that says it does not know — but reaching it in a SHIPPED
// path means somebody added a flag and not a code.
func TestEveryNotEvaluatedFlagHasItsOwnCode(t *testing.T) {
	for _, tc := range []struct {
		name string
		dec  Decision
		want string
	}{
		{"unvaluable", Decision{Unvaluable: true}, "NOTIONAL_UNREPRESENTABLE"},
		{"unscoped", Decision{Unscoped: true}, "MANDATE_TENANT_UNRESOLVED"},
		{"unreadable", Decision{Unreadable: true}, "MANDATE_UNREADABLE"},
		{"ungoverned", Decision{Ungoverned: true}, "MANDATE_MISSING"},
		{"unpriced", Decision{Unpriced: true}, "PRICE_UNAVAILABLE"},
		{"unconstrained", Decision{Unconstrained: true}, "MANDATE_HAS_NO_RULES"},
		// THE ARM THAT PROVES THE SWITCH DISCRIMINATES. A decision carrying no
		// flag at all is not classifiable, and must say so rather than borrow the
		// nearest code.
		{"no flag", Decision{}, NotEvaluatedUnclassified},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := notEvaluatedCode(tc.dec); got != tc.want {
				t.Fatalf("notEvaluatedCode = %q, want %q", got, tc.want)
			}
		})
	}
}
