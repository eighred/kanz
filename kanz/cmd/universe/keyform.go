package main

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// keyField is one labelled input; masked fields render as bullets so a shared screen
// never shows the secret.
type keyField struct {
	key, label, value string
	masked            bool
}

// keyForm is the venue key-entry form: scoped to one venue (chosen from the pane), it
// collects api_key, api_secret, and — for OKX — a passphrase. Pure state; render() is
// pure so keyform_test drives it with no TTY.
type keyForm struct {
	venue   string
	fields  []keyField
	focused int
}

func newKeyForm(venue string) keyForm {
	fields := []keyField{
		{key: "api_key", label: "API Key"},
		{key: "api_secret", label: "API Secret", masked: true},
	}
	if venue == "okx" {
		fields = append(fields, keyField{key: "passphrase", label: "Passphrase", masked: true})
	}
	return keyForm{venue: venue, fields: fields}
}

func (f keyForm) value(key string) string {
	for _, fl := range f.fields {
		if fl.key == key {
			return fl.value
		}
	}
	return ""
}

func (f keyForm) key(msg tea.KeyMsg) keyForm {
	if msg.Type == tea.KeyRunes {
		f.fields[f.focused].value += string(msg.Runes)
	}
	return f
}

func (f keyForm) backspace() keyForm {
	v := f.fields[f.focused].value
	if v != "" {
		r := []rune(v)
		f.fields[f.focused].value = string(r[:len(r)-1])
	}
	return f
}

func (f keyForm) next() keyForm { f.focused = (f.focused + 1) % len(f.fields); return f }
func (f keyForm) prev() keyForm {
	f.focused = (f.focused - 1 + len(f.fields)) % len(f.fields)
	return f
}

func (f keyForm) render() string {
	var b strings.Builder
	b.WriteString(styleTitle.Render("Set API Keys — "+f.venue) + "\n\n")
	for i, fl := range f.fields {
		shown := fl.value
		if fl.masked {
			shown = strings.Repeat("•", len([]rune(fl.value)))
		}
		label := fmt.Sprintf("%-12s", fl.label+":")
		line := fmt.Sprintf("  %s %s", label, shown)
		if i == f.focused {
			line = styleFocused.Render("▸ "+label) + " " + shown + "_"
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("\n" + styleDim.Render("[tab] next  [enter] save  [esc] cancel"))
	return b.String()
}
