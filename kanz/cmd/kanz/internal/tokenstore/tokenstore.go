// Package tokenstore persists the CLI's Eighred SSO token between runs so a
// user logs in once, not every session. The token is written to the user's
// config directory with 0600 permissions — it is a bearer credential.
package tokenstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/kanz-eng/kanz/pkg/deviceauth"
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
func (s *Store) Load() (*deviceauth.Token, error) {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("tokenstore: read: %w", err)
	}
	var tok deviceauth.Token
	if err := json.Unmarshal(b, &tok); err != nil {
		// A corrupt token file is treated as "not logged in": the CLI will
		// re-authenticate rather than wedge on unreadable state.
		return nil, nil
	}
	return &tok, nil
}

// Save writes the token atomically (write-temp-then-rename) with 0600
// permissions, creating the parent directory if needed.
func (s *Store) Save(tok *deviceauth.Token) error {
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

// Valid reports whether tok is present and its access token is not expired
// (with a small skew allowance, so a token about to expire is refreshed rather
// than used for a request that would then 401).
func Valid(tok *deviceauth.Token, now time.Time) bool {
	if tok == nil || tok.AccessToken == "" {
		return false
	}
	return tok.Expiry.IsZero() || now.Add(30*time.Second).Before(tok.Expiry)
}
