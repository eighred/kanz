package universe

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestAddFormCollectsFields(t *testing.T) {
	f := newAddForm()
	f = typeInto(f, "london") // hostname (field 0)
	f = f.next()
	f = typeInto(f, "10.0.0.5") // ip (field 1)
	if f.value("hostname") != "london" || f.value("ip") != "10.0.0.5" {
		t.Fatalf("form did not capture fields: %+v", f)
	}
}

func TestAddFormBackspace(t *testing.T) {
	f := newAddForm()
	f = typeInto(f, "lonX")
	f = f.backspace()
	if f.value("hostname") != "lon" {
		t.Errorf("backspace failed: %q", f.value("hostname"))
	}
}

func TestAddFormRendersFocusedField(t *testing.T) {
	f := newAddForm()
	out := f.render()
	for _, want := range []string{"Add Node", "Hostname", "IP", "SSH Port", "User", "Key Path"} {
		if !strings.Contains(out, want) {
			t.Errorf("form render missing %q", want)
		}
	}
}

// typeInto feeds a string to the form as individual key runes.
func typeInto(f addForm, s string) addForm {
	for _, r := range s {
		f = f.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	return f
}

func TestCtrlTFiresTestConnection(t *testing.T) {
	m := NewModel(Config{}, stubSource{})
	m.showForm = true
	m.form = newAddForm()
	// focus/complete the ip field then press ctrl+t
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlT})
	if cmd == nil {
		t.Fatal("ctrl+t should return a test-connection command")
	}
}

func TestTestConnResultRenders(t *testing.T) {
	m := Model{showForm: true, form: newAddForm(), testResult: "✓ reachable (7ms)"}
	if !strings.Contains(m.render(), "reachable (7ms)") {
		t.Errorf("form render should show the test result:\n%s", m.render())
	}
}

// TestTestConnectionShowsInFlightState covers the whole lifecycle of the in-flight
// state. The probe stopped being an in-process dial and became a Kubernetes Job, so the
// form now sits for seconds after the keypress; with nothing on screen an operator reads
// that as a hung TUI and either kills it mid-provision or mashes the key.
func TestTestConnectionShowsInFlightState(t *testing.T) {
	m := NewModel(Config{}, stubSource{})
	m.showForm = true
	m.form = newAddForm()
	m.testResult = "✓ reachable (7ms)" // a previous probe's verdict

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlT})
	m = next.(Model)
	if cmd == nil {
		t.Fatal("ctrl+t should return a test-connection command")
	}
	if !m.probing {
		t.Error("ctrl+t must set the in-flight state before the command runs — the screen has to " +
			"change on the keypress, not seconds later when the Job answers")
	}
	if m.testResult != "" {
		t.Errorf("testResult = %q, want it cleared — the last probe's verdict is not this one's",
			m.testResult)
	}
	if out := m.render(); !strings.Contains(out, "probing") {
		t.Errorf("form render must show the probe is running:\n%s", out)
	}
}

// TestTestConnectionResultClearsInFlightState checks BOTH outcomes release the state.
// Clearing it only on success is how a form ends up permanently refusing to probe again
// after the first error — the state that most needs a retry.
func TestTestConnectionResultClearsInFlightState(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  testConnResultMsg
	}{
		{"reachable", testConnResultMsg{res: testConnResult{reachable: true, latencyMs: 7}}},
		{"unreachable", testConnResultMsg{res: testConnResult{message: "i/o timeout"}}},
		{"probe failed", testConnResultMsg{err: errors.New("probe job did not complete in time")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := Model{showForm: true, form: newAddForm(), probing: true}
			next, _ := m.Update(tc.msg)
			if next.(Model).probing {
				t.Error("the in-flight state survived the result — a further probe is now impossible")
			}
		})
	}
}

// TestSecondTestConnectionWhileProbingIsANoOp is the assertion that matters more than
// the visual cue: every keypress costs a Job in the operator's namespace, holding :22
// egress. An operator who believes the TUI is wedged presses the key repeatedly.
func TestSecondTestConnectionWhileProbingIsANoOp(t *testing.T) {
	m := Model{showForm: true, form: newAddForm(), src: stubSource{}, probing: true}
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlT})
	if cmd != nil {
		t.Error("a second ctrl+t while a probe is in flight must not fire another one — each one " +
			"is another Job with :22 egress")
	}
	if !next.(Model).probing {
		t.Error("the ignored keypress must leave the in-flight state alone")
	}
}
