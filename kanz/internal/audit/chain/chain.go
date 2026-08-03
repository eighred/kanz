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

// Verifier walks a chain INCREMENTALLY, one link at a time, holding only the
// previous hash and a counter.
//
// Verification is inherently whole-chain — a suffix cannot be verified without
// a trusted anchor for what precedes it — but it does not need the whole chain
// IN MEMORY. It is a left fold over a stream. Push exists so a caller reading
// from a database can verify as rows arrive instead of materialising every
// record first, which is what /v1/audit/verify used to do to the entire
// compliance log (#229).
//
// The zero Verifier is ready: it chains from Genesis.
type Verifier struct {
	prev  string
	count int
	// failed records the first break so Push stays cheap afterwards and the
	// caller gets the same "index of first failure" answer Verify gives.
	failed *VerifyError
}

// NewVerifier returns a Verifier anchored at Genesis.
func NewVerifier() *Verifier { return &Verifier{} }

// Push folds the next link in sequence. It returns the error at the FIRST break
// and nil for every link after it — the chain is already known broken, and the
// index of the first failure is the diagnostic that matters. A caller may stop
// on the first error or keep streaming to learn the total length; both give the
// same Result.
func (v *Verifier) Push(l Link) error {
	if v.prev == "" {
		v.prev = Genesis
	}
	i := v.count
	v.count++
	if v.failed != nil {
		return nil
	}
	if l.PrevHash() != v.prev {
		v.failed = &VerifyError{Index: i, Kind: "prev-hash mismatch", Want: v.prev, Got: l.PrevHash()}
		return v.failed
	}
	want := Next(v.prev, l.Canonical())
	if l.Hash() != want {
		v.failed = &VerifyError{Index: i, Kind: "content hash mismatch", Want: want, Got: l.Hash()}
		return v.failed
	}
	v.prev = l.Hash()
	return nil
}

// Count is how many links have been pushed.
func (v *Verifier) Count() int { return v.count }

// Result reports the index of the first link that failed the chain (with a
// non-nil error), or -1 and nil if everything pushed so far is intact.
func (v *Verifier) Result() (int, error) {
	if v.failed != nil {
		return v.failed.Index, v.failed
	}
	return -1, nil
}

// Verify walks links in sequence and returns the index of the first record that
// fails the chain (with a non-nil error), or -1 and nil if the chain is intact.
// The first link must chain from Genesis. A failure means the log was tampered
// with at or before that index — recomputing from a forged record diverges here.
//
// It is the slice-shaped convenience over Verifier, not a second implementation:
// the walk lives in Push and nowhere else, so a fix to one cannot miss the
// other.
func Verify(links []Link) (int, error) {
	v := NewVerifier()
	for _, l := range links {
		if err := v.Push(l); err != nil {
			return v.Result()
		}
	}
	return v.Result()
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
