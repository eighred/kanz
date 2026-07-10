package tokenstore

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kanz-eng/kanz/pkg/deviceauth"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "token.json")
	s := NewAt(path)

	want := &deviceauth.Token{
		AccessToken:  "acc",
		RefreshToken: "ref",
		IDToken:      "id",
		TokenType:    "Bearer",
		Expiry:       time.Now().Add(time.Hour).Truncate(time.Second),
	}
	if err := s.Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// The parent directory was created; the file is 0600.
	if fi, err := os.Stat(path); err != nil {
		t.Fatalf("stat: %v", err)
	} else if perm := fi.Mode().Perm(); perm != 0o600 && perm != 0o666 {
		// Windows reports 0666; POSIX honors 0600. Accept either.
		t.Fatalf("perm = %o", perm)
	}

	got, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got == nil || got.AccessToken != "acc" || got.RefreshToken != "ref" || !got.Expiry.Equal(want.Expiry) {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

func TestLoadMissingIsNotAnError(t *testing.T) {
	s := NewAt(filepath.Join(t.TempDir(), "absent.json"))
	got, err := s.Load()
	if err != nil || got != nil {
		t.Fatalf("Load(missing) = (%v, %v), want (nil, nil)", got, err)
	}
}

func TestLoadCorruptIsTreatedAsLoggedOut(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token.json")
	if err := os.WriteFile(path, []byte("}{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := NewAt(path).Load()
	if err != nil || got != nil {
		t.Fatalf("Load(corrupt) = (%v, %v), want (nil, nil)", got, err)
	}
}

func TestDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token.json")
	s := NewAt(path)
	if err := s.Save(&deviceauth.Token{AccessToken: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("file still present after Delete")
	}
	// Deleting an already-absent file is not an error.
	if err := s.Delete(); err != nil {
		t.Fatalf("Delete(absent): %v", err)
	}
}

func TestValid(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		tok  *deviceauth.Token
		want bool
	}{
		{"nil", nil, false},
		{"empty access", &deviceauth.Token{}, false},
		{"no expiry", &deviceauth.Token{AccessToken: "x"}, true},
		{"future", &deviceauth.Token{AccessToken: "x", Expiry: now.Add(time.Hour)}, true},
		{"expired", &deviceauth.Token{AccessToken: "x", Expiry: now.Add(-time.Hour)}, false},
		{"within skew", &deviceauth.Token{AccessToken: "x", Expiry: now.Add(10 * time.Second)}, false},
	}
	for _, c := range cases {
		if got := Valid(c.tok, now); got != c.want {
			t.Errorf("%s: Valid = %v, want %v", c.name, got, c.want)
		}
	}
}
