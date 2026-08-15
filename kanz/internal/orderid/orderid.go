// Package orderid owns what an order id may look like, and how to mint one.
//
// # An order id is a VENUE id, not just ours
//
// The connectors stamp it directly as the exchange's client order id —
// `newClientOrderId` at Binance, `clOrdId` at OKX — and that is deliberate: a
// deterministic id we choose is what makes a retried placement idempotent at the
// exchange, and what lets an ambiguous timeout be resolved by asking the venue
// what it did with THAT id. The OKX return path goes further and uses the
// clOrdId it receives AS the kanz order id when it builds a fill FACT
// (okx_userdata.go), so the two are not merely related, they are the same
// string.
//
// That makes an order id subject to the intersection of every venue's rules, and
// nothing enforced it.
//
// # What the rule actually is, measured rather than assumed
//
// Probed against OKX's demo API on 2026-08-15, by asking it about ids that do
// not exist and reading which complaint came back — 51603 "Order does not exist"
// means the FORMAT was accepted, 51000 "Parameter clOrdId error" means it was
// not:
//
//	abcDEF123                            51603  accepted
//	aaaa…(32 chars)                      51603  accepted
//	aaaa…(33 chars)                      51000  REJECTED — 32 is the ceiling
//	abc_def                              51000  REJECTED — underscore
//	abc.def                              51000  REJECTED — dot
//	3f9a1c2e-0b7d-4e11-9a6f-2c8d5e4b7a13 51000  REJECTED — hyphen, and 36 long
//
// So: ALPHANUMERIC ONLY, 1 TO 32 CHARACTERS. Binance is looser — it accepts
// `[A-Za-z0-9_.:/-]` up to 36 — so the intersection is OKX's rule, and this
// package enforces the intersection rather than each venue's own.
//
// # The defect this was written for
//
// api-gateway minted order ids with uuid.NewString(), which is 36 characters
// and hyphenated. EVERY ORDER SUBMITTED THROUGH THE HTTP GATEWAY WAS THEREFORE
// UNPLACEABLE AT OKX — admitted, stored, its ORDER_ACCEPTED FACT published, and
// then refused by the exchange with an opaque parameter error. Binance accepts
// hyphens, so the same order worked there, which is why nothing noticed: the
// platform behaved correctly on one venue and silently could not trade on the
// other.
//
// It could not have been found from inside this repository. No unit test, no
// arch guard and no amount of reading tells you that OKX counts 32 and refuses a
// hyphen; only asking OKX does.
package orderid

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// MaxLen is the longest an order id may be. It is OKX's ceiling, which is the
// tightest of the venues this platform holds.
const MaxLen = 32

// Valid reports whether an id can be placed at every venue this platform holds,
// and says which rule it breaks when it cannot.
//
// THE ERROR NAMES THE RULE AND THE VENUE. "Parameter clOrdId error" — what OKX
// actually returns — sends an operator to an exchange's documentation to find
// out that their UUID has a hyphen in it.
func Valid(id string) error {
	if id == "" {
		return fmt.Errorf("order id is empty")
	}
	if len(id) > MaxLen {
		return fmt.Errorf("order id %q is %d characters; OKX accepts at most %d as a clOrdId, "+
			"and this platform stamps the order id directly as the venue's client order id",
			id, len(id), MaxLen)
	}
	for i, r := range id {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			continue
		}
		return fmt.Errorf("order id %q contains %q at position %d; OKX accepts only letters and "+
			"digits in a clOrdId, and this platform stamps the order id directly as the venue's "+
			"client order id", id, string(r), i)
	}
	return nil
}

// Mint returns a fresh order id: 128 bits of randomness as 32 hex characters.
//
// THE SAME ENTROPY AS A UUID AND THE SAME SHAPE AS translate.DeterministicID,
// which is what the signal fan-out has always produced — so the estate has one
// id shape rather than two, and the one it has is placeable.
//
// It cannot fail: crypto/rand.Read is documented never to return an error, and a
// caller with no id to fall back on has nothing useful to do with one anyway.
func Mint() string {
	var b [MaxLen / 2]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
