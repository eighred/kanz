// Package tokenstore persists the CLI's session token between runs so an
// operator signs in once, not every session. The token is written to the user's
// config directory with 0600 permissions — it is a bearer credential.
//
// THE PERSISTED SHAPE CHANGED WITH THE IDENTITY PROVIDER (#364), AND OLD FILES
// ARE HANDLED BY AN EXISTING RULE RATHER THAN A MIGRATION. It used to hold a
// deviceauth.Token (AccessToken/Expiry, from an SSO device flow that was never
// built); it now holds an identityclient.Token (token/expires_at/subject/
// tenant). A file in the old shape decodes to a Token with an empty token
// string, which Load already treats as "not logged in" — so the operator signs
// in again and the file is overwritten. Writing a converter for a credential
// nothing could have issued would be code that exists to migrate an empty set.
package tokenstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/eighred/kanz/internal/identityclient"
)

// Store reads and writes the persisted token at a fixed path.
type Store struct{ path string }

// New returns a Store writing to {dir}/kanz/token.json, where dir is the
// user's OS config directory (os.UserConfigDir). The parent is created lazily
// on Save.
func New() (*Store, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return nil, fmt.Errorf("tokenstore: locate config dir: %w", err)
	}
	return &Store{path: filepath.Join(dir, "kanz", "token.json")}, nil
}

// NewAt returns a Store writing to an explicit path (used in tests).
func NewAt(path string) *Store { return &Store{path: path} }

// Path is the file the token is persisted to.
func (s *Store) Path() string { return s.path }

// Load returns the persisted token. A missing file is reported as
// (nil, nil) — "not logged in", not an error — so callers can branch on it
// without inspecting the filesystem error.
func (s *Store) Load() (*identityclient.Token, error) {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("tokenstore: read: %w", err)
	}
	var tok identityclient.Token
	if err := json.Unmarshal(b, &tok); err != nil {
		// A corrupt token file is treated as "not logged in": the CLI will
		// re-authenticate rather than wedge on unreadable state.
		return nil, nil
	}
	return &tok, nil
}

// Save writes the token atomically (write-temp-then-rename) with 0600
// permissions, creating the parent directory if needed.
func (s *Store) Save(tok *identityclient.Token) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("tokenstore: mkdir: %w", err)
	}
	b, err := json.MarshalIndent(tok, "", "  ")
	if err != nil {
		return fmt.Errorf("tokenstore: marshal: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("tokenstore: write: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("tokenstore: rename: %w", err)
	}
	return nil
}

// Delete removes the persisted token (logout). A missing file is not an error.
func (s *Store) Delete() error {
	if err := os.Remove(s.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("tokenstore: delete: %w", err)
	}
	return nil
}

// Valid reports whether tok is present and not expired (with a small skew
// allowance, so a token about to expire sends the caller to sign in again
// rather than being spent on a request that would then 401).
//
// A ZERO EXPIRY MEANS NON-EXPIRING, AND ONLY ONE THING PRODUCES IT: a session
// adopted from KANZ_TOKEN, where the gateway is the authority on the bearer's
// lifetime and inventing one here would refuse a perfectly good token. Every
// token the identity provider mints carries a real expires_at (its TTL defaults
// to 8h and there is NO refresh route), so a signed-in session always takes the
// second branch.
func Valid(tok *identityclient.Token, now time.Time) bool {
	if tok == nil || tok.Token == "" {
		return false
	}
	return tok.Expires.IsZero() || now.Add(30*time.Second).Before(tok.Expires)
}
