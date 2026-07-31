package universe

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestNewKeyFormOKXHasPassphrase(t *testing.T) {
	f := newKeyForm("okx")
	if len(f.fields) != 3 {
		t.Fatalf("okx fields = %d, want 3", len(f.fields))
	}
	if f.fields[2].key != "passphrase" || !f.fields[2].masked {
		t.Fatalf("okx third field = %+v, want masked passphrase", f.fields[2])
	}
}

func TestNewKeyFormBinanceHasNoPassphrase(t *testing.T) {
	f := newKeyForm("binance")
	if len(f.fields) != 2 {
		t.Fatalf("binance fields = %d, want 2", len(f.fields))
	}
	for _, fl := range f.fields {
		if fl.key == "passphrase" {
			t.Fatalf("binance form should not have a passphrase field")
		}
	}
}

func TestKeyFormRendersSecretMaskedNotRaw(t *testing.T) {
	f := newKeyForm("binance")
	f.fields[1].value = "supersecretvalue123"
	out := f.render()
	if strings.Contains(out, "supersecretvalue123") {
		t.Fatalf("render must never show the raw secret\n%s", out)
	}
	if !strings.Contains(out, strings.Repeat("•", len("supersecretvalue123"))) {
		t.Fatalf("render should show the secret masked with bullets\n%s", out)
	}
}

func TestKeyFormRenderIsPure(t *testing.T) {
	f := newKeyForm("okx")
	f.fields[0].value = "abc"
	f.fields[1].value = "def"
	out1 := f.render()
	out2 := f.render()
	if out1 != out2 {
		t.Fatalf("render is not pure: two calls produced different output\nfirst:\n%s\nsecond:\n%s", out1, out2)
	}
}

func TestKeyFormTypingEditsFocusedField(t *testing.T) {
	f := newKeyForm("binance")
	for _, r := range "hi" {
		f = f.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	if f.value("api_key") != "hi" {
		t.Fatalf("api_key = %q, want hi", f.value("api_key"))
	}
}

func TestKeyFormBackspaceIsRuneSafe(t *testing.T) {
	f := newKeyForm("binance")
	for _, r := range "héllo" {
		f = f.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	f = f.backspace()
	if f.value("api_key") != "héll" {
		t.Fatalf("api_key after backspace = %q, want héll", f.value("api_key"))
	}
}

func TestKeyFormNextPrevCycleFields(t *testing.T) {
	f := newKeyForm("okx")
	if f.focused != 0 {
		t.Fatalf("initial focused = %d, want 0", f.focused)
	}
	f = f.next()
	if f.focused != 1 {
		t.Fatalf("after next, focused = %d, want 1", f.focused)
	}
	f = f.prev().prev()
	if f.focused != 2 {
		t.Fatalf("after prev wrap, focused = %d, want 2", f.focused)
	}
}
