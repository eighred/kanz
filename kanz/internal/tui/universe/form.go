package universe

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// addField is one labelled single-line input. The Key Path field is a FILE PATH,
// not the key itself — the PEM is read client-side at submit and never displayed.
// required marks a field submit refuses to proceed without; it lives beside the
// field rather than in a separate table so there is exactly one place to read
// what the form demands.
type addField struct {
	key      string
	label    string
	value    string
	required bool
}

// addForm is a minimal hand-rolled form: a fixed list of
// single-line text fields with a focused index. Pure state — render() is a pure
// function so form_test can drive it with no TTY.
type addForm struct {
	fields  []addField
	focused int
}

// newAddForm builds the empty Add Node form. Every field is required EXCEPT
// SSH Port: it is prefilled with 22, 22 is the only port this estate probes or
// provisions over, and parsePort already answers a cleared port with that same
// 22. Demanding it back would make the operator retype the only value the field
// can hold, and would put a second, contradicting answer next to parsePort's.
// The port is therefore DEFAULTED, not required — stated here so the choice is
// readable rather than inferred from an absent flag.
//
// Defaulted is not the same as unchecked. A port that is present but not a port
// (out of 1..65535, or wide enough to truncate on the way to the int32 wire
// field) is REFUSED by parsePort rather than quietly replaced with 22 — the
// operator typed something, and turning it into a different port they never
// asked for is the one answer worse than an error.
func newAddForm() addForm {
	return addForm{fields: []addField{
		{key: "hostname", label: "Hostname", required: true},
		{key: "ip", label: "IP", required: true},
		{key: "ssh_port", label: "SSH Port", value: "22"},
		{key: "ssh_user", label: "User", required: true},
		{key: "key_path", label: "Key Path", required: true},
		// The TARGET's PUBLIC host key, which is the opposite direction from Key
		// Path above: that one proves us to the host, this one proves the host to
		// us. Required, because the join session pipes K3S_TOKEN — cluster
		// admission — into a shell on the far end, and without this there is
		// nothing distinguishing the machine the operator meant from whatever
		// answered the dial.
		//
		// Whoever put the bootstrap key's public half into the target's
		// authorized_keys can read this off the same box:
		//   ssh-keyscan -t ed25519 <ip>   (drop the leading host field)
		// or cat /etc/ssh/ssh_host_ed25519_key.pub on the target itself.
		{key: "ssh_host_key", label: "Host Key", required: true},
	}}
}

// missingRequired returns the LABELS of every required field left empty, in the
// order they appear on screen. Labels rather than keys because the label is what
// the operator is looking at: "Key Path" points at a row, "key_path" asks them
// to translate. Whitespace-only counts as empty — a hostname of " " is not a
// hostname, and the server would only reject it after a round trip.
func (f addForm) missingRequired() []string {
	var missing []string
	for _, fl := range f.fields {
		if fl.required && strings.TrimSpace(fl.value) == "" {
			missing = append(missing, fl.label)
		}
	}
	return missing
}

func (f addForm) value(key string) string {
	for _, fl := range f.fields {
		if fl.key == key {
			return fl.value
		}
	}
	return ""
}

func (f addForm) key(msg tea.KeyMsg) addForm {
	if msg.Type == tea.KeyRunes {
		f.fields[f.focused].value += string(msg.Runes)
	}
	return f
}

func (f addForm) backspace() addForm {
	v := f.fields[f.focused].value
	if v != "" {
		f.fields[f.focused].value = v[:len(v)-1]
	}
	return f
}

func (f addForm) next() addForm {
	f.focused = (f.focused + 1) % len(f.fields)
	return f
}

func (f addForm) prev() addForm {
	f.focused = (f.focused - 1 + len(f.fields)) % len(f.fields)
	return f
}

var styleFocused = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))

func (f addForm) render() string {
	var b strings.Builder
	b.WriteString(styleTitle.Render("Add Node") + "\n\n")
	for i, fl := range f.fields {
		marker := "  "
		label := fmt.Sprintf("%-10s", fl.label+":")
		line := fmt.Sprintf("%s%s %s", marker, label, fl.value)
		if i == f.focused {
			line = styleFocused.Render("▸ "+label) + " " + fl.value + "_"
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("\n" + styleDim.Render("[tab] next  [enter] save  [ctrl+t] test  [esc] cancel"))
	return b.String()
}
