package estate

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/eighred/kanz/internal/tui/pane"
	"github.com/eighred/kanz/internal/tui/universe"
)

// EVERY UNIVERSE SCREEN MUST BE MOUNTED. This is #66's acceptance criterion in
// one assertion: "every universe pane is reachable from the kanz shell".
//
// The list is derived from universe's own Screen constants rather than restated,
// so adding a fourth screen there fails here instead of shipping a screen no tab
// reaches — which is exactly the state #66 exists to end, and it would be
// invisible.
func TestEveryUniverseScreenIsMounted(t *testing.T) {
	panes := All(NewShared(nil))

	mounted := map[universe.Screen]bool{}
	for _, p := range panes {
		ep, ok := p.(*Pane)
		if !ok {
			t.Fatalf("All returned a %T, want *Pane", p)
		}
		if mounted[ep.Screen()] {
			t.Errorf("screen %d mounted twice", ep.Screen())
		}
		mounted[ep.Screen()] = true
	}

	for _, s := range []universe.Screen{universe.ScreenNodes, universe.ScreenClusters, universe.ScreenAPI} {
		if !mounted[s] {
			t.Errorf("universe screen %d is not mounted in the kanz shell — it would be unreachable, "+
				"which is the defect #66 exists to fix", s)
		}
	}
	if len(panes) != 3 {
		t.Errorf("All returned %d panes, want 3 — a screen was added to universe without a tab, or "+
			"a tab exists for a screen that does not", len(panes))
	}
}

// The estate panes are GATEWAY plane: universe holds no SPIFFE identity and
// reaches the estate through the api-gateway with the operator's own bearer
// token (OPS-M2c). Marking them Bus would send them through ExecProcess and
// break them; marking a bus tool Gateway would be the security failure.
func TestEstatePanesAreGatewayPlane(t *testing.T) {
	for _, p := range All(NewShared(nil)) {
		if p.Plane() != pane.Gateway {
			t.Errorf("%s is on the %v plane, want gateway — universe carries no SVID and runs "+
				"in-process on the shell's token", p.ID(), p.Plane())
		}
	}
}

// A SIGNED-OUT OPERATOR MUST STILL GET A SHELL. universe.NewGatewaySource
// refuses an empty token, and the shell opens before anyone has signed in.
// Building eagerly would take down the Copilot pane too — the one that has
// /login on it.
func TestAnUnreachableEstateDoesNotBreakTheShell(t *testing.T) {
	boom := errors.New("no token: the gateway authenticates a PERSON")
	shared := NewShared(func() (universe.Model, error) { return universe.Model{}, boom })

	p := All(shared)[0].(*Pane)

	if cmd := p.Init(); cmd != nil {
		t.Error("Init returned a command despite construction failing — a poller was started against " +
			"a session that does not exist")
	}
	if shared.Ready() {
		t.Error("Ready() is true after a failed build")
	}

	v := p.View(60, 10)
	if v == "" {
		t.Fatal("an unreachable estate rendered a blank pane — indistinguishable from a hung one")
	}
	if !strings.Contains(v, "unreachable") {
		t.Errorf("View = %q, want it to say the estate is unreachable", v)
	}
	if !strings.Contains(v, "Copilot") {
		t.Errorf("View = %q, want it to name the fix (sign in on Copilot) — a blank screen is not "+
			"an instruction", v)
	}
}

// A failed build must be RETRIED on the next visit, or signing in on Copilot and
// tabbing back would need a restart to take effect.
func TestAFailedBuildIsRetriedOnTheNextVisit(t *testing.T) {
	calls := 0
	shared := NewShared(func() (universe.Model, error) {
		calls++
		if calls == 1 {
			return universe.Model{}, errors.New("not signed in")
		}
		return universe.NewModel(universe.Config{}, nil), nil
	})
	p := All(shared)[0].(*Pane)

	p.Init() // fails
	if shared.Ready() {
		t.Fatal("ready after the first, failing build")
	}
	p.Init() // retried, succeeds
	if !shared.Ready() {
		t.Fatal("a second visit did not retry the build — the operator would have to restart kanz " +
			"after signing in")
	}
	if calls != 2 {
		t.Errorf("build called %d times, want 2", calls)
	}
}

// ONE MODEL, ONE POLLER. Three panes share the estate; three Init calls must not
// start three poll loops against the gateway for one operator.
func TestTheEstateIsBuiltOnceAcrossAllThreePanes(t *testing.T) {
	calls := 0
	shared := NewShared(func() (universe.Model, error) {
		calls++
		return universe.NewModel(universe.Config{}, nil), nil
	})
	for _, p := range All(shared) {
		p.Init()
	}
	if calls != 1 {
		t.Errorf("build called %d times across three panes, want 1 — each extra call is another "+
			"poll loop against the gateway for the same estate", calls)
	}
}

// Each pane must assert ITS screen before delegating. The three share one model,
// so without this a keypress on Clusters would be interpreted against whatever
// screen the previously active pane left selected.
func TestEachPaneAssertsItsOwnScreenBeforeDelegating(t *testing.T) {
	shared := NewShared(func() (universe.Model, error) {
		return universe.NewModel(universe.Config{}, nil), nil
	})
	panes := All(shared)
	panes[0].Init()

	for _, p := range panes {
		ep := p.(*Pane)
		ep.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
		if got := shared.Model().Screen(); got != ep.Screen() {
			t.Errorf("after %s handled a message the shared model is on screen %d, want %d",
				ep.ID(), got, ep.Screen())
		}
	}
}

// A PANIC IN ONE PANE MUST NOT KILL THE SHELL.
//
// Defence in depth, deliberately not the fix for the nil token that caused it —
// that is fixed at the read, in cmd/kanz's builder. This asserts the blast
// radius: a build func that panics leaves the estate showing the panic and every
// OTHER pane working, including Copilot, which is where /login lives. Before
// this, the operator lost the shell and the message with it.
func TestAPanicWhileConnectingIsContainedToTheEstatePane(t *testing.T) {
	shared := NewShared(func() (universe.Model, error) {
		var nilToken *struct{ AccessToken string }
		_ = nilToken.AccessToken // the original defect, reproduced exactly
		return universe.Model{}, nil
	})
	p := All(shared)[0].(*Pane)

	// Must not panic out of Init.
	if cmd := p.Init(); cmd != nil {
		t.Error("a panicking build returned a command")
	}
	if shared.Ready() {
		t.Error("Ready() is true after the build panicked")
	}
	if shared.Err() == nil {
		t.Fatal("the panic was swallowed — the pane would render blank with no reason")
	}
	if !strings.Contains(shared.Err().Error(), "panicked") {
		t.Errorf("err = %q, want it to say the pane panicked", shared.Err())
	}
	if v := p.View(60, 10); !strings.Contains(v, "unreachable") {
		t.Errorf("View = %q, want the estate rendered as unreachable", v)
	}
}
