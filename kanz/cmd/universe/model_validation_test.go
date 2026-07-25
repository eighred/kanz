package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestAddFormRefusesAnEmptyRequiredFieldWithoutCallingTheServer is the whole
// point of inline validation: an operator who submits a blank form finds out
// from the form, not from a round trip. Before this existed the submit read the
// key file first, so a blank Key Path came back as "read key : no such file or
// directory" — an error about a path nobody typed.
func TestAddFormRefusesAnEmptyRequiredFieldWithoutCallingTheServer(t *testing.T) {
	m := newModel(Config{}, stubSource{})
	u, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	m = u.(model)
	if !m.showForm {
		t.Fatal("pressing a did not open the Add Node form")
	}

	// Submit with every field empty except the prefilled port.
	u2, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m2 := u2.(model)

	if cmd != nil {
		t.Error("the form issued a request with an empty hostname and IP. Validate inline: " +
			"the operator should see the problem next to the field, not as a server error " +
			"after a round trip.")
	}
	if m2.formErr == nil {
		t.Fatal("a rejected submit produced no error; the form would sit there looking as " +
			"though enter did nothing at all")
	}
	if !m2.showForm {
		t.Error("a rejected submit closed the form; the operator must stay in it to fix the " +
			"field rather than retype everything")
	}

	// Every empty field is named, and named by the label on screen — the error
	// has to be pointable at a row without the operator translating field keys.
	got := m2.formErr.Error()
	for _, label := range []string{"Hostname", "IP", "User", "Key Path"} {
		if !strings.Contains(got, label) {
			t.Errorf("formErr %q does not name the empty field %q; an error that does not "+
				"say which field is wrong saves no round trip", got, label)
		}
	}
	// The rejection must reach the screen, not just the model.
	if !strings.Contains(m2.render(), got) {
		t.Errorf("the validation error never rendered; frame was:\n%s", m2.render())
	}
}

// TestAddFormNamesOnlyTheFieldStillEmpty proves the error tracks the form
// rather than being one canned string. A message that always lists all four
// fields would send an operator to re-check three they already filled.
func TestAddFormNamesOnlyTheFieldStillEmpty(t *testing.T) {
	m := newModel(Config{}, stubSource{})
	m.showForm = true
	m.form = addFormWith(t, map[string]string{
		"hostname": "london",
		"ip":       "10.0.0.5",
		"ssh_user": "ubuntu",
		"key_path": "", // the one omission
	})

	u, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		t.Fatal("a form with an empty Key Path still issued a request")
	}
	got := u.(model).formErr
	if got == nil {
		t.Fatal("a form with an empty Key Path submitted without an error")
	}
	if !strings.Contains(got.Error(), "Key Path") {
		t.Errorf("formErr %q does not name Key Path", got)
	}
	for _, filled := range []string{"Hostname", "10.0.0.5", "ubuntu"} {
		if strings.Contains(got.Error(), filled) {
			t.Errorf("formErr %q names %q, which the operator already filled", got, filled)
		}
	}
}

// TestAddFormRejectsWhitespaceOnlyValues covers the field an operator "filled"
// with a stray space. The server would reject it too, but only after the wait
// this validation exists to remove.
func TestAddFormRejectsWhitespaceOnlyValues(t *testing.T) {
	m := newModel(Config{}, stubSource{})
	m.showForm = true
	m.form = addFormWith(t, map[string]string{
		"hostname": "   ",
		"ip":       "10.0.0.5",
		"ssh_user": "ubuntu",
		"key_path": "/tmp/id_ed25519",
	})

	u, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		t.Fatal("a whitespace-only Hostname was accepted as filled in")
	}
	if got := u.(model).formErr; got == nil || !strings.Contains(got.Error(), "Hostname") {
		t.Errorf("formErr = %v, want an error naming Hostname", got)
	}
}

// TestAddFormWithAClearedPortStillSubmits pins the one deliberate exception.
// SSH Port is prefilled with 22, 22 is the only port this estate provisions
// over, and atoi32 already answers a cleared port with that same 22 — so a
// cleared port is defaulted, not rejected. This test is where that decision
// lives; if it ever flips to a required field, this is what says so.
func TestAddFormWithAClearedPortStillSubmits(t *testing.T) {
	keyPath := writeStubKey(t)
	m := newModel(Config{}, stubSource{})
	m.showForm = true
	m.form = addFormWith(t, map[string]string{
		"hostname": "london",
		"ip":       "10.0.0.5",
		"ssh_port": "", // cleared by the operator
		"ssh_user": "ubuntu",
		"key_path": keyPath,
	})

	u, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("a cleared SSH Port blocked the submit; it is prefilled and defaulted, not required")
	}
	if got := u.(model).formErr; got != nil {
		t.Errorf("formErr = %v, want nil for a cleared port", got)
	}
	if msg, ok := cmd().(addNodeResultMsg); !ok || msg.err != nil {
		t.Fatalf("submit did not reach addNode: %#v", msg)
	}
}

// TestAFullyPopulatedAddFormReachesTheServer is the guard on the validation
// itself. The live PTY proof (TestAddNodeProbesThenJoinsTheNodeLive) fills
// exactly these fields and expects a real join; validation that rejected them
// would break a proof that needs a cluster to notice.
func TestAFullyPopulatedAddFormReachesTheServer(t *testing.T) {
	keyPath := writeStubKey(t)
	m := newModel(Config{}, stubSource{})
	m.showForm = true
	m.form = addFormWith(t, map[string]string{
		"hostname": "e2e-node2",
		"ip":       "10.0.0.5",
		"ssh_user": "ubuntu",
		"key_path": keyPath,
	})

	u, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("a fully populated form was rejected — validation must not block the happy path")
	}
	if got := u.(model).formErr; got != nil {
		t.Errorf("formErr = %v, want nil for a complete form", got)
	}
	msg, ok := cmd().(addNodeResultMsg)
	if !ok {
		t.Fatalf("submit returned %T, want addNodeResultMsg", msg)
	}
	if msg.err != nil {
		t.Fatalf("submit failed: %v", msg.err)
	}
}

// TestAFixedFormClearsThePriorRejection covers the second half of the loop the
// operator is kept in: fix the named field, press enter again. A stale error
// left on the model would render underneath a form that is now correct.
func TestAFixedFormClearsThePriorRejection(t *testing.T) {
	keyPath := writeStubKey(t)
	m := newModel(Config{}, stubSource{})
	m.showForm = true
	m.form = newAddForm()

	u, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = u.(model)
	if m.formErr == nil {
		t.Fatal("the empty submit was not rejected")
	}

	m.form = addFormWith(t, map[string]string{
		"hostname": "london",
		"ip":       "10.0.0.5",
		"ssh_user": "ubuntu",
		"key_path": keyPath,
	})
	u2, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("the corrected form was rejected")
	}
	if got := u2.(model).formErr; got != nil {
		t.Errorf("formErr = %v after a corrected submit; the operator would still be reading "+
			"the error they just fixed", got)
	}
}

// TestAFailedActionSurfacesAndLeavesTheEstateAlone covers the other failure
// path an operator hits: the action was accepted by the TUI and refused by the
// server. Nothing about the estate may change on that path — the next poll is
// the source of truth — but the operator must see that the key did not take.
func TestAFailedActionSurfacesAndLeavesTheEstateAlone(t *testing.T) {
	rows := []nodeRow{{Name: "n1", Status: "Ready", schedulable: true}}
	m := newModel(Config{}, stubSource{msg: fetchMsg{nodes: rows}})
	u, _ := m.Update(fetchMsg{nodes: rows})
	m = u.(model)

	u2, cmd := m.Update(nodeActionMsg{err: errStub{}})
	m2 := u2.(model)
	if cmd != nil {
		t.Error("a failed action scheduled more work; the next poll already reports reality")
	}
	if m2.actionErr == nil {
		t.Error("a failed action produced no visible error — silence is the worst outcome " +
			"for an operator who just pressed a destructive key")
	}
	if len(m2.nodes) != 1 || m2.nodes[0].Name != "n1" {
		t.Error("a failed action mutated the node list; the next poll is the source of truth")
	}
	frame := m2.render()
	if !strings.Contains(frame, "n1") {
		t.Errorf("the estate stopped rendering after a failed action; frame was:\n%s", frame)
	}
	if !strings.Contains(frame, errStub{}.Error()) {
		t.Errorf("the action error never reached the screen; frame was:\n%s", frame)
	}
}

// TestASucceedingActionClearsThePriorError is the same argument in reverse: an
// error that outlives the action it described tells the operator a cordon that
// worked did not.
func TestASucceedingActionClearsThePriorError(t *testing.T) {
	m := newModel(Config{}, stubSource{})
	m.nodes = []nodeRow{{Name: "n1", schedulable: true}}
	m.actionErr = errStub{}

	u, _ := m.Update(nodeActionMsg{err: nil})
	if got := u.(model).actionErr; got != nil {
		t.Errorf("actionErr = %v after a successful action; a stale error reports a failure "+
			"that did not happen", got)
	}
}

// addFormWith returns a fresh Add Node form with the named fields set. Keys are
// checked against the real form so a renamed field fails here rather than
// silently leaving a test asserting on an empty value.
func addFormWith(t *testing.T, values map[string]string) addForm {
	t.Helper()
	f := newAddForm()
	for k, v := range values {
		found := false
		for i := range f.fields {
			if f.fields[i].key == k {
				f.fields[i].value = v
				found = true
			}
		}
		if !found {
			t.Fatalf("addFormWith: no such form field %q", k)
		}
	}
	return f
}

// writeStubKey writes a throwaway file for Key Path to name, so a submit that
// passes validation can be driven all the way to addNode. The contents are
// irrelevant — the stub source never looks at the bytes.
func writeStubKey(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(path, []byte("stub-key"), 0o600); err != nil {
		t.Fatalf("write stub key: %v", err)
	}
	return path
}
