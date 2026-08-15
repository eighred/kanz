package orderid

import (
	"strings"
	"testing"
)

// THE RULE HERE WAS MEASURED, NOT ASSUMED (#405 family).
//
// Every case below is a string that was actually sent to OKX's demo API on
// 2026-08-15, with the answer it came back with. `51603 "Order does not exist"`
// means the format was ACCEPTED and the id merely unknown; `51000 "Parameter
// clOrdId error"` means the format was refused. Those two answers are what
// separate a rule from a guess, and they are recorded per-case so a future reader
// can tell which parts of this are evidence and which are inference.

// THE IDS THIS PLATFORM ACTUALLY MINTS ARE PLACEABLE. A rule that refused them
// would be a trading outage wearing a control's shape.
func TestValid_AcceptsWhatTheEstateMints(t *testing.T) {
	for _, tt := range []struct{ name, id string }{
		{"signal fan-out (translate.DeterministicID), 32 hex — probed 51603", "8d1f0c3b9a2e4d5f6071829304a5b6c7"},
		{"api-gateway after the fix, 32 hex — probed 51603", "9d3a45c14832d709db117779a8bfe3a2"},
		{"a scheduled child order id, 32 hex — probed 51603", "e71d0cb26113b44d2a6bca7beac8be7e"},
		{"mixed case and digits — probed 51603", "abcDEF123"},
		{"exactly the ceiling, 32 chars — probed 51603", strings.Repeat("a", 32)},
		{"a single character", "a"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := Valid(tt.id); err != nil {
				t.Fatalf("Valid(%q) = %v, want nil", tt.id, err)
			}
		})
	}
}

// AND THE ONES OKX REFUSES ARE REFUSED HERE FIRST, with the rule named.
//
// The first case is the defect this package exists for: api-gateway minted order
// ids with uuid.NewString(), so every order submitted through the HTTP gateway
// was unplaceable at OKX — admitted, stored, its ORDER_ACCEPTED FACT published,
// then refused by the exchange with a parameter error naming neither the rule nor
// the character. Binance accepts hyphens and 36 characters, so the identical
// order traded normally there, which is why nothing noticed.
func TestValid_RefusesWhatOKXRefuses(t *testing.T) {
	for _, tt := range []struct{ name, id, wantIn string }{
		{
			"a hyphenated UUID, as api-gateway used to mint — probed 51000",
			"3f9a1c2e-0b7d-4e11-9a6f-2c8d5e4b7a13", "-",
		},
		{"33 characters, one over the ceiling — probed 51000", strings.Repeat("a", 33), "32"},
		{"an underscore — probed 51000", "kanz_order_1", "_"},
		{"a dot — probed 51000", "kanz.order.1", "."},
		{
			"the old scheduled-child form, which broke BOTH rules at once — probed 51000",
			"3f9a1c2e0b7d4e119a6f2c8d5e4b7a13:0", ":",
		},
		{"empty", "", "empty"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := Valid(tt.id)
			if err == nil {
				t.Fatalf("Valid(%q) = nil, but OKX answers 51000 for it — the order would be "+
					"admitted, stored and announced, then refused by the exchange", tt.id)
			}
			// THE MESSAGE MUST NAME WHAT TO CHANGE. "Parameter clOrdId error" is
			// what OKX says, and it does not tell an operator which of their 36
			// characters was the problem.
			if !strings.Contains(err.Error(), tt.wantIn) {
				t.Errorf("error = %q, want it to name %q", err, tt.wantIn)
			}
		})
	}
}

// MINT PRODUCES A PLACEABLE ID, EVERY TIME. The one property the rest of the
// platform depends on without checking.
func TestMint_IsAlwaysPlaceable(t *testing.T) {
	seen := make(map[string]bool, 1000)
	for i := range 1000 {
		id := Mint()
		if err := Valid(id); err != nil {
			t.Fatalf("Mint() produced %q, which is not placeable: %v", id, err)
		}
		if len(id) != MaxLen {
			t.Fatalf("Mint() produced %d characters, want %d — the full ceiling is used so the "+
				"entropy matches the UUID it replaces", len(id), MaxLen)
		}
		if seen[id] {
			t.Fatalf("Mint() repeated %q within %d draws — an order id collision is two orders "+
				"merged at the store's primary key and at the venue's", id, i+1)
		}
		seen[id] = true
	}
}
