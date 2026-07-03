// Package chain provides the tamper-evidence primitive for the audit log
// (AUDIT-01b): a hash chain where each record's hash folds in the previous
// record's hash, so altering, reordering, or deleting any record invalidates
// every hash after it. Verification is then a linear walk.
//
// The package is deliberately payload-agnostic — it knows nothing about audit
// Records. It hashes over a caller-supplied canonical byte string, so the audit
// store depends on chain, not the reverse (no import cycle). WORM / object-lock
// storage is the OTHER half of AUDIT-01b: the chain proves tampering happened,
// immutable storage prevents the silent rewrite that would also recompute the
// chain. See the package README.
package chain

import (
	"crypto/sha256"
	"encoding/hex"
)

// Genesis is the predecessor hash of the very first record — a fixed, known
// anchor so the head of an empty log is unambiguous and the first hash is
// reproducible.
const Genesis = "genesis"

// Next computes a record's hash from its predecessor's hash and its own
// canonical content: H(prevHash || canonical). Folding prevHash in is what
// binds the record to its position — change anything earlier and this hash, and
// every later one, no longer matches.
func Next(prevHash string, canonical []byte) string {
	h := sha256.New()
	h.Write([]byte(prevHash))
	h.Write([]byte{0}) // domain separator: prevHash and canonical can't run together
	h.Write(canonical)
	return hex.EncodeToString(h.Sum(nil))
}

// Link is one record's view as the chain sees it: its stored predecessor hash,
// its stored hash, and its canonical content. The audit Record implements this.
type Link interface {
	PrevHash() string
	Hash() string
	Canonical() []byte
}

// Verify walks links in sequence and returns the index of the first record that
// fails the chain (with a non-nil error), or -1 and nil if the chain is intact.
// The first link must chain from Genesis. A failure means the log was tampered
// with at or before that index — recomputing from a forged record diverges here.
func Verify(links []Link) (int, error) {
	prev := Genesis
	for i, l := range links {
		if l.PrevHash() != prev {
			return i, &VerifyError{Index: i, Kind: "prev-hash mismatch", Want: prev, Got: l.PrevHash()}
		}
		want := Next(prev, l.Canonical())
		if l.Hash() != want {
			return i, &VerifyError{Index: i, Kind: "content hash mismatch", Want: want, Got: l.Hash()}
		}
		prev = l.Hash()
	}
	return -1, nil
}

// VerifyError pinpoints where and how the chain broke.
type VerifyError struct {
	Index     int
	Kind      string
	Want, Got string
}

func (e *VerifyError) Error() string {
	return "chain: " + e.Kind + " at index " + itoa(e.Index) + " (want " + short(e.Want) + ", got " + short(e.Got) + ")"
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12] + "…"
	}
	return s
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		p--
		b[p] = '-'
	}
	return string(b[p:])
}
