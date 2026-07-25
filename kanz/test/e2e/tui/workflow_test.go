package tui

import (
	"testing"
	"time"
)

// startTUI launches the real binary against the real gateway and waits until it has
// rendered its first estate poll.
func startTUI(t *testing.T, env Env) *Session {
	t.Helper()
	s := Start(t, env.Binary, nil, env.sessionEnv())
	s.WaitFor(t, "Nodes", 20*time.Second)
	return s
}

func TestNavigationReachesEveryPaneAndQuitsCleanly(t *testing.T) {
	env := requireEnv(t)
	s := startTUI(t, env)
	defer s.Close()

	// tab cycles Nodes -> Clusters -> API Manager -> Nodes. Assert on the pane's own
	// domain text, never on layout.
	s.Send("\t")
	s.WaitFor(t, "Clusters", 5*time.Second)
	s.Send("\t")
	s.WaitFor(t, "API", 5*time.Second)
	s.Send("\t")
	s.WaitFor(t, "Nodes", 5*time.Second)
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
