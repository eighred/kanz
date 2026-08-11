package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/cmd/kanz/internal/config"
	"github.com/eighred/kanz/cmd/kanz/internal/tokenstore"
	"github.com/eighred/kanz/internal/identityclient"
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
	if err := store.Save(&identityclient.Token{
		Token:   "stale",
		Expires: time.Now().Add(-time.Hour),
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
	if err := store.Save(&identityclient.Token{
		Token:   "good",
		Expires: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	build := newEstateBuilder(config.Config{GatewayURL: "https://gw.invalid"}, store)

	if _, err := build(); err != nil {
		t.Fatalf("a valid session failed to build the estate: %v", err)
	}
}

// ONE SHELL, ONE ANSWER ABOUT WHETHER THERE IS A SESSION.
//
// The REPL adopts KANZ_TOKEN and deliberately never persists it, so a
// store-only lookup here would leave the estate reporting "not signed in" while
// the Copilot pane beside it answered questions — the shell disagreeing with
// itself, which is worse than either behaviour alone.
func TestEstateBuilderUsesTheDevTokenWhenThereIsNoStoredSession(t *testing.T) {
	store := tokenstore.NewAt(filepath.Join(t.TempDir(), "no-such-token.json"))
	build := newEstateBuilder(
		config.Config{GatewayURL: "https://gw.invalid", DevToken: "pre-minted"}, store)

	if _, err := build(); err != nil {
		t.Fatalf("the estate refused to build on a KANZ_TOKEN session: %v — the Copilot pane would "+
			"be working while the estate said 'not signed in'", err)
	}
}

// With neither a dev token nor a stored session it still refuses, and still
// says what to do. The dev-token path must not have removed that.
func TestEstateBuilderStillRefusesWithNoSessionAtAll(t *testing.T) {
	store := tokenstore.NewAt(filepath.Join(t.TempDir(), "no-such-token.json"))
	build := newEstateBuilder(config.Config{GatewayURL: "https://gw.invalid"}, store)

	_, err := build()
	if err == nil {
		t.Fatal("the estate built with no session at all")
	}
	if !strings.Contains(err.Error(), "/login") {
		t.Errorf("error = %q, want it to still name the fix", err)
	}
}

// THE WIRING GAP THE UNIT TESTS COULD NOT SEE.
//
// The SSO client this replaced refused an empty issuer, and a KANZ_TOKEN session
// has no sign-in endpoint by construction. Building it unconditionally meant the
// dev-token path could not start at all — `kanz: deviceauth: issuer is required`
// before a single frame.
//
// Every REPL test passed: they construct the REPL directly and never run main's
// wiring. Same blind spot that hid the estate builder's nil dereference, which
// is why this is tested here rather than assumed. It survives the move to the
// identity provider unchanged, because the shape of the mistake is unchanged.
func TestAuthenticatorIsNotBuiltWhenThereIsNowhereToSignIn(t *testing.T) {
	auth, err := newAuthenticator(
		config.Config{GatewayURL: "https://gw.invalid", DevToken: "pre-minted"}, io.Discard)
	if err != nil {
		t.Fatalf("newAuthenticator refused a KANZ_TOKEN session: %v — kanz would not start at all", err)
	}
	if auth == nil {
		t.Fatal("returned a nil Authenticator — repl.login would panic instead of refusing")
	}

	// It must never succeed silently: if login ever reaches it, the result is an
	// error naming the cause, not a nil token treated as a session.
	tok, lerr := auth.Login(context.Background(), "user:alice", "correct horse")
	if lerr == nil {
		t.Error("the placeholder authenticator returned success")
	}
	if tok != nil {
		t.Error("the placeholder authenticator returned a token")
	}
}

// The real path: a configured identity URL builds a real client. A fix that
// returned the placeholder for everyone would pass the test above and quietly
// disable sign-in for the whole estate.
func TestAuthenticatorIsBuiltNormallyWhenAnIdentityURLIsConfigured(t *testing.T) {
	auth, err := newAuthenticator(
		config.Config{GatewayURL: "https://gw.invalid", IdentityURL: "https://identity.invalid"},
		io.Discard)
	if err != nil {
		t.Fatalf("newAuthenticator failed for a normal identity configuration: %v", err)
	}
	if _, isPlaceholder := auth.(unavailableAuth); isPlaceholder {
		t.Error("an identity-configured client got the placeholder authenticator — /login would " +
			"refuse on a deployment that has a real identity provider")
	}
}

// THE CREDENTIAL SOURCE IS WIRED, AND ITS ABSENCE IS A REAL FAILURE MODE.
//
// repl.login refuses with "this surface cannot ask for a credential" when the
// source is nil, which is the correct behaviour and a terrible thing to discover
// in production. main passes envCredential{} at both call sites; this asserts
// the value is usable rather than that the call compiles.
func TestEnvCredentialReadsTheEnvironmentAndRefusesWhenUnset(t *testing.T) {
	t.Setenv("KANZ_CREDENTIAL", "")
	if _, err := (envCredential{}).Credential(context.Background(), "user:alice"); err == nil {
		t.Fatal("an unset KANZ_CREDENTIAL produced no error — /login would send an empty secret to " +
			"the identity service and report it as a rejected credential")
	} else if !strings.Contains(err.Error(), "KANZ_CREDENTIAL") {
		t.Errorf("error %q does not name the variable to set", err)
	}

	t.Setenv("KANZ_CREDENTIAL", "correct horse")
	got, err := (envCredential{}).Credential(context.Background(), "user:alice")
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}
	// NOT TRIMMED, deliberately: a credential may legitimately begin or end with
	// a space, and silently "helping" would refuse a correct one with the same
	// message a wrong one gets.
	if got != "correct horse" {
		t.Errorf("Credential = %q, want it returned verbatim", got)
	}
}
