package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/optimization"
)

// /v1/orders COULD NOT WORK, AND THE REASON WAS THIS SERVICE'S CONFIG (#751).
//
// handlePropose ran Optimize then Rebalance inline — every step of
// optimization.Propose except the mandate check — so every proposal carried
// MandateStatus's zero value, MandateUnchecked, and bridge.ToOrders refused it.
// The route was kept and refused out loud rather than emitting capital commands
// certified by a check that never ran (#646), which was the right posture for a
// service with no mandate source: proposeRequest carried none, Server held no
// compliance.Engine and no compliance.MandateSource, and the composition root
// passed only WithAutoPublish.
//
// These tests pin the repair from both ends. The refusal must survive when
// nothing is wired — that is still the honest answer — and a wired gate must
// produce a verdict that is EARNED: a book the mandate permits comes back
// feasible, and one it does not comes back infeasible with the violation, from
// the same engine the OMS pre-trade gate runs.

// staticMandates is a MandateSource over one mandate, keyed by tenant so a test
// cannot accidentally prove the tenant-blind behaviour #243 was filed for.
type staticMandates struct {
	byTenant map[string]*compliancepb.Mandate
	err      error
}

func (m staticMandates) Mandate(_ context.Context, tenantID, _ string, _ time.Time) (*compliancepb.Mandate, bool, error) {
	if m.err != nil {
		return nil, false, m.err
	}
	got, ok := m.byTenant[tenantID]
	return got, ok, nil
}

// concentrationMandate caps any single instrument at cap of gross.
func concentrationMandate(tenantID, portfolioID string, capPct int64) *compliancepb.Mandate {
	return &compliancepb.Mandate{
		MandateId:   "M-" + portfolioID,
		TenantId:    tenantID,
		PortfolioId: portfolioID,
		Version:     1,
		Rules: []*compliancepb.Rule{{
			RuleId:      "single-name-cap",
			Type:        compliancepb.RuleType_RULE_TYPE_CONCENTRATION,
			OnViolation: compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH,
			// DIMENSION_INSTRUMENT needs no classifier, which keeps these tests
			// about the mandate reaching the service rather than about reference
			// data. A SECTOR cap with no classifier is refused as unresolvable
			// (#640) — correct, and a different property than the one under test.
			Params: &compliancepb.Rule_Concentration{Concentration: &compliancepb.ConcentrationLimit{
				Dimension: compliancepb.Dimension_DIMENSION_INSTRUMENT,
				MaxWeight: &commonpb.Decimal{Coefficient: capPct, Exponent: -2},
			}},
		}},
		EffectiveAt: timestamppb.New(time.Unix(0, 0).UTC()),
	}
}

func gatedServer(t *testing.T, src compliance.MandateSource) *Server {
	t.Helper()
	rd := &Readiness{}
	rd.Set(true)
	return New(rd, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithMandateGate(MandateGate{Mandates: src, Engine: compliance.NewEngine(nil)}))
}

// proposeBody asks for a min-variance split across two uncorrelated equal-vol
// assets, which lands near 50/50 — inside a 60% single-name cap and outside a
// 40% one. The same body therefore drives both verdicts, so a difference in the
// answer can only come from the mandate.
func proposeBody(currency string) string {
	return `{"portfolio_id":"PF","instruments":["A","B"],
		"covariance":[[0.04,0],[0,0.04]],
		"objective":{"Type":1},
		"current_weights":{"A":1.0},"nav":100000,
		"prices":{"A":10,"B":10},"currency":"` + currency + `"}`
}

func decodeProposal(t *testing.T, body []byte) optimization.RebalanceProposal {
	t.Helper()
	var p optimization.RebalanceProposal
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("decode proposal: %v — body %s", err, body)
	}
	return p
}

// A book the mandate permits comes back FEASIBLE. This is the arm that makes
// /v1/orders reachable at all, and it is the one that could not exist before.
func TestPropose_AMandateThePortfolioSatisfiesReturnsFeasible(t *testing.T) {
	s := gatedServer(t, staticMandates{byTenant: map[string]*compliancepb.Mandate{
		"acme": concentrationMandate("acme", "PF", 60),
	}})

	rec := asPrincipal(t, s, http.MethodPost, "/v1/propose", proposeBody("USD"), "user:pm")
	if rec.Code != http.StatusOK {
		t.Fatalf("propose: got %d body %s", rec.Code, rec.Body.String())
	}
	p := decodeProposal(t, rec.Body.Bytes())
	if p.MandateStatus != optimization.MandateFeasible {
		t.Fatalf("MandateStatus = %s, want FEASIBLE — a ~50/50 split is inside a 60%% single-name "+
			"cap, so the check ran and passed. Anything else means the verdict was not earned: "+
			"violations=%v", p.MandateStatus, p.Violations)
	}
	if len(p.Violations) != 0 {
		t.Errorf("a feasible proposal carries violations %v", p.Violations)
	}
}

// THE ARM THAT PROVES THE FIRST ONE MEANS ANYTHING. Without it, a gate that
// stamped FEASIBLE unconditionally — which is exactly the pre-#646 defect,
// Rebalance hardcoding MandateFeasible=true — would satisfy the test above.
func TestPropose_AMandateThePortfolioBreachesReturnsInfeasible(t *testing.T) {
	s := gatedServer(t, staticMandates{byTenant: map[string]*compliancepb.Mandate{
		"acme": concentrationMandate("acme", "PF", 40),
	}})

	rec := asPrincipal(t, s, http.MethodPost, "/v1/propose", proposeBody("USD"), "user:pm")
	if rec.Code != http.StatusOK {
		t.Fatalf("propose: got %d body %s", rec.Code, rec.Body.String())
	}
	p := decodeProposal(t, rec.Body.Bytes())
	if p.MandateStatus != optimization.MandateInfeasible {
		t.Fatalf("MandateStatus = %s, want INFEASIBLE — a ~50/50 split breaches a 40%% single-name cap",
			p.MandateStatus)
	}
	if len(p.Violations) == 0 {
		t.Error("an infeasible proposal names no violation — the caller is told it failed and not why")
	}
}

// THE TENANT COMES FROM THE PRINCIPAL, NOT THE BODY. A mandate held for another
// tenant must not govern this caller's portfolio: that is #243, where tenant B's
// "growth" order was evaluated against tenant A's "growth" limits.
func TestPropose_AnotherTenantsMandateDoesNotGovern(t *testing.T) {
	s := gatedServer(t, staticMandates{byTenant: map[string]*compliancepb.Mandate{
		"globex": concentrationMandate("globex", "PF", 40), // would breach, if it applied
	}})

	rec := asPrincipal(t, s, http.MethodPost, "/v1/propose", proposeBody("USD"), "user:pm")
	if rec.Code != http.StatusOK {
		t.Fatalf("propose: got %d body %s", rec.Code, rec.Body.String())
	}
	p := decodeProposal(t, rec.Body.Bytes())
	if p.MandateStatus == optimization.MandateInfeasible {
		t.Fatal("a mandate belonging to tenant globex governed a caller in tenant acme")
	}
	if p.MandateStatus != optimization.MandateUnchecked {
		t.Fatalf("MandateStatus = %s, want UNCHECKED — no mandate governs this tenant's portfolio, "+
			"and 'nobody declared constraints' is not the same claim as 'the constraints passed'",
			p.MandateStatus)
	}
}

// A SOURCE THAT CANNOT ANSWER REFUSES, rather than returning a proposal whose
// UNCHECKED verdict would read as "nobody set constraints" when the truth is
// "the constraints could not be read". Those spell the same thing to a reader
// and mean different things to an operator.
func TestPropose_AnUnreadableMandateSourceRefuses(t *testing.T) {
	s := gatedServer(t, staticMandates{err: errors.New("registry unavailable")})

	rec := asPrincipal(t, s, http.MethodPost, "/v1/propose", proposeBody("USD"), "user:pm")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503 — an unreadable mandate source must not produce a proposal: %s",
			rec.Code, rec.Body.String())
	}
}

// BOTH TERMINAL SENTINELS refuse once rather than inviting a retry: nobody can
// say which rules apply, or the rules that do apply cannot be parsed, and asking
// again changes neither. A terminal error answered as transient is an infinite
// redelivery on the bus (#619) and a client retrying forever here.
//
// TABLE-DRIVEN OVER THE SENTINEL SET, because the first version of this handler
// named only ErrMandateTenantUnresolved and treated ErrMandateUnreadable as
// retryable — caught by TestEveryMandateLookupHandlesEveryTerminalSentinel, not
// by review.
func TestPropose_EveryTerminalMandateSentinelRefusesOnce(t *testing.T) {
	for name, sentinel := range map[string]error{
		"tenant unresolved":  compliance.ErrMandateTenantUnresolved,
		"mandate unreadable": compliance.ErrMandateUnreadable,
	} {
		t.Run(name, func(t *testing.T) {
			s := gatedServer(t, staticMandates{err: sentinel})
			rec := asPrincipal(t, s, http.MethodPost, "/v1/propose", proposeBody("USD"), "user:pm")
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("got %d, want 422 — %s is terminal, and a 503 invites a retry against "+
					"data that cannot change: %s", rec.Code, name, rec.Body.String())
			}
		})
	}
}

// The candidate book has to be valued in something. A mandate that governs the
// portfolio with no currency stated is refused rather than evaluated against an
// unstated unit.
func TestPropose_AGoverningMandateNeedsTheCurrency(t *testing.T) {
	s := gatedServer(t, staticMandates{byTenant: map[string]*compliancepb.Mandate{
		"acme": concentrationMandate("acme", "PF", 60),
	}})

	rec := asPrincipal(t, s, http.MethodPost, "/v1/propose", proposeBody(""), "user:pm")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 for a governed portfolio with no currency: %s", rec.Code, rec.Body.String())
	}
}

// AN UNWIRED SERVICE IS UNCHANGED. The pre-#751 posture stays reachable and
// honest: no mandate source, so no verdict, so /v1/orders still declines. A
// deployment that has not wired a mandate stream is no less safe than before.
func TestPropose_WithNoMandateSourceStaysUnchecked(t *testing.T) {
	rec := asPrincipal(t, newTestServer(), http.MethodPost, "/v1/propose", proposeBody("USD"), "user:pm")
	if rec.Code != http.StatusOK {
		t.Fatalf("propose: got %d body %s", rec.Code, rec.Body.String())
	}
	p := decodeProposal(t, rec.Body.Bytes())
	if p.MandateStatus != optimization.MandateUnchecked {
		t.Fatalf("MandateStatus = %s, want UNCHECKED with no mandate source wired", p.MandateStatus)
	}
}

// The propose route now needs an identity, because a mandate is resolved per
// TENANT and the tenant comes from the principal the gateway injects.
func TestPropose_WithoutAPrincipalIsRefused(t *testing.T) {
	rec := do(t, newTestServer(), http.MethodPost, "/v1/propose", proposeBody("USD"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401 — whose mandate governs a portfolio cannot be answered without a principal",
			rec.Code)
	}
}
