package execution

import (
	"errors"
	"strings"
	"testing"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	"github.com/eighred/kanz/internal/dec"
)

// ParseDec IS THE VENUE PATH'S FRONT DOOR (#94).
//
// Every exchange string this platform acts on comes through here — a fill
// quantity, a fill price, a commission — for BOTH venues: venue-binance aliases
// these helpers rather than keeping its own copy (bridge.go). It had two defects
// at once, and they fail in opposite directions.

// DEFECT ONE: AN UNPARSEABLE STRING RETURNED ZERO.
//
// It answered `&commonpb.Decimal{}` for anything big.Rat could not read, so a
// garbled FillPx became a fill at price 0 and a garbled FillSz a fill of size 0 —
// published as FACTs the ledger folds. This is the never-substitute-zero rule
// broken in the parser itself, and it is the worse of the two defects: a zero
// price does not look wrong on a dashboard, it looks free.
func TestParseDecRefusesRatherThanReturningZero(t *testing.T) {
	for _, s := range []string{"", "abc", "1.2.3", "NaN", "--5", "1e", " "} {
		t.Run("input:"+s, func(t *testing.T) {
			got, ok := ParseDec(s)
			if ok {
				t.Fatalf("ParseDec(%q) accepted the string and returned %v", s, got)
			}
			if got != nil {
				t.Errorf("ParseDec(%q) returned %v alongside ok=false — a refusal must not "+
					"also hand back a value a caller might use", s, got)
			}
		})
	}
}

// DEFECT TWO: A LARGE VALUE WRAPPED.
//
// It converted through dec.ToProto, which overflows once the scaled coefficient
// exceeds an int64 — about 92.2 billion units at scale 8. That reads as
// unreachable in dollars and is an ordinary position in tokens: both venues list
// assets trading in the trillions. An accFillSz of 1e12 came back as
// 77662796314.5224192, and that number was published as the filled size.
func TestParseDecDoesNotWrapALargeQuantity(t *testing.T) {
	for _, s := range []string{
		"92000000000",       // just under the old wrap threshold
		"1000000000000",     // 1e12 — an ordinary meme-coin position
		"5000000000000",     // 5e12
		"-1000000000000",    // and the same magnitude short
		"123456789012345.5", // large with a fractional part
	} {
		t.Run("input:"+s, func(t *testing.T) {
			d, ok := ParseDec(s)
			if !ok {
				t.Fatalf("ParseDec(%q) refused a representable value", s)
			}
			if got := dec.Str(dec.FromProto(d)); got != s {
				t.Fatalf("ParseDec(%q) round-tripped to %s — the venue path would act on a "+
					"quantity nobody traded", s, got)
			}
		})
	}
}

// NON-VACUITY: ordinary values still convert exactly, so a parser that refused
// everything would not satisfy the tests above.
func TestParseDecStillReadsOrdinaryValues(t *testing.T) {
	for _, s := range []string{"0", "1", "0.00000001", "50000.25", "-3.5", "1234.56789"} {
		d, ok := ParseDec(s)
		if !ok {
			t.Fatalf("ParseDec(%q) refused an ordinary value", s)
		}
		if got := dec.Str(dec.FromProto(d)); got != s {
			t.Errorf("ParseDec(%q) = %s, want %s", s, got, s)
		}
	}
}

// FormatDec is the other half of the round trip — what actually reaches the
// exchange wire. A value that parses correctly and then renders wrong is the same
// incident one step later, so the two are pinned together.
func TestParseDecAndFormatDecRoundTrip(t *testing.T) {
	for _, s := range []string{"1", "0.5", "50000.25", "92000000000", "1000000000000"} {
		d, ok := ParseDec(s)
		if !ok {
			t.Fatalf("ParseDec(%q) refused", s)
		}
		if got := FormatDec(d); got != s {
			t.Errorf("FormatDec(ParseDec(%q)) = %q — the venue would receive a different "+
				"number than the one read", s, got)
		}
	}
}

// SubDec computes leaves quantity (ordered − filled) on every venue fill report.
func TestSubDecIsExactAtLargeMagnitudes(t *testing.T) {
	ordered, ok := ParseDec("1000000000000")
	if !ok {
		t.Fatal("ParseDec refused the ordered quantity")
	}
	filled, ok := ParseDec("250000000000")
	if !ok {
		t.Fatal("ParseDec refused the filled quantity")
	}
	leaves, ok := SubDec(ordered, filled)
	if !ok {
		t.Fatal("SubDec refused a representable subtraction")
	}
	if got := dec.Str(dec.FromProto(leaves)); got != "750000000000" {
		t.Fatalf("leaves = %s, want 750000000000 — a wrong leaves quantity is what the next "+
			"cancel or sweep is sized from", got)
	}
}

// LeavesRemaining IS THE BOUND BOTH CONNECTORS COMPUTE LEAVES THROUGH (#1045).
//
// The subtraction it replaces was not a bound at all: SubDec represents a
// negative result and reports ok, so a venue reporting a cumulative 14 against
// an order of 10 produced leaves −4 and published it as a fill FACT the position
// book and the accounting ledger both fold. The OMS order aggregate does not
// consume that subject, so nothing downstream refused it either.
func TestLeavesRemainingRefusesAnOverfill(t *testing.T) {
	ordered, cumulative := decOf(t, "10"), decOf(t, "14")
	leaves, err := LeavesRemaining(ordered, cumulative)
	if !errors.Is(err, ErrVenueOverfill) {
		t.Fatalf("LeavesRemaining(10, 14) err = %v, want ErrVenueOverfill — a venue cannot fill "+
			"more than it was sent, and a negative leaves quantity is what booking it looks like", err)
	}
	if leaves != nil {
		t.Errorf("leaves = %v on a refusal, want nil — a caller reading past the error must not "+
			"find a usable number there", leaves)
	}
	// The refusal names both numbers, because that is the whole finding an
	// operator resolves against the exchange's own order history.
	if msg := err.Error(); !strings.Contains(msg, "10") || !strings.Contains(msg, "14") {
		t.Errorf("refusal %q names neither the ordered nor the reported cumulative quantity", msg)
	}
}

// EQUALITY IS THE ORDINARY TERMINAL CASE, not an over-fill. A bound that refused
// it would stop every completed order from being booked.
func TestLeavesRemainingAllowsAnExactFill(t *testing.T) {
	leaves, err := LeavesRemaining(decOf(t, "10"), decOf(t, "10"))
	if err != nil {
		t.Fatalf("LeavesRemaining(10, 10) = %v, want no error", err)
	}
	if got := dec.FromProto(leaves); got.Sign() != 0 {
		t.Errorf("leaves = %s, want 0", got.FloatString(8))
	}
}

func TestLeavesRemainingIsTheOrdinaryPartialSubtraction(t *testing.T) {
	leaves, err := LeavesRemaining(decOf(t, "10"), decOf(t, "3.5"))
	if err != nil {
		t.Fatalf("LeavesRemaining(10, 3.5) = %v", err)
	}
	if got := dec.FromProto(leaves).FloatString(1); got != "6.5" {
		t.Errorf("leaves = %s, want 6.5", got)
	}
}

func decOf(t *testing.T, s string) *commonpb.Decimal {
	t.Helper()
	d, ok := ParseDec(s)
	if !ok {
		t.Fatalf("ParseDec(%q) refused", s)
	}
	return d
}
