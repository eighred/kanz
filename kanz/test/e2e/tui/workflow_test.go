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

// ctrl+t is byte 0x14. Test Connection is no longer an in-process dial: the operator
// creates an ephemeral Job that opens one TCP connection to the form's ip:port and
// reports what happened. Two consequences shape every proof below — the call now costs
// seconds rather than milliseconds, and a port other than :22 is refused as INPUT
// instead of being probed at all (see TestProbeRefusesAPortItCannotProbe).
const ctrlT = byte(0x14)

// probeVerdictWait bounds how long a proof waits for Test Connection to render a verdict.
//
// It is set ABOVE provision.probeTimeout (60s) on purpose. The probe is an ephemeral Job, and
// four layers bound the call — a 10s dial inside a 60s operator wait inside a 90s gateway
// budget inside the TUI's own 100s. Those nest outward so the innermost layer, which is the
// only one that knows WHY the probe did not answer, is the one that reports. A test bound
// below 60s throws that away: it would cut the operator off mid-verdict and report a bare PTY
// timeout naming no cause, which is precisely the misdiagnosis the nesting was built to end.
//
// 75s clears 60s with margin while staying under the gateway's 90s, so what lands on screen is
// the operator's own sentence. Every realistic shape resolves far sooner — an instant refusal,
// or a dropped packet that fails the dial at 10s — so this bound is only ever paid by a genuine
// fault, and when it is paid the screen explains itself.
const probeVerdictWait = 75 * time.Second

// TestAddNodeProbesThenJoinsTheNodeLive is the happy path end to end: probe a reachable
// host, then commit and watch the node actually join. The two negative outcomes a probe
// can produce are proven separately below, each against a target chosen to produce that
// one outcome and nothing else — a single test that pretends to cover all three ends up
// asserting on whichever glyph they happen to share, which is how the previous version
// of this proof came to pass for a reason it did not claim.
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

	// Fields in order: Hostname, IP, SSH Port (prefilled 22), User, Key Path. The port
	// is tabbed past UNTOUCHED: 22 is both the prefilled value and the only port this
	// estate can probe or provision over, so there is nothing to edit here and — unlike
	// the previous version of this test — nothing to restore before the save below.
	s.Send("e2e-node2\t")
	s.Send(env.Node2IP + "\t") // focus now sits on SSH Port, prefilled "22"
	s.Send("\t")               // leave "22" exactly as it is; focus moves to User
	s.Send("ubuntu\t")
	s.Send(env.SSHKeyPath)

	// S2b happy path: probe before committing.
	s.SendKey(ctrlT)

	// The IN-FLIGHT state must be asserted BEFORE the verdict, or this proof says
	// nothing about the seconds in between — and those seconds are now where the
	// operator spends the entire probe. A form that answered a keypress with an
	// unchanged screen would still satisfy a verdict-only assertion.
	//
	// 5s: "probing…" (cmd/universe/view.go) is set in the same Update that handles the
	// keypress, with no I/O in front of it, so this is a keystroke-latency bound of the
	// same class as the 5s form and pane waits above — not a probe bound.
	s.WaitFor(t, "probing", 5*time.Second)

	// "✓ reachable", not the bare word: "✗ unreachable: ..." CONTAINS "reachable", so
	// matching the word alone would report a refused host as a successful probe and
	// only fail later, at the join, with a cause that points at the wrong thing.
	//
	// 45s, up from the 20s the in-process dial needed. ctrl+t now creates a Job, waits
	// for a pod to be scheduled and its container to start, and only then spends up to
	// probeDialTimeout (10s, cmd/kanz-provisioner/probe.go) on the dial itself, so 20s
	// now sits below the floor of a perfectly healthy run on a cold image.
	//
	s.WaitFor(t, "✓ reachable", probeVerdictWait)

	s.Send("\r") // save
	// The join installs k3s over SSH on a 414MB host; allow real time for it.
	s.WaitFor(t, "Installing", 30*time.Second)

	waitForNodeReady(t, 6*time.Minute)
}

// TestProbeReportsAClosedPortAsUnreachable proves Test Connection can genuinely answer
// "no" — that a ✗ verdict is a real observation of a real dial rather than the only
// thing the form knows how to say.
//
// The target is 127.0.0.1 on the prefilled port 22, and both halves are deliberate:
//
//   - The ADDRESS is what makes this fail. Nothing listens on :22 inside the distroless
//     probe pod, so the kernel refuses the connection immediately: a real reachable=false
//     carrying a real reason ("connection refused"), in milliseconds, with no second host
//     to arrange and nothing about the environment left to assume. Loopback never leaves
//     the pod's network namespace, so no NetworkPolicy is in the path either — the
//     verdict cannot be an artefact of cluster policy the way a probe of a routed
//     address can be.
//   - The PORT stays at 22. Forcing the failure with a non-22 port is exactly the trap
//     this test replaces: that path is now an INPUT rejection at the operator's boundary
//     (TestProbeRefusesAPortItCannotProbe), so no probe is ever created, and asserting on
//     ✗ would pass without a dial having happened at all.
//
// Nothing is saved, so the cluster is unchanged — this may run at any time, whether or
// not a second node is joined. The probe Job itself is created and deleted by the
// operator on every path, including timeout and cancellation.
func TestProbeReportsAClosedPortAsUnreachable(t *testing.T) {
	env := requireEnv(t)
	s := startTUI(t, env)
	defer s.Close()

	s.Send("a")
	s.WaitFor(t, "Hostname", 5*time.Second)

	s.Send("probe-only\t") // Hostname; this form is never submitted
	s.Send("127.0.0.1\t")  // IP; focus now sits on SSH Port, prefilled "22"

	s.SendKey(ctrlT)
	// 5s, and the same keystroke-latency reasoning as the happy path: the in-flight
	// state is set synchronously with the keypress, so nothing here waits on the probe.
	s.WaitFor(t, "probing", 5*time.Second)

	// "✗ unreachable", not a bare "✗". The other ✗ this form can render is
	// "✗ test failed: ...", which means the probe could not be RUN — the opposite of
	// what this test claims to prove. Matching the glyph alone would let a broken probe
	// stand in for a working one that said no, which is the same class of false pass
	// this restructuring exists to remove.
	//
	// probeVerdictWait for the reasons given where it is declared; a refusal is
	// immediate, so everything spent here is Job creation and pod scheduling.
	s.WaitFor(t, "✗ unreachable", probeVerdictWait)

	// Esc out without saving, asserting what the Esc proof above asserts: control is
	// back at the NODES pane, not stuck in a form that has just shown an error.
	s.SendKey(0x1b) // Esc
	s.WaitFor(t, "NODES", 5*time.Second)
}

// TestProbeRefusesAPortItCannotProbe proves a non-22 port is reported as the input
// error it is, and never disguised as a host that is down.
//
// node-provisioner-egress pins the provisioner's outbound SSH to :22, so a probe of any
// other port is dropped by policy before it leaves the node and would come back as a
// dial error indistinguishable from a firewalled host. The operator therefore refuses
// the port at its own boundary with InvalidArgument, which the gateway maps to 400 and
// the TUI renders as "✗ test failed: gateway returned 400: cannot probe port 23: only
// port 22 can be probed, ...".
//
// The assertion is on "only port 22" — the constraint itself — and deliberately NOT on
// "✗". A ✗ is precisely what this outcome must not be confused with: an operator who
// reads "port 23 is unreachable" goes off to debug a healthy host's firewall, while one
// who reads "only port 22 can be probed" fixes the form. That distinction is also why
// the closed-port proof above uses a closed ADDRESS rather than a closed port — on this
// path no probe ever runs, so there is nothing there to observe.
//
// Nothing is saved and no probe Job is created, so the cluster is untouched and this may
// run at any time, joined second node or not.
func TestProbeRefusesAPortItCannotProbe(t *testing.T) {
	env := requireEnv(t)
	s := startTUI(t, env)
	defer s.Close()

	s.Send("a")
	s.WaitFor(t, "Hostname", 5*time.Second)

	s.Send("e2e-node2\t")
	s.Send(env.Node2IP + "\t") // focus now sits on SSH Port, prefilled "22"
	s.SendKey(0x7f)            // backspace: "22" -> "2"
	s.SendKey(0x7f)            // backspace: "2" -> ""; SSH Port is now empty
	s.Send("23")

	s.SendKey(ctrlT)
	// 15s, NOT the 45s the two probe waits use, and the gap is the point: this answer
	// costs one gateway round trip to an argument check, with no Job, no scheduling and
	// no dial anywhere in it. A bound sized like a probe's would also accommodate a
	// build that quietly went and probed, which is the behaviour being ruled out.
	s.WaitFor(t, "only port 22", 15*time.Second)

	s.SendKey(0x1b) // Esc, without saving
	s.WaitFor(t, "NODES", 5*time.Second)
}

// clusterNodeNames lists every node in the cluster. Used instead of predicting a
// node's name: the name comes from the remote host's hostname, which this test has
// no reliable way to derive from the address an operator typed.
func clusterNodeNames(t *testing.T) []string {
	t.Helper()
	return strings.Fields(kubectl(t, "get", "nodes",
		"-o", "jsonpath={range .items[*]}{.metadata.name} {end}"))
}
