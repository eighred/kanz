// Package secret resolves a sensitive configuration value from a CSI/Vault file
// mount or a plaintext env var, and refuses to guess when the mount is declared
// but unreadable.
//
// IT EXISTS BECAUSE SEVENTEEN COPIES OF IT DID. Every service composition root
// plus cmd/kanz-migrate carried its own `func secret(k string) string`, and
// fifteen of them discarded os.ReadFile's error:
//
//	if p := os.Getenv(k + "_FILE"); p != "" {
//	    if b, err := os.ReadFile(p); err == nil {
//	        return strings.TrimSpace(string(b))
//	    }
//	}
//	return os.Getenv(k)          // ← reached when the mount is UNREADABLE
//
// That fall-through is the defect. Setting <k>_FILE is the deployment's
// statement that a durable secret mount was intended; if the file is missing,
// wrong-permissioned or badly mounted, that is a DEPLOYMENT FAULT, not an
// absent secret. Falling through to the plaintext env and then to "" makes a
// mounted-but-unreadable secret indistinguishable from one nobody configured —
// so a service comes up with an empty DSN, an empty API key, an empty signing
// secret, and reports a clean start.
//
// The two venue adapters had already been fixed and are the shape this package
// generalises. Sixteen copies were not, and copies are how a fix stops
// spreading: venue-binance and venue-okx were correct for weeks while every
// other root stayed wrong, because nothing connected them.
package secret

import (
	"fmt"
	"os"
	"strings"
)

// Read returns the value of k, preferring the file at <k>_FILE over the
// plaintext env var <k> (SEC-01d).
//
// The three outcomes are deliberately distinct:
//
//   - <k>_FILE unset          → the plaintext env var, or "" — no mount was
//     ever claimed, so there is nothing to fail about.
//   - <k>_FILE set, readable  → the file's contents, whitespace-trimmed.
//   - <k>_FILE set, UNREADABLE → an error. Never the env var, never "".
//
// A missing file and a permission error are both deployment faults and neither
// is special-cased: the caller cannot act differently on them, and pretending
// otherwise would invite a "well, missing is probably fine" branch — which is
// the fall-through this package exists to delete.
//
// Read does NOT reject an empty value. A file that exists and is empty resolves
// to "", and whether that is fatal belongs to the caller: some values are
// genuinely optional, and this package cannot tell which. What it guarantees is
// that "" means the value really was absent, not that reading it failed.
func Read(k string) (string, error) {
	p := os.Getenv(k + "_FILE")
	if p == "" {
		return os.Getenv(k), nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", fmt.Errorf("%s_FILE=%q: declared secret mount is unreadable: %w", k, p, err)
	}
	return strings.TrimSpace(string(b)), nil
}
