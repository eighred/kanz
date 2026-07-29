package compute

// LATENCY-01b — white-box micro-benchmarks for the unexported Decimal
// arithmetic helpers (the per-position inner loop of every measure). In
// `package compute` (not compute_test) so it can reach addDecimal/mulDecimal/
// absDecimal/sumInBaseCurrency directly. See profiling.md for the analysis.

import (
	"fmt"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/domain"
)

// decSink defeats dead-code elimination of the benchmarked result.
var decSink *commonpb.Decimal

func BenchmarkAddDecimal_Aligned(b *testing.B) {
	a := &commonpb.Decimal{Coefficient: 100, Exponent: -2}
	c := &commonpb.Decimal{Coefficient: 250, Exponent: -2}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		decSink = addDecimal(a, c)
	}
}

// Misaligned exponents force the pow10 rescale on both operands.
func BenchmarkAddDecimal_Misaligned(b *testing.B) {
	a := &commonpb.Decimal{Coefficient: 12345, Exponent: -2} // 123.45
	c := &commonpb.Decimal{Coefficient: 6789, Exponent: -3}  // 6.789
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		decSink = addDecimal(a, c)
	}
}

func BenchmarkMulDecimal(b *testing.B) {
	a := &commonpb.Decimal{Coefficient: 12345, Exponent: -2}
	c := &commonpb.Decimal{Coefficient: 1, Exponent: -2}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		decSink = mulDecimal(a, c)
	}
}

func BenchmarkAbsDecimal(b *testing.B) {
	neg := &commonpb.Decimal{Coefficient: -98765, Exponent: -2}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		decSink = absDecimal(neg)
	}
}

// BenchmarkSumInBaseCurrency is the measure inner loop: one addDecimal (+ an
// absDecimal in the gross case) per position, each allocating a fresh Decimal.
func BenchmarkSumInBaseCurrency(b *testing.B) {
	for _, n := range []int{16, 256, 1024} {
		p := benchPortfolio(n)
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				decSink = sumInBaseCurrency(p, true)
			}
		})
	}
}

func benchPortfolio(n int) *domain.Portfolio {
	p := domain.NewPortfolio("BENCH", "USD")
	p.SetAggregate(domain.AggregateUpdate{AsOf: time.Unix(0, 0), BaseCurrency: "USD"})
	for i := 0; i < n; i++ {
		p.SetPosition(domain.Position{
			InstrumentID: v1.InstrumentID(fmt.Sprintf("INS%06d", i)),
			MarketValue: &commonpb.Money{
				Amount:       &commonpb.Decimal{Coefficient: int64(i + 1), Exponent: int32(-(i % 4))},
				CurrencyCode: "USD",
			},
			AsOf: time.Unix(0, 0),
		})
	}
	return p
}
