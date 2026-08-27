package tools

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/eighred/kanz/internal/agentgate"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/copilot/internal/governed"
	"github.com/eighred/kanz/services/copilot/internal/llm"
)

// A FAILED OWNERSHIP LOOKUP MUST NOT DISSOLVE THE TENANT BOUNDARY (#741).
//
// authorize resolved the portfolio's owning tenant and, on ANY error, set it to
// "". The authorizer read "" as "this resource is not tenant-scoped" and skipped
// the cross-tenant gate; portfolio scope and RBAC then passed on their own
// terms, and the tool read the data. So a deadline, an UNAVAILABLE or a 500 on
// one dependency turned the estate's strongest stated boundary off for that
// invocation — availability deciding isolation.
//
// It was reachable through POST /v1/ask, which the gateway mounts at authz.Read,
// the baseline capability every authenticated caller already holds.
//
// These tests assert the property at the level that matters: the governed READ
// is never issued. Asserting only on the refusal string would pass for a
// version that leaked the data and then apologised.

// transientErr is the class the old code swallowed: not a NotFound, just a
// dependency that did not answer.
var transientErr = errors.New("rpc error: code = DeadlineExceeded desc = context deadline exceeded")

// The headline case: an intruder from another tenant, while the ownership
// lookup is failing. Under the old code the cross-tenant gate was skipped and
// t1's measures came back.
func TestUnresolvedOwner_CrossTenantProbeReadsNothing(t *testing.T) {
	reg, rec, client := harnessWith(t, transientErr)
	intruder := &auth.Principal{Subject: "mallory", Tenant: "t2", Roles: []string{"analyst"}}

	out := reg.Invoke(context.Background(), intruder,
		llm.ToolCall{Name: "get_risk_measures", Input: map[string]any{"portfolio_id": "PF-T1"}})

	if !out.IsError {
		t.Fatalf("a cross-tenant probe was SERVED while the ownership lookup was failing: %s", out.Content)
	}
	if client.reads != 0 {
		t.Fatalf("the governed read surface was called %d time(s) behind a failed ownership lookup — "+
			"the refusal is cosmetic and the data left the tenant", client.reads)
	}
	if len(out.Values) != 0 || len(out.Citations) != 0 {
		t.Fatalf("refusal carried data: values=%v citations=%+v", out.Values, out.Citations)
	}
	// Fail LOUDLY: an outage of the read surface must not be dressed up as an
	// empty portfolio, or the operator sees a quiet estate instead of a broken
	// dependency.
	if out.Content != agentgate.OwnerUnavailable(auth.ResourcePortfolio) {
		t.Errorf("content = %q, want the unavailable wording %q", out.Content, agentgate.OwnerUnavailable(auth.ResourcePortfolio))
	}
	// The attempt is still audited. A probe that is refused before the
	// authorizer runs is a probe nobody can see afterwards.
	if len(rec.logs) == 0 {
		t.Fatal("the refused attempt was not recorded to the observation stream")
	}
	last := rec.logs[len(rec.logs)-1]
	if got := last.GetAttributes()["decision"]; got != "deny" {
		t.Errorf("recorded decision = %q, want deny", got)
	}
	if got := last.GetAttributes()["deny.code"]; got != string(auth.DenyResourceTenantUnresolved) {
		t.Errorf("recorded deny.code = %q, want %q", got, auth.DenyResourceTenantUnresolved)
	}
}

// The same refusal reaches the portfolio's OWN tenant. Fail-closed is not a
// property of who is asking — if the owner cannot be established, it cannot be
// established for anyone, and a rule that only bites strangers is a rule that
// was never load-bearing.
func TestUnresolvedOwner_OwningTenantIsRefusedToo(t *testing.T) {
	reg, _, client := harnessWith(t, transientErr)

	out := reg.Invoke(context.Background(), t1Analyst(),
		llm.ToolCall{Name: "get_exposure", Input: map[string]any{"portfolio_id": "PF-T1"}})

	if !out.IsError || out.Content != agentgate.OwnerUnavailable(auth.ResourcePortfolio) {
		t.Fatalf("the owning tenant was served on an unresolved owner: err=%v content=%q", out.IsError, out.Content)
	}
	if client.reads != 0 {
		t.Fatalf("governed read issued %d time(s) on an unresolved owner", client.reads)
	}
}

// Every governed tool runs the same gate. A tool added later that forgot to call
// authorize would show up here as a read that got through.
func TestUnresolvedOwner_EveryToolFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		tool  string
		input map[string]any
	}{
		{"get_risk_measures", map[string]any{"portfolio_id": "PF-T1"}},
		{"get_exposure", map[string]any{"portfolio_id": "PF-T1"}},
		{"evaluate_scenario", map[string]any{"portfolio_id": "PF-T1", "scenario": "rates_up_200bp"}},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			reg, _, client := harnessWith(t, transientErr)
			intruder := &auth.Principal{Subject: "mallory", Tenant: "t2", Roles: []string{"analyst"}}
			out := reg.Invoke(context.Background(), intruder, llm.ToolCall{Name: tc.tool, Input: tc.input})
			if !out.IsError || client.reads != 0 {
				t.Fatalf("%s served behind a failed ownership lookup: err=%v reads=%d content=%q",
					tc.tool, out.IsError, client.reads, out.Content)
			}
		})
	}

	// NON-VACUITY: the loop above must cover every tool the registry exposes, or
	// it proves the gate for the ones somebody remembered. A tool added without
	// a row here fails this count.
	reg, _ := harness(t)
	if n := len(reg.Defs()); n != 3 {
		t.Fatalf("the registry exposes %d tools but this test covers 3 — add the new tool to the "+
			"table above rather than raising this number", n)
	}
}

// THE REFUSALS MUST BE INDISTINGUISHABLE. "PF-T1 belongs to another tenant" and
// "PF-NOPE does not exist" have to read identically, or a caller enumerates
// another tenant's portfolios by trying ids and sorting the answers.
func TestRefusal_CrossTenantIsWordForWordTheNotFoundAnswer(t *testing.T) {
	reg, _, _ := harnessWith(t, nil)
	intruder := &auth.Principal{Subject: "mallory", Tenant: "t2", Roles: []string{"analyst"}}

	// A portfolio that exists, owned by t1.
	existing := reg.Invoke(context.Background(), intruder,
		llm.ToolCall{Name: "get_risk_measures", Input: map[string]any{"portfolio_id": "PF-T1"}})
	// A portfolio that exists nowhere. The stub returns ErrUnknownPortfolio.
	missing := reg.Invoke(context.Background(), intruder,
		llm.ToolCall{Name: "get_risk_measures", Input: map[string]any{"portfolio_id": "PF-NOPE"}})

	if !existing.IsError || !missing.IsError {
		t.Fatalf("both probes must be refused: existing=%+v missing=%+v", existing, missing)
	}
	// Same sentence, differing only in the id the caller itself supplied.
	if strings.TrimSuffix(existing.Content, "PF-T1") != strings.TrimSuffix(missing.Content, "PF-NOPE") {
		t.Fatalf("a cross-tenant portfolio answers %q and a nonexistent one answers %q — the "+
			"difference is a cross-tenant existence oracle", existing.Content, missing.Content)
	}
	if existing.Content != msgNoGovernedData+"PF-T1" {
		t.Errorf("content = %q, want %q", existing.Content, msgNoGovernedData+"PF-T1")
	}
}

// A same-tenant caller asking for a portfolio that is genuinely not there still
// gets the not-found answer, not an authorization error. The sanitizing must not
// have been bought by making every refusal say the same unhelpful thing.
func TestUnknownPortfolio_StillReadsAsMissingForItsOwnTenant(t *testing.T) {
	reg, _, _ := harnessWith(t, nil)
	out := reg.Invoke(context.Background(), t1Analyst(),
		llm.ToolCall{Name: "get_risk_measures", Input: map[string]any{"portfolio_id": "PF-NOPE"}})
	if !out.IsError || out.Content != msgNoGovernedData+"PF-NOPE" {
		t.Fatalf("content = %q, want %q", out.Content, msgNoGovernedData+"PF-NOPE")
	}
}

// A refusal that is about the CALLER's own token — no grant, out of scope — is
// stated plainly. Flattening those into "no such portfolio" would tell an
// analyst their own portfolio had vanished.
func TestGrantRefusals_AreStatedRatherThanFlattened(t *testing.T) {
	reg, _, _ := harnessWith(t, nil)
	cases := map[string]*auth.Principal{
		"no role":      {Subject: "carol", Tenant: "t1", Roles: []string{"nobody"}},
		"out of scope": {Subject: "bob", Tenant: "t1", Roles: []string{"analyst"}, Portfolios: []string{"PF-OTHER"}},
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			out := reg.Invoke(context.Background(), p,
				llm.ToolCall{Name: "get_risk_measures", Input: map[string]any{"portfolio_id": "PF-T1"}})
			if !out.IsError || out.Content != agentgate.NotAuthorized(auth.ResourcePortfolio) {
				t.Fatalf("content = %q, want %q", out.Content, agentgate.NotAuthorized(auth.ResourcePortfolio))
			}
		})
	}
}

// ErrUnknownPortfolio is the one error that is NOT an outage: it is the read
// surface answering, tenant-indistinguishably, that it has nothing. It must not
// be swept into the unavailable branch, or a legitimate miss reads as a broken
// dependency and an operator chases an outage that is not happening.
func TestUnknownPortfolio_IsNotReportedAsAnOutage(t *testing.T) {
	reg, _, _ := harnessWith(t, nil)
	out := reg.Invoke(context.Background(), t1Analyst(),
		llm.ToolCall{Name: "get_exposure", Input: map[string]any{"portfolio_id": "PF-NOPE"}})
	if out.Content == agentgate.OwnerUnavailable(auth.ResourcePortfolio) {
		t.Fatal("a missing portfolio was reported as an unresolved-owner outage")
	}
	// And the branch really is keyed on the sentinel, not on "err != nil": feed
	// the sentinel in as the injected error and the answer must still be the
	// not-found one.
	sentinel, _, _ := harnessWith(t, governed.ErrUnknownPortfolio)
	again := sentinel.Invoke(context.Background(), t1Analyst(),
		llm.ToolCall{Name: "get_exposure", Input: map[string]any{"portfolio_id": "PF-T1"}})
	if again.Content != msgNoGovernedData+"PF-T1" {
		t.Fatalf("content = %q, want %q — ErrUnknownPortfolio was classed as an outage", again.Content, msgNoGovernedData+"PF-T1")
	}
}

// THE TWO NOT-FOUND SENTENCES MUST STAY IDENTICAL. One is the gate's refusal
// for a resource this caller may not see; the other is this package's answer for
// a read that got PAST authorization and found nothing. A caller able to tell
// them apart could ask "was I refused, or is it empty?" — which is the existence
// oracle the gate exists to deny, rebuilt one layer up.
//
// They are two constants in two packages by necessity: the gate cannot know the
// copilot found no data, and the copilot must not re-run the gate's decision.
// This is what keeps them in step.
func TestTheRefusalAndTheEmptyAnswerAreTheSameSentence(t *testing.T) {
	const pf = "PF-T1"
	gate := agentgate.NotVisible(auth.ResourcePortfolio, pf)
	empty := msgNoGovernedData + pf
	if gate != empty {
		t.Fatalf("the gate refuses with %q and this package answers %q — the difference tells a "+
			"caller whether the resource exists", gate, empty)
	}
}
