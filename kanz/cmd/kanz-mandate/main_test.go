package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eighred/kanz/internal/dualcontrol"
)

// THE TWO-INVOCATION MANDATE PATH (#410).
//
// Every case here runs against a temp directory and NO BROKER. That is not a
// limitation of the test — propose must not open a connection at all, and every
// approve case below is one that must refuse before reaching a dial. The one
// valid approval is asserted with --dry-run, which stops at the same point.

const mandateJSON = `{
  "mandate_id": "M-1",
  "tenant_id": "acme",
  "portfolio_id": "PF1",
  "version": 3,
  "effective_at": "2026-08-01T00:00:00Z"
}`

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// propose runs the propose step and returns the proposal path.
func propose(t *testing.T, dir, mandatePath, by string) string {
	t.Helper()
	out := filepath.Join(dir, "proposal.json")
	var buf bytes.Buffer
	err := run([]string{"propose", "--tenant", "acme", "--file", mandatePath,
		"--by", by, "--reason", "Q3 mandate", "--out", out}, &buf)
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	return out
}

// THE OLD SINGLE-SHOT FORM IS GONE, AND SAYS SO.
//
// It published a mandate on one person's say-so. Leaving it working beside the
// guarded path would leave the shorter unguarded route in place, and that is the
// one that gets used.
func TestRun_TheOldSingleCommandFormIsRefused(t *testing.T) {
	dir := t.TempDir()
	m := writeFile(t, dir, "mandate.json", mandateJSON)

	var buf bytes.Buffer
	err := run([]string{"--tenant", "acme", "--file", m, "--by", "operator:alice",
		"--reason", "Q3"}, &buf)
	if err == nil {
		t.Fatal("the old one-person form published a mandate")
	}
	if !strings.Contains(err.Error(), "two people") {
		t.Errorf("error does not explain the replacement:\n%v", err)
	}
}

func TestValidate_RecordsNoDecisionAndPublishesNothing(t *testing.T) {
	dir := t.TempDir()
	m := writeFile(t, dir, "mandate.json", mandateJSON)

	var buf bytes.Buffer
	if err := run([]string{"validate", "--tenant", "acme", "--file", m}, &buf); err != nil {
		t.Fatalf("validate: %v", err)
	}
	for _, want := range []string{"VALID", "mandate M-1 v3", "NOTHING WAS PROPOSED, APPROVED, OR PUBLISHED"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("output does not mention %q:\n%s", want, buf.String())
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "mandate.json" {
		t.Fatalf("validate created a decision artifact: %v", entries)
	}
}

// PROPOSE PUBLISHES NOTHING, AND SAYS SO.
func TestPropose_PublishesNothing(t *testing.T) {
	dir := t.TempDir()
	m := writeFile(t, dir, "mandate.json", mandateJSON)

	var buf bytes.Buffer
	out := filepath.Join(dir, "proposal.json")
	if err := run([]string{"propose", "--tenant", "acme", "--file", m,
		"--by", "operator:alice", "--reason", "Q3 mandate", "--out", out}, &buf); err != nil {
		t.Fatalf("propose: %v", err)
	}
	if !strings.Contains(buf.String(), "NOTHING WAS PUBLISHED") {
		t.Errorf("propose did not say it published nothing:\n%s", buf.String())
	}

	var pf proposalFile
	blob, err := os.ReadFile(out) //#nosec G304 -- test temp dir
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(blob, &pf); err != nil {
		t.Fatalf("the proposal is not readable JSON: %v", err)
	}
	if pf.Proposal.Act != dualcontrol.ActMandateChange {
		t.Errorf("act = %q, want MANDATE_CHANGE", pf.Proposal.Act)
	}
	if pf.Proposal.Proposer != "operator:alice" {
		t.Errorf("proposer = %q", pf.Proposal.Proposer)
	}
	if pf.Proposal.Digest == "" {
		t.Error("the proposal carries no digest — an approval would cover nothing")
	}
	if pf.Proposal.ExpiresAt.IsZero() {
		t.Error("the proposal never expires — an approval collected today could be applied " +
			"against next quarter's book")
	}
}

// THE PROPOSER CANNOT APPROVE THEIR OWN MANDATE.
func TestApprove_RefusesSelfApproval(t *testing.T) {
	dir := t.TempDir()
	m := writeFile(t, dir, "mandate.json", mandateJSON)
	p := propose(t, dir, m, "operator:alice")

	var buf bytes.Buffer
	err := run([]string{"approve", "--tenant", "acme", "--file", m, "--proposal", p,
		"--by", "operator:alice", "--dry-run"}, &buf)
	if !errors.Is(err, dualcontrol.ErrSelfApproval) {
		t.Fatalf("error = %v, want ErrSelfApproval", err)
	}
}

// AND NOT WITH A DIFFERENT CASE EITHER.
//
// "Alice@kanz" approving "alice@kanz" is one person. A case-sensitive comparison
// would call it two and produce a four-eyes trail with one pair of eyes.
func TestApprove_RefusesSelfApprovalAcrossCase(t *testing.T) {
	dir := t.TempDir()
	m := writeFile(t, dir, "mandate.json", mandateJSON)
	p := propose(t, dir, m, "operator:alice")

	var buf bytes.Buffer
	err := run([]string{"approve", "--tenant", "acme", "--file", m, "--proposal", p,
		"--by", "  Operator:ALICE ", "--dry-run"}, &buf)
	if !errors.Is(err, dualcontrol.ErrSelfApproval) {
		t.Fatalf("error = %v, want ErrSelfApproval", err)
	}
}

// THE MANDATE CANNOT BE SWAPPED BETWEEN THE TWO STEPS.
//
// This is the attack the digest exists for, and the reason approve takes --file
// at all rather than trusting the proposal: propose something defensible, collect
// the second name, publish something else.
func TestApprove_RefusesASwappedMandate(t *testing.T) {
	dir := t.TempDir()
	approvedPath := writeFile(t, dir, "mandate.json", mandateJSON)
	p := propose(t, dir, approvedPath, "operator:alice")

	swapped := writeFile(t, dir, "swapped.json",
		strings.Replace(mandateJSON, `"version": 3`, `"version": 9`, 1))

	var buf bytes.Buffer
	err := run([]string{"approve", "--tenant", "acme", "--file", swapped, "--proposal", p,
		"--by", "operator:bob", "--dry-run"}, &buf)
	if !errors.Is(err, dualcontrol.ErrPayloadChanged) {
		t.Fatalf("error = %v, want ErrPayloadChanged — a different mandate was approved "+
			"under a signature collected for another", err)
	}
}

// A GENUINE SECOND APPROVER IS ACCEPTED, AND BOTH NAMES ARE REPORTED.
//
// Without this every refusal above is satisfied by a tool that refuses
// everything — which would look like a working control and publish no mandates,
// re-creating the empty-registry state this binary exists to end.
func TestApprove_ASecondPersonIsAccepted(t *testing.T) {
	dir := t.TempDir()
	m := writeFile(t, dir, "mandate.json", mandateJSON)
	p := propose(t, dir, m, "operator:alice")

	var buf bytes.Buffer
	if err := run([]string{"approve", "--tenant", "acme", "--file", m, "--proposal", p,
		"--by", "operator:bob", "--dry-run"}, &buf); err != nil {
		t.Fatalf("a valid two-person approval was refused: %v", err)
	}
	for _, want := range []string{"operator:alice", "operator:bob", "nothing published"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("output does not mention %q:\n%s", want, buf.String())
		}
	}
}

// A PROPOSAL FOR ANOTHER ACT IS REFUSED BY NAME.
//
// Approval.Covers refuses it again at publish time; this refuses it where the
// operator can still read why.
func TestApprove_RefusesAProposalForAnotherAct(t *testing.T) {
	dir := t.TempDir()
	m := writeFile(t, dir, "mandate.json", mandateJSON)
	p := propose(t, dir, m, "operator:alice")

	blob, err := os.ReadFile(p) //#nosec G304 -- test temp dir
	if err != nil {
		t.Fatal(err)
	}
	tampered := writeFile(t, dir, "other-act.json",
		strings.Replace(string(blob), `"MANDATE_CHANGE"`, `"PRICING_OVERRIDE"`, 1))

	var buf bytes.Buffer
	err = run([]string{"approve", "--tenant", "acme", "--file", m, "--proposal", tampered,
		"--by", "operator:bob", "--dry-run"}, &buf)
	if err == nil || !strings.Contains(err.Error(), "not a mandate change") {
		t.Fatalf("error = %v, want a refusal naming the wrong act", err)
	}
}

// A MANDATE WITHOUT effective_at IS REFUSED RATHER THAN STAMPED.
//
// It used to default to time.Now(). With the mandate inside the approval digest,
// defaulting would make the two invocations hash different payloads and EVERY
// approval would fail as a payload change — a control failing for a reason that
// has nothing to do with the control, which is the worst kind of failure to
// debug.
func TestPropose_RefusesAMandateWithNoEffectiveAt(t *testing.T) {
	dir := t.TempDir()
	m := writeFile(t, dir, "mandate.json",
		strings.Replace(mandateJSON, ",\n  \"effective_at\": \"2026-08-01T00:00:00Z\"", "", 1))

	var buf bytes.Buffer
	err := run([]string{"propose", "--tenant", "acme", "--file", m,
		"--by", "operator:alice", "--reason", "Q3"}, &buf)
	if err == nil || !strings.Contains(err.Error(), "effective_at") {
		t.Fatalf("error = %v, want a refusal naming effective_at", err)
	}
}

// APPROVE WITHOUT A PROPOSAL IS REFUSED.
//
// There must be no route to publish that skips the first step.
func TestApprove_RequiresAProposal(t *testing.T) {
	dir := t.TempDir()
	m := writeFile(t, dir, "mandate.json", mandateJSON)

	var buf bytes.Buffer
	err := run([]string{"approve", "--tenant", "acme", "--file", m, "--by", "operator:bob"}, &buf)
	if err == nil || !strings.Contains(err.Error(), "--proposal is required") {
		t.Fatalf("error = %v, want --proposal required", err)
	}
}
