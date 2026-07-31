package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/cmd/kanz/internal/config"
	"github.com/eighred/kanz/cmd/kanz/internal/tokenstore"
	"github.com/eighred/kanz/pkg/deviceauth"
)

// TABBING TO THE ESTATE BEFORE SIGNING IN MUST NOT PANIC.
//
// It did. tokenstore.Load returns (nil, nil) when there is no token file —
// "not signed in" is deliberately not an error, so the CLI re-authenticates
// rather than wedging — and the first version of this builder checked only the
// error before reading tok.AccessToken. The common case, a fresh operator
// pressing Tab once, dereferenced nil and took the whole shell down.
//
// The panic surfaced at estate.go's ensure(), which is only where the closure is
// CALLED. Guarding there would have hidden the nil and left the estate silently
// unconnected — the fix belongs at the read, which is here.
func TestEstateBuilderRefusesAnAbsentSessionInsteadOfPanicking(t *testing.T) {
	store := tokenstore.NewAt(filepath.Join(t.TempDir(), "no-such-token.json"))
	build := newEstateBuilder(config.Config{GatewayURL: "https://gw.invalid"}, store)

	// A panic here fails the test by crashing it, which is the point: this is a
	// regression test for a nil dereference, so "returns an error" and "does not
	// panic" are the same assertion made two ways.
	_, err := build()
	if err == nil {
		t.Fatal("building the estate with no saved session returned no error — the operator would " +
			"get an unauthenticated source instead of being told to sign in")
	}
	if !strings.Contains(err.Error(), "/login") {
		t.Errorf("error = %q, want it to name the fix (/login on the Copilot pane) — the estate pane "+
			"renders this verbatim, and a message that does not say what to do is a blank screen with "+
			"extra words", err)
	}
}

// A CORRUPT TOKEN FILE TAKES THE SAME PATH. tokenstore.Load returns (nil, nil)
// for unparseable JSON too, so this is the same nil, arrived at differently —
// and it is the case least likely to be tried by hand.
func TestEstateBuilderRefusesACorruptTokenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	build := newEstateBuilder(config.Config{GatewayURL: "https://gw.invalid"}, tokenstore.NewAt(path))

	if _, err := build(); err == nil {
		t.Fatal("a corrupt token file built an estate source — Load returns (nil, nil) for it, so this " +
			"is the same nil dereference by another route")
	}
}

// AN EXPIRED SESSION IS REFUSED HERE, not at the gateway. Without this the token
// reaches the api-gateway and comes back 401, which tells the operator far less
// than "your session expired".
func TestEstateBuilderRefusesAnExpiredSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token.json")
	store := tokenstore.NewAt(path)
	if err := store.Save(&deviceauth.Token{
		AccessToken: "stale",
		Expiry:      time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	build := newEstateBuilder(config.Config{GatewayURL: "https://gw.invalid"}, store)

	if _, err := build(); err == nil {
		t.Fatal("an expired token built an estate source — the operator would get a 401 from the " +
			"gateway instead of being told to sign in again")
	}
}

// The counterpart: a valid session BUILDS. A fix that refused everything would
// pass all three tests above and leave the estate permanently unreachable.
func TestEstateBuilderSucceedsWithAValidSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token.json")
	store := tokenstore.NewAt(path)
	if err := store.Save(&deviceauth.Token{
		AccessToken: "good",
		Expiry:      time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	build := newEstateBuilder(config.Config{GatewayURL: "https://gw.invalid"}, store)

	if _, err := build(); err != nil {
		t.Fatalf("a valid session failed to build the estate: %v", err)
	}
}
