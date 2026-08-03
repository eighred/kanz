package main

import (
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/pkg/bus"
)

// The flag layer is the only thing between an operator's typo and a republish
// onto a live capital-path subject, and it is the layer no unit test of pkg/bus
// covers. main.go wiring has shipped crashes twice in this repo with a fully
// green suite.
func TestParseFlagsRejectsASubjectOutsideTheDLQNamespace(t *testing.T) {
	_, err := parseFlags([]string{"--subject", "order.order.submit"})
	if err == nil {
		t.Fatal("a LIVE subject was accepted. The drain would then read live order traffic and " +
			"republish it onto itself — an infinite loop of real order submissions, which is the " +
			"single worst outcome this tool can produce")
	}
	// The most likely mistake is naming the live subject, so the error should
	// hand back the fixed version rather than just refusing.
	if !strings.Contains(err.Error(), "dlq.order.order.submit") {
		t.Errorf("error %q does not suggest the corrected subject", err)
	}
}

func TestParseFlagsRequiresASubject(t *testing.T) {
	if _, err := parseFlags(nil); err == nil {
		t.Fatal("no --subject was accepted; there is no safe default DLQ subject to drain")
	}
}

func TestParseFlagsRejectsAnEmptyGroup(t *testing.T) {
	_, err := parseFlags([]string{"--subject", "dlq.order.order.submit", "--group", ""})
	if err == nil {
		t.Fatal("an empty --group was accepted. Without a durable cursor the next run re-reads " +
			"the whole DLQ retention window and re-submits every order it already recovered")
	}
}

func TestParseFlagsRejectsANegativeLimit(t *testing.T) {
	if _, err := parseFlags([]string{"--subject", "dlq.x.y.z", "--limit", "-1"}); err == nil {
		t.Fatal("a negative --limit was accepted")
	}
}

// The defaults are the safety posture for an operator who passes only
// --subject, which is how this will actually be run during an incident.
func TestDefaultsAreTheSafeOnes(t *testing.T) {
	opt, err := parseFlags([]string{"--subject", "dlq.order.order.submit"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if opt.maxRedrives != bus.DefaultMaxRedrives {
		t.Errorf("max-redrives = %d, want the bounded default %d — an unbounded drain can cycle "+
			"a permanently-failing order forever", opt.maxRedrives, bus.DefaultMaxRedrives)
	}
	if opt.minAge != bus.DefaultMinAge {
		t.Errorf("min-age = %s, want %s — redriving inside the dedup windows silently does "+
			"nothing while reporting success", opt.minAge, bus.DefaultMinAge)
	}
	if opt.includeTerminal {
		t.Error("--include-terminal defaults ON: unprocessable messages would be redriven " +
			"automatically, fail identically, and burn the loop budget")
	}
	if opt.group == "" {
		t.Error("--group defaults empty; the durable cursor prevents double-submission")
	}
	if opt.timeout <= 0 || opt.idleTimeout <= 0 {
		t.Errorf("non-positive timeouts (%s / %s) would end the run before it read anything",
			opt.timeout, opt.idleTimeout)
	}
}

func TestParseFlagsAcceptsAWellFormedDrain(t *testing.T) {
	opt, err := parseFlags([]string{
		"--subject", "dlq.order.order.submit",
		"--limit", "1",
		"--min-age", "0",
		"--include-terminal",
	})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if opt.subject != "dlq.order.order.submit" || opt.limit != 1 ||
		opt.minAge != time.Duration(0) || !opt.includeTerminal {
		t.Errorf("flags did not round-trip: %+v", opt)
	}
}
