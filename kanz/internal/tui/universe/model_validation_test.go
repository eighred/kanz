package universe

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// TestAddFormRefusesAnEmptyRequiredFieldWithoutCallingTheServer is the whole
// point of inline validation: an operator who submits a blank form finds out
// from the form, not from a round trip. Before this existed the submit read the
// key file first, so a blank Key Path came back as "read key : no such file or
// directory" — an error about a path nobody typed.
func TestAddFormRefusesAnEmptyRequiredFieldWithoutCallingTheServer(t *testing.T) {
	m := NewModel(Config{}, stubSource{})
	u, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	m = u.(Model)
	if !m.showForm {
		t.Fatal("pressing a did not open the Add Node form")
	}

	// Submit with every field empty except the prefilled port.
	u2, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m2 := u2.(Model)

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
	// The rejection must reach the screen, not just the Model.
	if !strings.Contains(m2.render(), got) {
		t.Errorf("the validation error never rendered; frame was:\n%s", m2.render())
	}
}

// TestAddFormNamesOnlyTheFieldStillEmpty proves the error tracks the form
// rather than being one canned string. A message that always lists all four
// fields would send an operator to re-check three they already filled.
func TestAddFormNamesOnlyTheFieldStillEmpty(t *testing.T) {
	m := NewModel(Config{}, stubSource{})
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
	got := u.(Model).formErr
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
	m := NewModel(Config{}, stubSource{})
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
	if got := u.(Model).formErr; got == nil || !strings.Contains(got.Error(), "Hostname") {
		t.Errorf("formErr = %v, want an error naming Hostname", got)
	}
}

// TestAddFormWithAClearedPortStillSubmits pins the one deliberate exception.
// SSH Port is prefilled with 22, 22 is the only port this estate provisions
// over, and parsePort already answers a cleared port with that same 22 — so a
// cleared port is defaulted, not rejected. This test is where that decision
// lives; if it ever flips to a required field, this is what says so.
func TestAddFormWithAClearedPortStillSubmits(t *testing.T) {
	keyPath := writeStubKey(t)
	m := NewModel(Config{}, stubSource{})
	m.showForm = true
	m.form = addFormWith(t, map[string]string{
		"hostname":     "london",
		"ip":           "10.0.0.5",
		"ssh_port":     "", // cleared by the operator
		"ssh_user":     "ubuntu",
		"key_path":     keyPath,
		"ssh_host_key": stubHostKey,
	})

	u, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("a cleared SSH Port blocked the submit; it is prefilled and defaulted, not required")
	}
	if got := u.(Model).formErr; got != nil {
		t.Errorf("formErr = %v, want nil for a cleared port", got)
	}
	if msg, ok := cmd().(addNodeResultMsg); !ok || msg.err != nil {
		t.Fatalf("submit did not reach addNode: %#v", msg)
	}
}

// wrapsToPort22 is 2³²+22: the value that made the old Atoi-then-int32(n) parse
// dangerous rather than merely wrong. On a 64-bit build Atoi accepts it and the
// cast truncates it to exactly 22 — a plausible port the operator never typed.
const wrapsToPort22 = "4294967318"

// wrapsNegative is 2³¹+22, the other half of the same truncation: it arrives on
// the wire as -2147483626.
const wrapsNegative = "2147483670"

// TestParsePortRefusesValuesThatWouldTruncate is the direct proof of the fix. A
// port field is carried as an int32 all the way to the wire, so a parse that
// keeps a platform-width int and casts does not reject an oversized number, it
// SILENTLY BECOMES A DIFFERENT PORT. 4294967318 becoming 22 is the case that
// matters: nothing downstream can tell it apart from an operator who typed 22,
// so a node gets provisioned over a port no one chose and the estate's record of
// how to reach it is wrong while looking confirmed.
func TestParsePortRefusesValuesThatWouldTruncate(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    int32
		wantErr bool
	}{
		{in: "", want: 22},           // cleared: the documented default
		{in: "   ", want: 22},        // whitespace is a cleared field, not a port
		{in: "22", want: 22},         //
		{in: "2222", want: 2222},     // a real non-default port
		{in: "65535", want: 65535},   // the top of the range is legal
		{in: "0", wantErr: true},     // not dialable
		{in: "-1", wantErr: true},    //
		{in: "65536", wantErr: true}, // in int32 range, still not a port
		{in: "22a", wantErr: true},   // a stray keystroke
		{in: "2147483647", wantErr: true},
		{in: wrapsNegative, wantErr: true},
		{in: wrapsToPort22, wantErr: true},
		{in: "99999999999999999999", wantErr: true},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parsePort(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parsePort(%q) = %d, nil — an out-of-range port must be refused, "+
						"not truncated into a different, valid-looking port", tc.in, got)
				}
				if got != 0 {
					t.Errorf("parsePort(%q) returned port %d alongside its error; a rejected "+
						"port must not also be usable", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePort(%q) = _, %v, want %d", tc.in, err, tc.want)
			}
			if got != tc.want {
				t.Errorf("parsePort(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestAddFormRefusesAnOutOfRangePortWithoutCallingTheServer is the same argument
// at the level the operator lives at. The rejection has to happen on the UI
// thread with the form still open — an AddNode issued with a truncated port
// provisions the node, and no later error undoes a joined host.
func TestAddFormRefusesAnOutOfRangePortWithoutCallingTheServer(t *testing.T) {
	keyPath := writeStubKey(t)
	m := NewModel(Config{}, stubSource{})
	m.showForm = true
	m.form = addFormWith(t, map[string]string{
		"hostname":     "london",
		"ip":           "10.0.0.5",
		"ssh_port":     wrapsToPort22,
		"ssh_user":     "ubuntu",
		"key_path":     keyPath,
		"ssh_host_key": stubHostKey,
	})

	u, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m2 := u.(Model)
	if cmd != nil {
		t.Fatal("a port of " + wrapsToPort22 + " was submitted; it truncates to 22 on the wire, so " +
			"the node would join over a port the operator never typed")
	}
	if m2.formErr == nil {
		t.Fatal("the rejected port produced no error; enter would look like it did nothing")
	}
	if !strings.Contains(m2.formErr.Error(), "SSH Port") {
		t.Errorf("formErr %q does not name SSH Port; the operator cannot tell which row to fix",
			m2.formErr)
	}
	if !m2.showForm {
		t.Error("the rejected submit closed the form; everything else typed would have to be retyped")
	}
	if !strings.Contains(m2.render(), m2.formErr.Error()) {
		t.Errorf("the rejection never rendered; frame was:\n%s", m2.render())
	}
}

// TestTestConnectionRefusesAnOutOfRangePortWithoutProbing covers the other call
// site of the same parse. It asserts two things at once: the probe never reaches
// the server (budgets stays empty — testConnection records itself), and the
// command still answers, because updateForm has already set probing and only a
// testConnResultMsg clears it. A nil command here would wedge the form.
func TestTestConnectionRefusesAnOutOfRangePortWithoutProbing(t *testing.T) {
	budgets := map[string]time.Duration{}
	m := NewModel(Config{}, stubSource{budgets: &budgets})
	m.showForm = true
	m.form = addFormWith(t, map[string]string{"ip": "10.0.0.5", "ssh_port": wrapsNegative})

	u, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlT})
	m = u.(Model) // the post-keypress Model: probing is already set on it
	if cmd == nil {
		t.Fatal("ctrl+t returned no command with a bad port, but probing is already set — the form " +
			"would refuse every further probe for the rest of the session")
	}
	if !m.probing {
		t.Fatal("ctrl+t did not set the in-flight state; this test's premise no longer holds")
	}
	msg, ok := cmd().(testConnResultMsg)
	if !ok {
		t.Fatalf("ctrl+t returned %T, want testConnResultMsg", msg)
	}
	if msg.err == nil {
		t.Fatal("the bad port probed anyway; " + wrapsNegative + " reaches the wire as a negative port")
	}
	if _, probed := budgets["testConnection"]; probed {
		t.Error("the probe Job was spawned for a port that cannot be dialled")
	}

	// The refusal must release the in-flight lock and reach the screen.
	next, _ := m.Update(msg)
	m2 := next.(Model)
	if m2.probing {
		t.Error("the refusal left the form in-flight; no further probe would ever be accepted")
	}
	if !strings.Contains(m2.render(), "SSH Port") {
		t.Errorf("the refusal never named SSH Port on screen; frame was:\n%s", m2.render())
	}
}

// TestAFullyPopulatedAddFormReachesTheServer is the guard on the validation
// itself. The live PTY proof (TestAddNodeProbesThenJoinsTheNodeLive) fills
// exactly these fields and expects a real join; validation that rejected them
// would break a proof that needs a cluster to notice.
func TestAFullyPopulatedAddFormReachesTheServer(t *testing.T) {
	keyPath := writeStubKey(t)
	m := NewModel(Config{}, stubSource{})
	m.showForm = true
	m.form = addFormWith(t, map[string]string{
		"hostname":     "e2e-node2",
		"ip":           "10.0.0.5",
		"ssh_user":     "ubuntu",
		"key_path":     keyPath,
		"ssh_host_key": stubHostKey,
	})

	u, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("a fully populated form was rejected — validation must not block the happy path")
	}
	if got := u.(Model).formErr; got != nil {
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
// left on the Model would render underneath a form that is now correct.
func TestAFixedFormClearsThePriorRejection(t *testing.T) {
	keyPath := writeStubKey(t)
	m := NewModel(Config{}, stubSource{})
	m.showForm = true
	m.form = newAddForm()

	u, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = u.(Model)
	if m.formErr == nil {
		t.Fatal("the empty submit was not rejected")
	}

	m.form = addFormWith(t, map[string]string{
		"hostname":     "london",
		"ip":           "10.0.0.5",
		"ssh_user":     "ubuntu",
		"key_path":     keyPath,
		"ssh_host_key": stubHostKey,
	})
	u2, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("the corrected form was rejected")
	}
	if got := u2.(Model).formErr; got != nil {
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
	m := NewModel(Config{}, stubSource{msg: fetchMsg{nodes: rows}})
	u, _ := m.Update(fetchMsg{nodes: rows})
	m = u.(Model)

	u2, cmd := m.Update(nodeActionMsg{err: errStub{}})
	m2 := u2.(Model)
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
	m := NewModel(Config{}, stubSource{})
	m.nodes = []nodeRow{{Name: "n1", schedulable: true}}
	m.actionErr = errStub{}

	u, _ := m.Update(nodeActionMsg{err: nil})
	if got := u.(Model).actionErr; got != nil {
		t.Errorf("actionErr = %v after a successful action; a stale error reports a failure "+
			"that did not happen", got)
	}
}

// stubHostKey is a syntactically valid authorized_keys line for the Host Key
// field. The TUI does not parse it — the provisioner does, at dial time — so a
// well-formed placeholder is enough here, and using one keeps these tests about
// form validation rather than key formats.
const stubHostKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExampleExampleExampleExampleExampleXX"

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
