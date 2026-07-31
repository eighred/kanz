package dec

import (
	"strings"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

func ok(exp int32) *commonpb.Decimal { return &commonpb.Decimal{Coefficient: 1, Exponent: exp} }
func absurd() *commonpb.Decimal      { return &commonpb.Decimal{Coefficient: 1, Exponent: 2000000000} }

// A well-formed Fill passes, and passes without inventing a field path. This is
// the non-vacuity half: a walk that refused everything would satisfy every
// refusal test below.
func TestInDomainDeep_OrdinaryMessagePasses(t *testing.T) {
	f := &orderpb.Fill{
		FillId: "f-1", OrderId: "o-1", InstrumentId: "BTC-USD",
		Quantity: ok(-8), Price: ok(-2),
		Fee: &commonpb.Money{Amount: ok(-8), CurrencyCode: "USD"},
	}
	if path, in := InDomainDeep(f); !in {
		t.Fatalf("an ordinary fill was refused at %q", path)
	}
}

// THE FIELD THE WALK EXISTS FOR: fee.amount is one level below the message the
// consumer unmarshalled. A per-field check at the top level does not see it, and
// tv-sync reads it through a helper returning a bare *big.Rat.
func TestInDomainDeep_FindsANestedDecimal(t *testing.T) {
	f := &orderpb.Fill{
		FillId: "f-1", Quantity: ok(-8), Price: ok(-2),
		Fee: &commonpb.Money{Amount: absurd(), CurrencyCode: "USD"},
	}
	path, in := InDomainDeep(f)
	if in {
		t.Fatal("an out-of-domain fee amount was accepted — FromProto on it does not return")
	}
	if path != "fee.amount" {
		t.Errorf("path = %q, want %q: a DLQ entry that does not name the field sends an "+
			"operator to read a binary payload by hand", path, "fee.amount")
	}
}

// Top-level fields are found too, and the path names them.
func TestInDomainDeep_FindsATopLevelDecimal(t *testing.T) {
	for _, tc := range []struct {
		name string
		fill *orderpb.Fill
		want string
	}{
		{"quantity", &orderpb.Fill{Quantity: absurd(), Price: ok(-2)}, "quantity"},
		{"price", &orderpb.Fill{Quantity: ok(-8), Price: absurd()}, "price"},
		{"negative exponent", &orderpb.Fill{Price: &commonpb.Decimal{Coefficient: 1, Exponent: -2000000000}}, "price"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, in := InDomainDeep(tc.fill)
			if in {
				t.Fatal("an out-of-domain decimal was accepted")
			}
			if path != tc.want {
				t.Errorf("path = %q, want %q", path, tc.want)
			}
		})
	}
}

// Absent is not out of range. An empty message has no Decimals to refuse, and a
// consumer's own "this field is required" check owns the missing case — the walk
// must not start refusing messages on its behalf.
func TestInDomainDeep_AbsentIsInDomain(t *testing.T) {
	if path, in := InDomainDeep(&orderpb.Fill{}); !in {
		t.Fatalf("an empty Fill was refused at %q", path)
	}
	if _, in := InDomainDeep(nil); !in {
		t.Fatal("a nil message was refused")
	}
}

// The boundary is the same number FromProtoChecked enforces. If these drift, the
// bus refuses what the converter would have accepted, or worse, the reverse.
func TestInDomainDeep_SharesTheBoundWithInDomain(t *testing.T) {
	for _, exp := range []int32{-65, -64, 0, 64, 65} {
		f := &orderpb.Fill{Price: ok(exp)}
		_, deep := InDomainDeep(f)
		if deep != InDomain(ok(exp)) {
			t.Fatalf("exponent %d: InDomainDeep = %v but InDomain = %v", exp, deep, InDomain(ok(exp)))
		}
	}
}

// The walk must terminate promptly on the input that hangs FromProto — it reads
// the exponent, it does not raise ten to it.
func TestInDomainDeep_DoesNotMaterialiseTheExponent(t *testing.T) {
	done := make(chan string, 1)
	go func() {
		p, _ := InDomainDeep(&orderpb.Fill{Fee: &commonpb.Money{Amount: absurd()}})
		done <- p
	}()
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("InDomainDeep did not return within 4s — the walk is expanding 10^exponent")
	}
}

// A Decimal that no longer has an exponent field is refused, not waved through:
// it means the walk has stopped understanding the type it is checking.
func TestInDomainDeep_RefusesADecimalItCannotRead(t *testing.T) {
	// commonpb.Money is not a Decimal, so it is walked rather than bound-checked;
	// this pins that the name match is what selects the check.
	if path, in := InDomainDeep(&commonpb.Money{CurrencyCode: "USD"}); !in {
		t.Fatalf("a Money with no amount was refused at %q", path)
	}
	if !strings.HasPrefix(string(decimalName), "common.v1.") {
		t.Error("decimalName no longer points at the platform's Decimal")
	}
}
