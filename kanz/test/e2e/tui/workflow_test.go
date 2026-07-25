package tui

import (
	"strings"
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

// ctrl+t is byte 0x14. The Add Node form's Test Connection probe is a TCP dial, so a
// reachable SSH port answers and a closed port does not.
const ctrlT = byte(0x14)

// closedPort is not :22, the only port the environment's security group and
// node-provisioner-egress NetworkPolicy are documented to permit toward node 2. A
// probe against it therefore either gets a fast RST (rendered "✗ unreachable: ...")
// or the packet is dropped and the gateway's own dial times out (rendered
// "✗ test failed: ..."). Both render with a leading "✗", so asserting on that glyph
// proves the probe actually distinguishes reachable from not, regardless of which of
// those two failure shapes this particular network produces.
const closedPort = "23"

func TestAddNodeProbesThenJoinsTheNodeLive(t *testing.T) {
	env := requireEnv(t)
	// Guard by COUNTING nodes, not by guessing a name. k3s names a node after the
	// target host's own hostname, which on this provider derives from its PRIVATE ip
	// (node 2 is ip-172-26-12-47) — never from the public ip the operator types into
	// the form. A guard built on "ip-"+publicIP could never match, so it would never
	// skip, and this proof would silently re-run against an already-joined node.
	if len(clusterNodeNames(t)) > 1 {
		t.Skip("a second node is already joined; remove it from the cluster to re-prove the join")
	}

	s := startTUI(t, env)
	defer s.Close()

	s.Send("a")
	s.WaitFor(t, "Hostname", 5*time.Second)

	// Fields in order: Hostname, IP, SSH Port (prefilled 22), User, Key Path.
	s.Send("e2e-node2\t")
	s.Send(env.Node2IP + "\t") // focus now sits on SSH Port, prefilled "22"

	// S2b, failure path: prove the probe can actually say no, not just that it
	// always renders "reachable" for whatever ip:port happens to be in the form.
	// This must run BEFORE the real probe below and the port must be restored
	// afterward — testConnCmd reads whatever is currently in the SSH Port field,
	// and the field that gets probed here is also the one Save will submit.
	s.SendKey(0x7f) // backspace: "22" -> "2"
	s.SendKey(0x7f) // backspace: "2" -> ""; SSH Port is now empty
	s.Send(closedPort)
	s.SendKey(ctrlT)
	s.WaitFor(t, "✗", 20*time.Second) // "✗" — see closedPort's comment for why

	// Restore the real port. Leaving it at closedPort would make the save below
	// provision against a port nothing is listening on.
	s.SendKey(0x7f)
	s.SendKey(0x7f)
	s.Send("22\t") // put the real port back, then move on to User
	s.Send("ubuntu\t")
	s.Send(env.SSHKeyPath)

	// S2b happy path: probe before committing.
	s.SendKey(ctrlT)
	s.WaitFor(t, "reachable", 20*time.Second)

	s.Send("\r") // save
	// The join installs k3s over SSH on a 414MB host; allow real time for it.
	s.WaitFor(t, "Installing", 30*time.Second)

	waitForNodeReady(t, 6*time.Minute)
}

// clusterNodeNames lists every node in the cluster. Used instead of predicting a
// node's name: the name comes from the remote host's hostname, which this test has
// no reliable way to derive from the address an operator typed.
func clusterNodeNames(t *testing.T) []string {
	t.Helper()
	return strings.Fields(kubectl(t, "get", "nodes",
		"-o", "jsonpath={range .items[*]}{.metadata.name} {end}"))
}
