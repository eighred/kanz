package tui

import (
	"encoding/base64"
	"regexp"
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

// selectionMarker is the glyph cmd/universe/view.go's renderNodes puts in front of
// the highlighted row (styleSelected.Render("▸ ")). It appears nowhere else in the
// Nodes pane, which is what makes the selection machine-readable from the pty stream
// at all. It IS also used by the two forms, so every helper below assumes the Nodes
// pane is on screen — which is the only state these cordon proofs ever put it in.
const selectionMarker = "▸"

// ansiSeq matches the escape sequences Bubble Tea and lipgloss interleave with the
// text: CSI (colour, erase-line, alt-screen, cursor moves), OSC, and the short
// charset/keypad forms. Stripping them is not cosmetic — the marker is styled, so in
// the raw capture the glyph is wrapped in SGR escapes and is NOT textually adjacent
// to the node name that follows it. Whether those escapes are present at all depends
// on the colour profile lipgloss infers from the environment, so a matcher that
// assumed either shape would be a proof that passes or fails on TERM rather than on
// what the TUI did.
var ansiSeq = regexp.MustCompile(
	"\x1b\\[[0-9;?]*[ -/]*[@-~]|\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\)|\x1b[()][0-9A-Za-z]|\x1b[=>]")

func stripANSI(s string) string { return ansiSeq.ReplaceAllString(s, "") }

// selectedNodeName reports which node the Nodes pane highlight is on RIGHT NOW, read
// off the last complete marker line in the capture.
//
// "Last", not "any": the capture is cumulative, so a Contains over the whole buffer
// answers "was this row ever highlighted", which is a different and far more
// dangerous question — it says yes about a row the selection has already moved past.
// Bubble Tea's standard renderer rewrites only the lines that changed, top to bottom
// (the paint loop in standard_renderer.go), so the most recent line still carrying
// the marker is the row the highlight sits on now. That holds for a move in either
// direction and for a full repaint on the 3s poll, since exactly one rendered row
// carries the marker in any frame.
//
// The final segment is dropped: the reader goroutine takes 4096-byte chunks and can
// split a line, and a half-written row must never be read as a selection.
func selectedNodeName(s *Session) (string, bool) { return selectedNodeIn(s.Frames()) }

// selectedNodeIn is selectedNodeName's parser, split out so it can be proved against
// hand-built captures (selection_test.go) on a machine with no cluster and no pty.
func selectedNodeIn(capture string) (string, bool) {
	lines := strings.Split(stripANSI(capture), "\n")
	for i := len(lines) - 2; i >= 0; i-- {
		_, after, found := strings.Cut(lines[i], selectionMarker)
		if !found {
			continue
		}
		if f := strings.Fields(after); len(f) > 0 {
			return f[0], true
		}
	}
	return "", false
}

// navStepTimeout bounds one arrow key: how long the TUI gets to redraw the two rows a
// selection move changes. It is a keystroke-latency bound of the same class as the 5s
// form and pane waits above, not a bound on any call — moving the highlight is a local
// model update with no I/O anywhere in it.
const navStepTimeout = 5 * time.Second

// maxNavSteps caps the walk. It is belt and braces only: the real termination
// condition is an arrow key that fails to move the highlight, which means the list has
// no more rows and is reported as such.
const maxNavSteps = 64

// selectNode2 moves the Nodes-pane selection onto node 2 and does not return until the
// pane has REDRAWN with the highlight there. Node ordering is the cluster's, so the
// helper searches rather than assuming an index.
//
// Call it before sending any other key: the WaitFor below relies on the capture mark
// still being 0 (see driver.go) so that it searches the whole startup frame rather
// than output since a keystroke.
//
// This is a NAVIGATION loop, not a retry loop, and every step waits for the highlight
// to actually move before pressing again. That synchronisation is the whole point, and
// it replaces a fixed sleep between keypresses, which races: a redraw slower than the
// sleep lets the next arrow key overshoot the target, and a Contains over the
// cumulative capture would then still find node 2 marked — in a frame the selection
// has already left. The proof would go on to cordon whatever row it overshot onto. On
// a two-node cluster the only other row is the control plane.
func selectNode2(t *testing.T, s *Session, node2 string) {
	t.Helper()
	s.WaitFor(t, node2, 10*time.Second)

	for step := 0; step < maxNavSteps; step++ {
		cur, ok := waitForSelection(s, "", navStepTimeout)
		if !ok {
			t.Fatalf("the Nodes pane never drew a %q selection marker within %s. If the TUI "+
				"does not mark the selected row in a machine-readable way, that is the finding: "+
				"an operator cannot tell which node an action will hit either.\n"+
				"--- captured output ---\n%s\n--- end ---",
				selectionMarker, navStepTimeout, s.Frames())
		}
		if cur == node2 {
			return
		}
		s.Send("\x1b[B") // down arrow
		if _, moved := waitForSelection(s, cur, navStepTimeout); !moved {
			t.Fatalf("the selection stayed on %s after a down arrow, so the Nodes pane's list "+
				"ends there and %s is not below it. Stopping here rather than pressing on: the "+
				"next action key would act on %s.", cur, node2, cur)
		}
	}
	t.Fatalf("walked %d rows without reaching %s in the Nodes pane", maxNavSteps, node2)
}

// waitForSelection polls the capture until the highlighted row is a node other than
// notName and returns its name; pass "" to wait for any highlighted row at all. It
// returns false rather than failing the test, because both of its callers have a more
// specific thing to say about a timeout than "nothing happened".
func waitForSelection(s *Session, notName string, timeout time.Duration) (string, bool) {
	deadline := time.Now().Add(timeout)
	for {
		if name, ok := selectedNodeName(s); ok && name != notName {
			return name, true
		}
		if time.Now().After(deadline) {
			return "", false
		}
		time.Sleep(pollInterval)
	}
}

// TestCordonAndUncordonChangeTheCluster proves c and u reach Kubernetes. Both keys
// fire directly from the Nodes pane with no confirmation (cmd/universe/model.go —
// only drain is gated behind y/n), so the keypress is the whole interaction and
// .spec.unschedulable on the node is the whole assertion.
func TestCordonAndUncordonChangeTheCluster(t *testing.T) {
	env := requireEnv(t)
	node2 := waitForNodeReady(t, 2*time.Minute)
	if !nodeSchedulable(t, node2) {
		kubectl(t, "uncordon", node2) // start from a known state
	}

	s := startTUI(t, env)
	defer s.Close()
	selectNode2(t, s, node2)

	s.Send("c")
	waitForSchedulable(t, node2, false, 30*time.Second)

	s.Send("u")
	waitForSchedulable(t, node2, true, 30*time.Second)
}

// openDrainConfirm presses d and does not return until the prompt ON SCREEN names the
// node this proof intends to act on.
//
// Waiting for the NAME, not just for "y/n", is what stops this proof from ever
// answering a prompt armed on the control plane. m.selected is an INDEX into a list the
// 3s poll rebuilds (cmd/universe/model.go), so a reordering of the node list between
// selectNode2 and this keypress would move the highlight with no keystroke sent and no
// way for the harness to notice. The prompt is the TUI stating which node it is about
// to drain, and it is the last checkable moment before y commits.
//
// Both waits search the output since the d keypress (driver.go's mark), so neither can
// be satisfied by the previous branch's prompt still sitting in the capture.
func openDrainConfirm(t *testing.T, s *Session, node string) {
	t.Helper()
	s.Send("d")
	// "Drain <name>? evicts %d pods  [y/n]" — cmd/universe/view.go's renderNodes, one
	// lipgloss.Render of one line, so the text is contiguous in the pty stream whether or
	// not the colour profile wraps it in SGR escapes.
	s.WaitFor(t, "Drain "+node+"?", 5*time.Second)
	s.WaitFor(t, "y/n", 5*time.Second)
}

// abortSettle is how long the abort branch lets a wrongly-fired drain run before it
// looks at the cluster.
//
// This is the one sleep in the suite that is not a masking sleep, and the distinction is
// the whole point. Every other wait here polls for something to START existing, where a
// sleep would pass on a fast machine and flake on a slow one. This branch asserts a
// NEGATIVE — that nothing happened — and a negative cannot be polled for: checking
// instantly after n would pass even against a build that fires the drain on every key,
// because the eviction had not landed yet. The sleep is the window in which a wrong
// build gets to convict itself. The operator's drain cordons synchronously before it
// returns and its eviction loop's first pass is immediate, so 3s is several times what a
// wrongly-fired drain needs to become visible.
const abortSettle = 3 * time.Second

// TestDrainConfirmAbortsAndProceeds proves the drain confirm dialog in BOTH directions.
//
// The n branch is the one that matters and is usually untested: a confirm dialog that
// acts on the wrong answer is worse than no dialog, because the operator has been taught
// it is safe to press d and look. The y branch then proves the same dialog is not merely
// decorative.
//
// Every assertion is scoped to node 2 plus one on node 1 — draining one node must not
// disturb the trading loop on another, which is the reason a second node was provisioned
// rather than draining the rig.
func TestDrainConfirmAbortsAndProceeds(t *testing.T) {
	env := requireEnv(t)
	node2 := waitForNodeReady(t, 2*time.Minute)
	node1 := controlPlaneNodeName(t)
	if node1 == node2 {
		t.Fatalf("waitForNodeReady returned the control plane (%s), so this proof would drain "+
			"the node running the trading loop. Refusing.", node1)
	}
	if !nodeSchedulable(t, node2) {
		// A previous run that died between the drain and its uncordon leaves the node
		// cordoned, which would fail the abort branch for something this run did not do.
		kubectl(t, "uncordon", node2)
	}
	// Deferred, not trailing: if any assertion below fails the node must still be handed
	// back schedulable, or the next run starts from a cordoned node and the suite decays
	// into a state where each failure poisons the one after it. Registered before the TUI
	// starts so it unwinds after s.Close() — cluster restored once the client is gone.
	defer kubectl(t, "uncordon", node2)

	// The baseline must hold still before it can be trusted; see waitForStableEvictablePods.
	evictable := waitForStableEvictablePods(t, node2)
	if len(evictable) == 0 {
		t.Fatal("node 2 runs no evictable pods, so a drain would prove nothing — both branches " +
			"would pass against a TUI that does not implement drain at all. Apply the e2e-drain " +
			"workload (task 6, step 1) first.")
	}
	before1 := len(podsOnNode(t, node1))

	s := startTUI(t, env)
	defer s.Close()
	selectNode2(t, s, node2)

	// BRANCH 1: n must abort. Nothing evicted, node stays schedulable.
	openDrainConfirm(t, s, node2)
	s.Send("n")
	time.Sleep(abortSettle)
	if gone := missingFrom(evictable, podsOnNode(t, node2)); len(gone) > 0 {
		t.Errorf("answering n to the drain confirm evicted %v anyway. A confirm dialog that "+
			"acts on the wrong answer is worse than no dialog.", gone)
	}
	if !nodeSchedulable(t, node2) {
		t.Error("answering n cordoned the node; abort must change nothing at all. This is the " +
			"earliest visible half of a drain — Ops.Drain cordons before it evicts — so it " +
			"fires even when the eviction has not landed yet.")
	}

	// BRANCH 2: y must drain. Drain cordons first and evicts in the background, so the
	// cordon is the fast signal and the eviction is the real one; assert both.
	openDrainConfirm(t, s, node2)
	s.Send("y")
	waitForSchedulable(t, node2, false, 60*time.Second)
	waitForPodsEvicted(t, node2, evictable, 3*time.Minute)

	// The estate must be untouched: drain is scoped to the selected node. This is also
	// the backstop for the residual race openDrainConfirm cannot close — if the highlight
	// had moved to node 1 between the prompt and the y, node 1's pods would be gone.
	if after1 := len(podsOnNode(t, node1)); after1 != before1 {
		t.Errorf("draining node 2 changed the pod count on node 1 (%d -> %d) — the trading "+
			"loop must not be affected by draining another node", before1, after1)
	}
}

// clusterNodeNames lists every node in the cluster. Used instead of predicting a
// node's name: the name comes from the remote host's hostname, which this test has
// no reliable way to derive from the address an operator typed.
func clusterNodeNames(t *testing.T) []string {
	t.Helper()
	return strings.Fields(kubectl(t, "get", "nodes",
		"-o", "jsonpath={range .items[*]}{.metadata.name} {end}"))
}

// TestMoveRegionRelabelsTheNode proves m reaches Kubernetes: the operator types a
// region into the prompt cmd/universe/view.go renders ("Move <node> to region:
// <input>_  [enter] move  [esc] cancel"), and the node's own
// topology.kubernetes.io/region label — never the TUI's rendering of the move — is the
// assertion.
//
// Both ends are left exactly as found. The setup strips a pre-existing "asia" label so
// the move always has somewhere to go, and the deferred cleanup below strips whatever
// this run set, so a re-run never starts from the label the previous run committed.
// Without that defer, a first run would leave the node labelled "asia" and a second run
// would have its own setup delete that label out from under it — survivable, but not
// the standard the other proofs here hold to (see TestDrainConfirmAbortsAndProceeds's
// defer kubectl(t, "uncordon", node2) for the same pattern). Registered before the TUI
// starts so it unwinds after s.Close() — cluster restored once the client is gone.
func TestMoveRegionRelabelsTheNode(t *testing.T) {
	env := requireEnv(t)
	node2 := waitForNodeReady(t, 2*time.Minute)
	const want = "asia"
	if nodeLabel(t, node2, "topology.kubernetes.io/region") == want {
		kubectl(t, "label", "node", node2, "topology.kubernetes.io/region-")
	}
	defer kubectl(t, "label", "node", node2, "topology.kubernetes.io/region-")

	s := startTUI(t, env)
	defer s.Close()
	selectNode2(t, s, node2)

	s.Send("m")
	s.WaitFor(t, "to region:", 5*time.Second) // the prompt is "Move <node> to region: _"
	s.Send(want + "\r")

	waitForRegion(t, node2, want, 30*time.Second)
}

// keyFormVenue is the venue this proof drives the Set API Keys form for, and the choice
// is load-bearing rather than arbitrary.
//
// newKeyForm (cmd/universe/keyform.go) gives binance TWO fields — API Key, API Secret —
// and okx THREE, appending a Passphrase. The fill below is written for two, so on a
// three-field form the Enter that saves would instead land one field early and the canary
// would be typed into a field this proof does not track. openKeyForm therefore asserts
// WHICH venue the form opened for before a single character is sent.
const keyFormVenue = "binance"

// leakLogWindow is how far back the log leak check reads. It must cover the whole run,
// not just the write: the form's submit is the LAST thing this proof does, but a leak can
// just as easily have been logged when the TUI dialled the gateway at startup.
const leakLogWindow = "5m"

// openKeyForm presses k from the API Manager pane and does not return until the form ON
// SCREEN names the venue this proof is about to type a credential into.
//
// This mirrors openDrainConfirm: the last checkable moment before an irreversible key is
// pressed is the TUI stating what it is about to act on. m.apiSelected is an INDEX into a
// venue list the poll rebuilds (cmd/universe/model.go), so a reordering between the pane
// appearing and this keypress moves the selection with no keystroke sent. Waiting only for
// "API Secret" would not catch it — BOTH venues render that label — and the fill would
// then misalign against okx's extra field and write a credential into the wrong one.
//
// Both waits search the output since the k keypress (driver.go's mark), so neither can be
// satisfied by anything already in the capture.
func openKeyForm(t *testing.T, s *Session, venue string) {
	t.Helper()
	s.Send("k")
	// "Set API Keys — <venue>", one lipgloss.Render of one short line (keyform.go's
	// render), so the text is contiguous in the pty stream whether or not the colour
	// profile wraps it in SGR escapes.
	s.WaitFor(t, "Set API Keys — "+venue, 5*time.Second)
	s.WaitFor(t, "API Secret", 5*time.Second)
}

// TestVenueKeysWrittenFromTheFormAndNeverLeaked proves the API Manager's key form reaches
// Kubernetes, and that the credential it carried never surfaced anywhere it could be read.
//
// OPS-M2d proved this write over HTTP with curl. This proves the FORM that is supposed to
// call it — the gap the board flagged. The leak check is EXECUTED rather than assumed: a
// canary is typed into the masked field and then searched for in every captured frame and
// in every pod log of both services on the path.
func TestVenueKeysWrittenFromTheFormAndNeverLeaked(t *testing.T) {
	env := requireEnv(t)
	const (
		canary = "E2E-CANARY-SECRET-8f3a"
		ns     = "kanz-services"
	)
	secret := "venue-" + keyFormVenue + "-keys"

	// Start from no Secret at all, so waitForSecret below is watching this run's write
	// rather than finding a previous one's and calling it proof.
	kubectl(t, "-n", ns, "delete", "secret", secret, "--ignore-not-found")
	// Deferred, not trailing: every assertion below is an Errorf or a Fatalf, and a
	// trailing delete is skipped by both. That would leave a live credential Secret in
	// the cluster on exactly the runs that went wrong — and leave the next run to find a
	// pre-existing Secret. Registered before the TUI starts so it unwinds after s.Close().
	defer kubectl(t, "-n", ns, "delete", "secret", secret, "--ignore-not-found")

	s := startTUI(t, env)
	defer s.Close()

	s.Send("\t\t") // Nodes -> Clusters -> API Manager
	s.WaitFor(t, keyFormVenue, 10*time.Second)
	openKeyForm(t, s, keyFormVenue)

	// Two fields, proven to be two by openKeyForm's title assertion: tab off API Key,
	// then Enter saves from API Secret.
	s.Send("e2e-key\t")
	s.Send(canary + "\r")

	keys := waitForSecret(t, ns, secret, 30*time.Second)
	if missing := missingFrom([]string{"api-key", "api-secret"}, keys); len(missing) > 0 {
		t.Errorf("secret data keys are %v, missing %v. HYPHENS matter: the dev rig mounts "+
			"the Secret as a plain volume with no CSI mapping, so the data key IS the "+
			"filename the adapter reads.", keys, missing)
	}

	// The keys above are written unconditionally by the operator, empty values included,
	// so their presence alone does not establish that the canary ever entered the
	// credential path — and a leak check for a string that was never written is a check
	// that cannot fail. Length, compared in base64 so the value itself is never read out
	// of the cluster, is what makes the search below mean something.
	wantLen := len(base64.StdEncoding.EncodeToString([]byte(canary)))
	if got := secretValueEncodedLen(t, ns, secret, "api-secret"); got != wantLen {
		t.Fatalf("api-secret in %s/%s encodes to %d base64 chars, want %d for the %d-byte "+
			"canary. The form did not put what was typed into the masked field — so the "+
			"leak check below would be searching for a string that was never submitted.",
			ns, secret, got, wantLen, len(canary))
	}

	// LEAK CHECK, executed rather than assumed.
	//
	// ANSI is stripped first. The canary contains no escape bytes, so stripping can only
	// JOIN a leak that a style sequence had split — it can never hide one, and it can
	// never manufacture one. A raw search is therefore strictly weaker here.
	if strings.Contains(stripANSI(s.Frames()), canary) {
		t.Error("the submitted credential appeared in a rendered frame — a masked field that " +
			"echoes on any screen is a credential on a shared terminal")
	}
	for _, target := range []struct{ ns, deployment string }{
		{"kanz-services", "api-gateway"},
		{"kanz-operator", "operator"},
	} {
		if strings.Contains(deploymentLogs(t, target.ns, target.deployment, leakLogWindow), canary) {
			t.Errorf("the credential appeared in %s/%s logs", target.ns, target.deployment)
		}
	}
}
