package monitor

// WHAT THE POST-TRADE COMPLIANCE BOOK COSTS UNDER CONTENTION (#1008).
//
// This is the first b.RunParallel in the module. `grep -rn "RunParallel"
// --include=*.go` returned NOTHING across all sixteen benchmarks before this
// file, so contention on shared mutable state had never been measured anywhere
// in this estate — including on the one control that decides whether a fund is
// still inside its mandate after the trade.
//
// # What it is for, and what it is NOT evidence of
//
// PRODUCTION DISPATCH CONCURRENCY IS ONE. The monitor's position fold is a
// SubscribeBroadcast, and one bus subject is dispatched by one goroutine —
// stated in test/arch/jetstream_consumer_tuning_test.go, stated again in
// test/load/capacity-model.md, and measured against a real broker in #1008.
//
// So a parallel benchmark here does not describe today's deployment. It
// describes the CEILING today's deployment would hit if dispatch were widened,
// and it is the measurement that has to exist BEFORE anyone widens it: the
// ordering guarantee the monitor gets for free from a single dispatch goroutine
// is the same thing capping it, and removing that guarantee without a
// per-aggregate lock underneath would trade a throughput ceiling for a
// correctness one.
//
// Read the two together:
//
//	BenchmarkHandlePositionFact          serial per-FACT cost — what production pays
//	BenchmarkHandlePositionFactParallel  the same work under GOMAXPROCS goroutines
//
// If the parallel number does not scale with cores, the shared lock is the
// ceiling. That is the claim #1008 rests on, and it is now checkable rather than
// asserted.

import (
	"strconv"
	"sync/atomic"
	"testing"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	comp "github.com/eighred/kanz/internal/compliance"
)

// BenchmarkHandlePositionFactParallel measures the per-FACT cost when many
// goroutines fold into DIFFERENT portfolios — the shape a widened dispatch would
// produce, and the one where a process-global lock shows up as a flat curve.
//
// Distinct portfolios per goroutine is the point. Two goroutines folding the
// SAME portfolio must serialize — that is the per-aggregate ordering the book
// depends on and it is not a bottleneck to remove. What must NOT serialize is
// two goroutines folding two unrelated funds, and before the per-aggregate lock
// they did: Monitor.mu covers every tenant and every portfolio in the process.
func BenchmarkHandlePositionFactParallel(b *testing.B) {
	for _, n := range []int{50, 1000} {
		b.Run("held="+strconv.Itoa(n), func(b *testing.B) {
			const portfolios = 64

			reg := comp.NewMandateRegistry()
			for i := 0; i < portfolios; i++ {
				md := benchMandate()
				md.MandateId = "m" + strconv.Itoa(i)
				md.PortfolioId = "p" + strconv.Itoa(i)
				if err := reg.Put(md); err != nil {
					b.Fatal(err)
				}
			}
			m := NewMonitor(comp.NewEngine(nil), reg, nil, NewEmitter(&fakeBus{}), nil, nil)
			for i := 0; i < portfolios; i++ {
				seedBook(m, bookKey{tenant: "t1", portfolio: "p" + strconv.Itoa(i)}, n)
			}

			ctx := testCtx()
			env := &envelopepb.Envelope{TenantId: "t1"}
			payloads := make([][]byte, portfolios)
			for i := range payloads {
				payloads[i] = benchPositionFactFor(b, "p"+strconv.Itoa(i), "AAA0", 100, 1000)
			}

			b.ReportAllocs()
			b.ResetTimer()
			var seq atomic.Int64
			b.RunParallel(func(pb *testing.PB) {
				// Each goroutine walks its own portfolio, so the benchmark measures
				// CROSS-portfolio contention rather than the intended per-portfolio
				// serialization.
				i := int(seq.Add(1)) % portfolios
				for pb.Next() {
					if err := m.Handle(ctx, env, payloads[i]); err != nil {
						b.Fatal(err)
					}
				}
			})
		})
	}
}
