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

// A form must not submit on quit. An operator who opens Add Node, changes their mind
// and presses q must not have provisioned anything.
func TestQuitFromInsideAFormSubmitsNothing(t *testing.T) {
	env := requireEnv(t)
	before := len(kubectlLines(t, "get", "jobs", "-n", "kanz-operator", "-o", "name"))

	s := startTUI(t, env)
	s.Send("a")
	s.WaitFor(t, "Hostname", 5*time.Second)
	s.Send("q")
	s.Close()

	after := len(kubectlLines(t, "get", "jobs", "-n", "kanz-operator", "-o", "name"))
	if after != before {
		t.Errorf("quitting from inside the Add Node form changed the Job count (%d -> %d); "+
			"abandoning a form must provision nothing", before, after)
	}
}
