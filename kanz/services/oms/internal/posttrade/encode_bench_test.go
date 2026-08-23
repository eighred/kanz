package posttrade_test

import (
	"testing"

	"github.com/eighred/kanz/services/oms/internal/posttrade"
)

// EncodeFail is on the publish path: one call per detected fail, per detection
// tick. It allocates the proto and its two timestamps and nothing else — no
// intermediate slice, and no fmt on the success path (the Errorf calls are
// reached only on a refusal). This pins that, so a future validation added with
// a formatted string in the hot path shows up as an allocation rather than as a
// slow morning.
func BenchmarkEncodeFail(b *testing.B) {
	f := validFail("i1")
	b.ReportAllocs()
	for b.Loop() {
		if _, err := posttrade.EncodeFail(f); err != nil {
			b.Fatal(err)
		}
	}
}
