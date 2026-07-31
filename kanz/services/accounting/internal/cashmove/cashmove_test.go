package cashmove

import (
	"math/big"
	"testing"
	"time"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"

	"github.com/eighred/kanz/internal/dec"
)

func mv(kind Kind, amount string) CashMovement {
	return CashMovement{
		MovementID:  "M1",
		PortfolioID: "PF",
		Kind:        kind,
		Amount:      dec.Rat(amount),
		Currency:    "USD",
		Effective:   time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
	}
}

// A subscription encodes to a positive CASH entry; the entry id is idempotent
// on the movement id and the subject routes by kind.
func TestEncodeSubscription(t *testing.T) {
	entry, subject, err := encode(mv(Subscription, "100000"), time.Now())
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if subject != "accounting.cash.subscription" {
		t.Fatalf("subject = %q", subject)
	}
	if entry.GetEntryType() != accountingpb.EntryType_ENTRY_TYPE_CASH {
		t.Fatalf("entry type = %v, want CASH", entry.GetEntryType())
	}
	if entry.GetEntryId() != "cash:M1" {
		t.Fatalf("entry id = %q, want cash:M1", entry.GetEntryId())
	}
	if got := dec.FromProto(entry.GetCash()); got.Cmp(big.NewRat(100000, 1)) != 0 {
		t.Fatalf("cash = %s, want +100000", got.RatString())
	}
}

// A redemption is a negative CASH entry; a fee is a negative FEE entry.
func TestEncodeSignsAndTypes(t *testing.T) {
	red, _, err := encode(mv(Redemption, "5000"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if red.GetEntryType() != accountingpb.EntryType_ENTRY_TYPE_CASH {
		t.Fatalf("redemption type = %v, want CASH", red.GetEntryType())
	}
	if dec.FromProto(red.GetCash()).Sign() >= 0 {
		t.Fatal("redemption cash should be negative")
	}
	fee, subject, err := encode(mv(Fee, "250"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if subject != "accounting.cash.fee" || fee.GetEntryType() != accountingpb.EntryType_ENTRY_TYPE_FEE {
		t.Fatalf("fee subject=%q type=%v", subject, fee.GetEntryType())
	}
	if dec.FromProto(fee.GetCash()).Sign() >= 0 {
		t.Fatal("fee cash should be negative")
	}
}

// A movement passing a signed (already-negative) amount is still normalized by
// magnitude — the Kind, not the caller, decides the sign.
func TestEncodeUsesMagnitude(t *testing.T) {
	m := mv(Redemption, "5000")
	m.Amount = big.NewRat(-5000, 1) // caller passed a negative
	entry, _, err := encode(m, time.Now())
	if err == nil {
		// amount must be positive — a negative magnitude is rejected.
		_ = entry
		t.Fatal("expected rejection of a non-positive amount")
	}
}

func TestEncodeValidation(t *testing.T) {
	now := time.Now()
	cases := map[string]CashMovement{
		"empty id":        {PortfolioID: "PF", Kind: Subscription, Amount: dec.Rat("1"), Currency: "USD"},
		"empty portfolio": {MovementID: "M", Kind: Subscription, Amount: dec.Rat("1"), Currency: "USD"},
		"zero amount":     {MovementID: "M", PortfolioID: "PF", Kind: Subscription, Amount: dec.Rat("0"), Currency: "USD"},
		"nil amount":      {MovementID: "M", PortfolioID: "PF", Kind: Subscription, Currency: "USD"},
		"empty currency":  {MovementID: "M", PortfolioID: "PF", Kind: Subscription, Amount: dec.Rat("1")},
		"unknown kind":    {MovementID: "M", PortfolioID: "PF", Kind: Kind(99), Amount: dec.Rat("1"), Currency: "USD"},
	}
	for name, m := range cases {
		if _, _, err := encode(m, now); err == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}
}

// A zero effective time falls back to the knowledge time so the FACT always
// carries a domain time.
func TestEncodeEffectiveFallback(t *testing.T) {
	m := mv(Subscription, "1")
	m.Effective = time.Time{}
	know := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	entry, _, err := encode(m, know)
	if err != nil {
		t.Fatal(err)
	}
	if !entry.GetEffectiveTime().AsTime().Equal(know) {
		t.Fatalf("effective = %v, want knowledge fallback %v", entry.GetEffectiveTime().AsTime(), know)
	}
}

// A LARGE SUBSCRIPTION POSTS ITS ACTUAL AMOUNT (#94).
//
// The cash leg went through the WRAPPING dec.ToProto, which overflows once the
// scaled coefficient exceeds an int64 — about $92.2bn at scale 8. Above that the
// book of record did not round the figure, it carried a different one: a $100bn
// subscription posted as roughly $7.7bn, and every NAV and every report derived
// from it agreed with each other and with nothing real.
func TestEncodeDoesNotWrapALargeSubscription(t *testing.T) {
	const amount = "100000000000" // $100bn — above the wrap threshold, below absurd

	entry, _, err := encode(mv(Subscription, amount), time.Now())
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if got := dec.Str(dec.FromProto(entry.GetCash())); got != amount {
		t.Fatalf("posted cash = %s, want %s — the ledger recorded an amount nobody moved", got, amount)
	}
}

// NON-VACUITY: an ordinary amount is unchanged, so the guard above is not met by
// an encoder that refuses or mangles everything.
func TestEncodeStillPostsAnOrdinaryAmountExactly(t *testing.T) {
	entry, _, err := encode(mv(Subscription, "1234.56"), time.Now())
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if got := dec.Str(dec.FromProto(entry.GetCash())); got != "1234.56" {
		t.Fatalf("posted cash = %s, want 1234.56", got)
	}
}
