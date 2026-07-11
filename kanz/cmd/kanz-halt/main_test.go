package main

import (
	"strings"
	"testing"

	lifecyclepb "github.com/kanz-eng/kanz-schemas-go/lifecycle/v1"

	"github.com/kanz-eng/kanz/internal/platform/mode"
	"github.com/kanz-eng/kanz/internal/signal/translate"
)

func baseArgs(extra ...string) []string {
	return append([]string{"--by", "operator:akif", "--reason", "test", "--tenant", "eighred"}, extra...)
}

// Every field lifecycle.v1 requires for a transition must be forced at the flag
// boundary. An unattributable or unexplained halt is not an audit record.
func TestParseFlags_RequiresAttributionAndReason(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no principal", []string{"--reason", "x", "--tenant", "t"}, "--by is required"},
		{"no reason", []string{"--by", "operator:akif", "--tenant", "t"}, "--reason is required"},
		{"no tenant", []string{"--by", "operator:akif", "--reason", "x"}, "--tenant is required"},
		{"principal is not {type}:{id}", []string{"--by", "akif", "--reason", "x", "--tenant", "t"}, "is not a principal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseFlags(tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("parseFlags = %v, want error containing %q", err, tc.want)
			}
		})
	}
}

// The sharpest trap in the whole tool: translate.Gate deliberately IGNORES a
// NORMAL transition issued by a "system:" principal, so automated recovery cannot
// clear a safety trip. A resume sent under system: would publish cleanly and
// change nothing — a silent no-op on the one command an operator runs during an
// incident. It must fail at the flag boundary instead.
func TestParseFlags_RejectsSystemPrincipalResume(t *testing.T) {
	_, err := parseFlags([]string{"--resume", "--by", "system:health-detector", "--reason", "looks fine", "--tenant", "t"})
	if err == nil || !strings.Contains(err.Error(), "cannot resume") {
		t.Fatalf("parseFlags = %v, want a rejection of a system: resume", err)
	}

	// The same principal may still HALT — automation is allowed to pull the brake,
	// only not to release it.
	if _, err := parseFlags([]string{"--by", "system:risk-monitor", "--reason", "breach", "--tenant", "t"}); err != nil {
		t.Fatalf("a system: principal must still be able to HALT: %v", err)
	}
}

func TestBuildFact_HaltAndResume(t *testing.T) {
	opt, err := parseFlags(baseArgs())
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	mc, err := buildFact(opt)
	if err != nil {
		t.Fatalf("buildFact: %v", err)
	}
	if mc.GetNewMode() != lifecyclepb.OperatingMode_OPERATING_MODE_HALTED {
		t.Fatalf("default new_mode = %s, want HALTED — the tool's default action must be to stop", mc.GetNewMode())
	}
	if mc.GetComponent() != mode.ComponentSystem {
		t.Fatalf("component = %q, want %q — the gate only reacts to a whole-system transition", mc.GetComponent(), mode.ComponentSystem)
	}
	if mc.GetChangedBy() != "operator:akif" || mc.GetReason() != "test" {
		t.Fatalf("attribution lost: by=%q reason=%q", mc.GetChangedBy(), mc.GetReason())
	}

	opt, err = parseFlags(baseArgs("--resume"))
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	mc, err = buildFact(opt)
	if err != nil {
		t.Fatalf("buildFact: %v", err)
	}
	if mc.GetNewMode() != lifecyclepb.OperatingMode_OPERATING_MODE_NORMAL {
		t.Fatalf("--resume new_mode = %s, want NORMAL", mc.GetNewMode())
	}
}

// The tool cannot observe prior state, so it must not invent it: an unset
// --previous stays UNSPECIFIED rather than being guessed at.
func TestBuildFact_DoesNotFabricatePreviousMode(t *testing.T) {
	opt, _ := parseFlags(baseArgs())
	mc, err := buildFact(opt)
	if err != nil {
		t.Fatalf("buildFact: %v", err)
	}
	if mc.GetPreviousMode() != lifecyclepb.OperatingMode_OPERATING_MODE_UNSPECIFIED {
		t.Fatalf("previous_mode = %s, want UNSPECIFIED — the tool guessed at state it cannot observe", mc.GetPreviousMode())
	}

	opt, _ = parseFlags(baseArgs("--previous", "normal"))
	mc, err = buildFact(opt)
	if err != nil {
		t.Fatalf("buildFact: %v", err)
	}
	if mc.GetPreviousMode() != lifecyclepb.OperatingMode_OPERATING_MODE_NORMAL {
		t.Fatalf("previous_mode = %s, want NORMAL", mc.GetPreviousMode())
	}
}

// A ModeChanged is always a real transition (lifecycle.v1).
func TestBuildFact_RejectsNonTransition(t *testing.T) {
	opt, _ := parseFlags(baseArgs("--previous", "halted"))
	if _, err := buildFact(opt); err == nil {
		t.Fatal("halting from HALTED was accepted; a ModeChanged must be a real transition")
	}
}

// The whole point of the tool: what it emits must actually drive the gate the
// edge processes hold. This wires the real publisher payload into the real
// consumer and asserts the brake moves — if the subject, component, or principal
// conventions ever drift apart, this fails.
func TestFactDrivesTheRealGate(t *testing.T) {
	gate := translate.OpenGate(nil)

	opt, _ := parseFlags(baseArgs())
	halt, err := buildFact(opt)
	if err != nil {
		t.Fatalf("buildFact: %v", err)
	}
	gate.Observe(halt)
	if !gate.Halted() {
		t.Fatal("the FACT this tool publishes did not halt the gate the edge processes check")
	}

	opt, _ = parseFlags(baseArgs("--resume"))
	resume, err := buildFact(opt)
	if err != nil {
		t.Fatalf("buildFact: %v", err)
	}
	gate.Observe(resume)
	if gate.Halted() {
		t.Fatal("the resume FACT this tool publishes did not reopen the gate — the halt would be unrecoverable")
	}
}

// The publisher and the consumer must agree on the subject, or the kill-switch
// publishes into the void.
func TestSubjectMatchesTheGatesSubscription(t *testing.T) {
	if mode.Subject != translate.SubjectModeChanged {
		t.Fatalf("publisher subject %q != gate subscription %q", mode.Subject, translate.SubjectModeChanged)
	}
}
