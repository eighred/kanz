package storeconformance

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"testing"

	"github.com/eighred/kanz/internal/fillfact"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"
)

// Specify the serialized economic identity independently: Rust's ordinary JSON
// encoder does not perform Go's HTML or U+2028/U+2029 escaping.
func identityCases(t *testing.T) []identityCase {
	t.Helper()
	cases := []struct{ name, venue, json string }{
		{"ascii", "VENUE", `["VENUE","account","instrument","execution"]`},
		{"html", "<>&", `["\u003c\u003e\u0026","account","instrument","execution"]`},
		{"unicode-separator", "\u2028\u2029", `["\u2028\u2029","account","instrument","execution"]`},
		{"controls", "\x00\n\t\r\b\f\"\\", `["\u0000\n\t\r\b\f\"\\","account","instrument","execution"]`},
		{"unicode", "İ東京😀", `["İ東京😀","account","instrument","execution"]`},
	}
	var out []identityCase
	for _, c := range cases {
		f := &orderpb.Fill{FillId: "alias", OrderId: "order", Venue: c.venue, VenueAccountId: "account", InstrumentId: "instrument", VenueExecutionId: "execution"}
		sum := sha256.Sum256([]byte(c.json))
		want := "execution:v1:" + hex.EncodeToString(sum[:])
		if got := fillfact.ExecutionKey(f); got != want {
			t.Fatalf("independent identity oracle %s", c.name)
		}
		out = append(out, identityCase{c.name, f, want})
	}
	for _, missing := range []int{0, 1, 2, 3} {
		f := &orderpb.Fill{FillId: "legacy", Venue: "venue", VenueAccountId: "account", InstrumentId: "instrument", VenueExecutionId: "execution"}
		switch missing {
		case 0:
			f.Venue = ""
		case 1:
			f.VenueAccountId = ""
		case 2:
			f.InstrumentId = ""
		case 3:
			f.VenueExecutionId = ""
		}
		if fillfact.ExecutionKey(f) != "legacy" {
			t.Fatal("missing attribution must retain legacy key")
		}
		out = append(out, identityCase{"legacy", f, "legacy"})
	}
	return out
}

type identityCase struct {
	name string
	fill *orderpb.Fill
	key  string
}

func TestExecutionKeyIndependentOracle(t *testing.T) { _ = identityCases(t) }

func TestRustExecutionEconomics(t *testing.T) {
	fixtureBinary(t)
	a := fill()
	for _, tc := range []struct {
		name   string
		want   bool
		mutate func(*orderpb.Fill)
	}{
		{"alias-not-economics", true, func(b *orderpb.Fill) { b.FillId = "transport-alias" }},
		{"decimal-representation", true, func(b *orderpb.Fill) { b.Price = &commonpb.Decimal{Coefficient: 125} }},
		{"price-change", false, func(b *orderpb.Fill) { b.Price.Coefficient++ }},
		{"missing-quantity", false, func(b *orderpb.Fill) { b.Quantity = nil }},
		{"missing-fee", false, func(b *orderpb.Fill) { b.Fee = nil }},
		{"currency", false, func(b *orderpb.Fill) { b.Fee.CurrencyCode = "EUR" }},
		{"tenant-independent-account", false, func(b *orderpb.Fill) { b.VenueAccountId = "other" }},
		{"instrument", false, func(b *orderpb.Fill) { b.InstrumentId = "other" }},
		{"order", false, func(b *orderpb.Fill) { b.OrderId = "other" }},
		{"timestamp-presence", false, func(b *orderpb.Fill) { b.ExecutedAt = nil }},
		{"timestamp-invalid", false, func(b *orderpb.Fill) { b.ExecutedAt.Nanos = 1_000_000_000 }},
		{"domain", false, func(b *orderpb.Fill) { b.Price.Exponent = 65 }},
		{"nested-domain", false, func(b *orderpb.Fill) {
			b.Recovery = &orderpb.ExecutionRecoveryProvenance{FeeApproval: &orderpb.ExecutionFeeApproval{PreviousFee: &commonpb.Money{Amount: &commonpb.Decimal{Exponent: 65}}}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := proto.Clone(a).(*orderpb.Fill)
			tc.mutate(b)
			if fillfact.SameExecution(a, b) != tc.want {
				t.Fatal("independent economic equality assumption differs")
			}
			expect(t, run(t, map[string]any{"mode": "same", "fill": wire(t, a), "other": wire(t, b)}), strconv.FormatBool(tc.want))
		})
	}
}

// Keep the fixture transport explicit and local; it is not an OMS command API.
func encodedJSON(t *testing.T, value any) []byte {
	t.Helper()
	b, e := json.Marshal(value)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
