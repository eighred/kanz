package main

import (
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
	m := newModel(Config{}, stubSource{})
	m.showForm = true
	m.form = newAddForm()
	// focus/complete the ip field then press ctrl+t
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlT})
	if cmd == nil {
		t.Fatal("ctrl+t should return a test-connection command")
	}
}

func TestTestConnResultRenders(t *testing.T) {
	m := model{showForm: true, form: newAddForm(), testResult: "✓ reachable (7ms)"}
	if !strings.Contains(m.render(), "reachable (7ms)") {
		t.Errorf("form render should show the test result:\n%s", m.render())
	}
}
