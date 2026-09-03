package projection

import (
	"fmt"
	"math/big"
	"testing"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"time"
)

// BenchmarkPositionFoldByHistoryDepth measures a full history fold — what a
// BITEMPORAL as-of read costs, and what the LIVE path used to cost per fill.
//
// It is deliberately kept after #995 rather than deleted, because it measures a
// cost that still exists and should stay visible: an explicit as-of query walks
// the whole history by design, since only the history knows what was believed at
// a past instant. At 100k executions that is ~208ms, paid by the operator who
// asked the question.
//
// Until #995 the LIVE path paid it too — twice per fill, because appendFill
// published a position delta and a state delta and each re-folded everything, so
// the Nth fill cost O(N). Compare against BenchmarkAppendFillByHistoryDepth in
// livefold_test.go, which measures the same fill path now: ~8.5us, flat in
// history depth.
func BenchmarkPositionFoldByHistoryDepth(b *testing.B) {
	for _, n := range []int{100, 1000, 10000, 100000} {
		b.Run(fmt.Sprintf("execs=%d", n), func(b *testing.B) {
			execs := make([]execution, 0, n)
			base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			for i := 0; i < n; i++ {
				side := orderpb.Side_SIDE_BUY
				if i%2 == 1 {
					side = orderpb.Side_SIDE_SELL
				}
				execs = append(execs, execution{
					fillID: fmt.Sprintf("f-%d", i), orderID: fmt.Sprintf("o-%d", i),
					instrument: "BTC-USD", venue: "BINANCE", side: side,
					qty: big.NewRat(1, 1), price: big.NewRat(int64(50000+i%100), 1),
					fee: big.NewRat(1, 100), effective: base.Add(time.Duration(i) * time.Second),
					knowledge: base.Add(time.Duration(i) * time.Second),
				})
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = foldPositions(visibleExecs(execs, time.Time{}))
			}
		})
	}
}
