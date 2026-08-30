package monitor

import (
	"strconv"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	comp "github.com/eighred/kanz/internal/compliance"
)

// BenchmarkSnapshot measures what one position FACT costs the monitor, as a
// function of how many instruments the book holds — which is the whole point of
// #810. snapshot() runs once per FACT (applyAndSnapshot) and copies every entry,
// so an unevicted flat is charged to every subsequent event for the life of the
// pod.
//
// Run it as:
//
//	go test ./services/compliance/internal/monitor/ -run '^$' -bench Snapshot -benchmem
func BenchmarkSnapshot(b *testing.B) {
	for _, n := range []int{50, 1000} {
		b.Run("held="+strconv.Itoa(n), func(b *testing.B) {
			m := NewMonitor(comp.NewEngine(nil), comp.NewMandateRegistry(), nil, nil, nil, nil)
			key := bookKey{tenant: "t1", portfolio: "p1"}
			seedBook(m, key, n)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = m.snapshot(key)
			}
		})
	}
}

// BenchmarkHandlePositionFact is the end-to-end per-FACT cost through a mandate
// that actually evaluates, on a book of the given size.
func BenchmarkHandlePositionFact(b *testing.B) {
	for _, n := range []int{50, 1000} {
		b.Run("held="+strconv.Itoa(n), func(b *testing.B) {
			reg := comp.NewMandateRegistry()
			if err := reg.Put(benchMandate()); err != nil {
				b.Fatal(err)
			}
			m := NewMonitor(comp.NewEngine(nil), reg, nil, NewEmitter(&fakeBus{}), nil, nil)
			key := bookKey{tenant: "t1", portfolio: "p1"}
			seedBook(m, key, n)
			ctx := testCtx()
			env := &envelopepb.Envelope{TenantId: "t1"}
			payload := benchPositionFact(b, "AAA0", 100, 1000)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := m.Handle(ctx, env, payload); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func benchMandate() *compliancepb.Mandate {
	return &compliancepb.Mandate{
		MandateId: "m1", TenantId: "t1", PortfolioId: "p1", Version: 1,
		EffectiveAt: timestamppb.New(t0),
		Rules: []*compliancepb.Rule{{
			RuleId: "c1", Type: compliancepb.RuleType_RULE_TYPE_CONCENTRATION,
			Params: &compliancepb.Rule_Concentration{Concentration: &compliancepb.ConcentrationLimit{
				Dimension: compliancepb.Dimension_DIMENSION_INSTRUMENT,
				MaxWeight: decv(60, -2),
			}},
		}},
	}
}

// seedBook installs a book of n held instruments directly, so a benchmark
// measures the snapshot rather than the fold that built it.
func seedBook(m *Monitor, key bookKey, n int) {
	insts := make(map[string]comp.Position, n)
	for i := 0; i < n; i++ {
		id := "AAA" + strconv.Itoa(i)
		insts[id] = comp.Position{
			InstrumentID: id,
			Quantity:     decv(100, 0),
			MarketValue:  &commonpb.Money{Amount: decv(1000, 0), CurrencyCode: "USD"},
		}
	}
	m.mu.Lock()
	m.books[key] = &book{positions: insts}
	m.mu.Unlock()
}

func benchPositionFact(b *testing.B, inst string, qty, mv int64) []byte {
	b.Helper()
	out, err := proto.Marshal(&domainpb.PositionState{
		PortfolioId:  "p1",
		InstrumentId: inst,
		Quantity:     decv(qty, 0),
		MarketValue:  &commonpb.Money{Amount: decv(mv, 0), CurrencyCode: "USD"},
		AsOf:         timestamppb.New(t0.Add(time.Minute)),
	})
	if err != nil {
		b.Fatal(err)
	}
	return out
}
