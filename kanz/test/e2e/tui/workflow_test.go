package tui

import (
	"testing"
	"time"
)

// startTUI launches the real binary against the real gateway and waits until it has
// rendered its first estate poll.
//
// Pane titles render UPPERCASE ("NODES", "CLUSTERS", "API MANAGER" — see
// cmd/universe/view.go's styleTitle.Render calls) and WaitFor's match is
// case-sensitive. Verified against a captured frame from the live cluster, not
// inferred from the plan doc — do not "correct" this back to title case.
func startTUI(t *testing.T, env Env) *Session {
	t.Helper()
	s := Start(t, env.Binary, nil, env.sessionEnv())
	s.WaitFor(t, "NODES", 20*time.Second)
	return s
}

func TestNavigationReachesEveryPaneAndQuitsCleanly(t *testing.T) {
	env := requireEnv(t)
	s := startTUI(t, env)
	defer s.Close()

	// tab cycles Nodes -> Clusters -> API Manager -> Nodes. Assert on the pane's own
	// domain text, never on layout. "API MANAGER" rather than the shorter "API": the
	// venue-key form also renders "API Key"/"API Secret", so the short form would
	// pass even if the API Manager pane never actually became active.
	s.Send("\t")
	s.WaitFor(t, "CLUSTERS", 5*time.Second)
	s.Send("\t")
	s.WaitFor(t, "API MANAGER", 5*time.Second)
	s.Send("\t")
	s.WaitFor(t, "NODES", 5*time.Second)
}

// closeGraceBound is how long a clean tea.Quit takes to unwind versus driver.go's
// 3-second forced-kill fallback in Session.Close. A graceful exit and a killed
// process are different outcomes for this proof, so the bound must sit strictly
// below the kill timeout with headroom for scheduler jitter — 2s leaves a full
// second of margin under Close's 3s Kill().
const closeGraceBound = 2 * time.Second

// FINDING (recorded here, not fixed here — that is a later UX slice's call):
// inside the Add Node form, q is not a cancel key. cmd/universe/model.go's
// updateForm has no case for the rune "q"; unhandled runes fall to
// tea.KeyRunes, which updateForm routes to m.form.key(msg) — i.e. it is typed
// into whichever field has focus (Hostname, first in tab order). An operator
// who reflexively presses q to back out is silently editing the Hostname
// field instead of leaving the form. Esc is the only key that exits the form
// (updateForm's tea.KeyEsc case, which clears m.showForm and m.formErr).
//
// TestQPressedInsideFormIsTypedNotCancelled below exists so that if a future
// change ever makes q a real cancel key, this suite fails until it is
// reflected — right now nothing pins q's current (mis)behaviour.

// TestEscCancelsFormAndReturnsToNodesPane proves the operator's actual escape
// hatch: Esc, not q, closes the Add Node form and returns control to the
// NODES pane the operator started from — not to some stuck intermediate
// state.
func TestEscCancelsFormAndReturnsToNodesPane(t *testing.T) {
	env := requireEnv(t)
	s := startTUI(t, env)
	defer s.Close()

	s.Send("a")
	s.WaitFor(t, "Hostname", 5*time.Second)
	s.SendKey(0x1b) // Esc
	s.WaitFor(t, "NODES", 5*time.Second)
}

// TestQPressedInsideFormIsTypedNotCancelled pins the finding above: q inside
// the form is a literal keystroke into the focused field, not a cancel. The
// form must still be showing (Hostname still rendered) after q is sent.
func TestQPressedInsideFormIsTypedNotCancelled(t *testing.T) {
	env := requireEnv(t)
	s := startTUI(t, env)
	defer s.Close()

	s.Send("a")
	s.WaitFor(t, "Hostname", 5*time.Second)
	s.Send("q")
	s.WaitFor(t, "Hostname", 5*time.Second)
}

// TestCancelledFormThenQuitProvisionsNothingAndExitsCleanly establishes the
// full abandon-the-form workflow with the program's real keys: Esc backs out
// of the form (proven above to actually work, unlike q), and only then does q
// reach the top-level handler where it is tea.Quit. Both halves matter:
//
//  1. The process must exit gracefully (Close well under the 3s kill
//     fallback), because if q only "worked" via a forced kill, the program
//     did not actually handle the operator's quit — the harness gave up on
//     its behalf, which is not the same thing.
//  2. Kubernetes — never the rendered screen — must show nothing provisioned,
//     since a form that renders as closed but has already fired its submit
//     command would still pass a screen-only assertion.
func TestCancelledFormThenQuitProvisionsNothingAndExitsCleanly(t *testing.T) {
	env := requireEnv(t)
	before := len(kubectlLines(t, "get", "jobs", "-n", "kanz-operator", "-o", "name"))

	s := startTUI(t, env)
	s.Send("a")
	s.WaitFor(t, "Hostname", 5*time.Second)
	s.SendKey(0x1b) // Esc: the only key that actually leaves the form
	s.WaitFor(t, "NODES", 5*time.Second)

	closeStart := time.Now()
	s.Close()
	if elapsed := time.Since(closeStart); elapsed >= closeGraceBound {
		t.Errorf("Close took %s, at or above the %s grace bound; q should have quit "+
			"the program cleanly rather than needing driver.go's forced-kill fallback",
			elapsed, closeGraceBound)
	}

	after := len(kubectlLines(t, "get", "jobs", "-n", "kanz-operator", "-o", "name"))
	if after != before {
		t.Errorf("cancelling the Add Node form and quitting changed the Job count (%d -> %d); "+
			"abandoning a form must provision nothing", before, after)
	}
}
