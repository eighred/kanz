package riskview

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
)

// THE OMS'S LOCAL COPY OF WHAT RISK HAS COMPUTED (#438).
//
// The pre-trade gate checks a declared risk limit against this map. It must
// never answer with a number it cannot show to be current — a stale VaR passed as
// fresh is worse than none, because the whole point of the gate is that somebody
// trusted it.

var t0 = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

func measures(t *testing.T, pf string, asOf time.Time, kv map[string]int64) []byte {
	t.Helper()
	set := &domainpb.RiskMeasureSet{PortfolioId: pf, AsOf: timestamppb.New(asOf)}
	for name, v := range kv {
		set.Measures = append(set.Measures, &domainpb.RiskMeasure{
			Name: name, Value: &commonpb.Decimal{Coefficient: v},
		})
	}
	b, err := proto.Marshal(set)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestMeasure_FoldsWhatRiskAnnounced(t *testing.T) {
	v := New(WithClock(func() time.Time { return t0 }))
	if err := v.Handle(context.Background(), nil, measures(t, "PF1", t0, map[string]int64{"VaR99": 250})); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got, ok := v.Measure("PF1", "VaR99")
	if !ok {
		t.Fatal("the measure was not folded")
	}
	if got.RatString() != "250" {
		t.Errorf("VaR99 = %s, want 250", got.RatString())
	}
}

// A PORTFOLIO NEVER ANNOUNCED IS UNKNOWN, NOT ZERO. A zero VaR would pass every
// limit there is — a control reporting success on a portfolio the risk engine has
// never seen.
func TestMeasure_NeverAnnouncedIsUnknown(t *testing.T) {
	v := New(WithClock(func() time.Time { return t0 }))
	if _, ok := v.Measure("PF1", "VaR99"); ok {
		t.Fatal("an unannounced portfolio reported a measure")
	}
}

// A MEASURE ABSENT FROM THE LAST ANNOUNCEMENT IS UNKNOWN. The engine computing
// VaR and not ES does not make ES zero.
func TestMeasure_AbsentFromTheAnnouncementIsUnknown(t *testing.T) {
	v := New(WithClock(func() time.Time { return t0 }))
	_ = v.Handle(context.Background(), nil, measures(t, "PF1", t0, map[string]int64{"VaR99": 250}))
	if _, ok := v.Measure("PF1", "ExpectedShortfall97"); ok {
		t.Fatal("a measure the engine never produced was reported as known")
	}
}

// STALE IS UNKNOWN, AND IT SAYS SO.
//
// Measures are derived state; a lost announcement or a stalled engine must not
// leave the gate checking this morning's VaR against this afternoon's book.
func TestMeasure_StaleIsUnknownAndReported(t *testing.T) {
	now := t0
	var staleFor, staleMeasure string
	v := New(
		WithClock(func() time.Time { return now }),
		WithMaxAge(5*time.Minute),
		WithOnStale(func(pf, m string, _ time.Duration) { staleFor, staleMeasure = pf, m }),
	)
	_ = v.Handle(context.Background(), nil, measures(t, "PF1", t0, map[string]int64{"VaR99": 250}))
	if _, ok := v.Measure("PF1", "VaR99"); !ok {
		t.Fatal("a fresh measure was treated as stale")
	}

	now = t0.Add(6 * time.Minute)
	if _, ok := v.Measure("PF1", "VaR99"); ok {
		t.Fatal("a measure past its freshness bound gated an order — the gate would be checking " +
			"a VaR from before whatever moved the book")
	}
	if staleFor != "PF1" || staleMeasure != "VaR99" {
		t.Errorf("stale callback = (%q, %q), want PF1/VaR99 — an operator must learn that "+
			"announcements stopped, not infer it from orders being refused", staleFor, staleMeasure)
	}
}

// A LEVEL, NOT A DELTA. A measure the engine has STOPPED producing must not show
// its last value forever — a stale number that never ages out is worse than none,
// because the freshness bound would never fire on it.
func TestHandle_IsAReplaceNotAMerge(t *testing.T) {
	v := New(WithClock(func() time.Time { return t0 }))
	_ = v.Handle(context.Background(), nil, measures(t, "PF1", t0,
		map[string]int64{"VaR99": 250, "ExpectedShortfall97": 400}))
	_ = v.Handle(context.Background(), nil, measures(t, "PF1", t0, map[string]int64{"VaR99": 260}))

	if _, ok := v.Measure("PF1", "ExpectedShortfall97"); ok {
		t.Fatal("a measure dropped from the latest announcement still reported a value — it " +
			"would never age out, because its portfolio keeps being refreshed")
	}
	got, _ := v.Measure("PF1", "VaR99")
	if got.RatString() != "260" {
		t.Errorf("VaR99 = %s, want the latest 260", got.RatString())
	}
}

// GARBAGE IS ACKED. Nacking replays the same bad bytes forever while the measures
// age out to unknown and the gate refuses anyway.
func TestHandle_AcksGarbage(t *testing.T) {
	v := New(WithClock(func() time.Time { return t0 }))
	if err := v.Handle(context.Background(), nil, []byte("not a proto")); err != nil {
		t.Fatalf("Handle(garbage) = %v, want nil", err)
	}
}

// AN OUT-OF-DOMAIN EXPONENT IS REFUSED BEFORE ANY NUMBER IS READ (#95).
func TestHandle_RefusesAnOutOfDomainExponent(t *testing.T) {
	v := New(WithClock(func() time.Time { return t0 }))
	payload, err := proto.Marshal(&domainpb.RiskMeasureSet{
		PortfolioId: "PF1", AsOf: timestamppb.New(t0),
		Measures: []*domainpb.RiskMeasure{{
			Name: "VaR99", Value: &commonpb.Decimal{Coefficient: 1, Exponent: 2_000_000_000},
		}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- v.Handle(context.Background(), nil, payload) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Handle = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Handle did not return — the exponent was materialised instead of refused (#95)")
	}
	if _, ok := v.Measure("PF1", "VaR99"); ok {
		t.Fatal("an out-of-domain measure was folded")
	}
}

// HELD vs CURRENT is the posture an operator needs BEFORE a risk limit starts
// refusing everything.
func TestStats_SeparatesHeldFromCurrent(t *testing.T) {
	now := t0
	v := New(WithClock(func() time.Time { return now }), WithMaxAge(5*time.Minute))
	_ = v.Handle(context.Background(), nil, measures(t, "PF1", t0, map[string]int64{"VaR99": 1}))
	_ = v.Handle(context.Background(), nil, measures(t, "PF2", t0.Add(-time.Hour), map[string]int64{"VaR99": 1}))

	held, live := v.Stats()
	if held != 2 || live != 1 {
		t.Fatalf("held=%d live=%d, want 2 and 1 — held−current is the population whose orders "+
			"a risk mandate will refuse", held, live)
	}
}
