package integrity_test

import (
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"

	"github.com/kanz-eng/kanz/internal/integrity"
)

// stalenessEnv builds an envelope with the two timestamps the monitor
// reads; eventType is the stream subject.
func stalenessEnv(eventType, partitionKey string, eventTime, ingestionTime time.Time) *envelopepb.Envelope {
	return &envelopepb.Envelope{
		EventType:     eventType,
		PartitionKey:  partitionKey,
		EventTime:     timestamppb.New(eventTime),
		IngestionTime: timestamppb.New(ingestionTime),
	}
}

var base = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// lagEnv: event happened at `base`, ingested `lag` later.
func lagEnv(lag time.Duration) *envelopepb.Envelope {
	return stalenessEnv("market.equity.trade", "AAPL", base, base.Add(lag))
}

// --- Levels -----------------------------------------------------------

func TestObserve_FreshWithinBudget(t *testing.T) {
	m := integrity.NewStalenessMonitor(integrity.DefaultStalenessConfig())
	r := m.Observe(lagEnv(2 * time.Second))
	if r.Level != integrity.StalenessFresh {
		t.Errorf("Level=%v want fresh", r.Level)
	}
	if r.Lag != 2*time.Second {
		t.Errorf("Lag=%v want 2s", r.Lag)
	}
	if r.Threshold != 0 {
		t.Errorf("Threshold=%v want 0 for fresh", r.Threshold)
	}
	if r.Stale() {
		t.Error("Stale() true for fresh")
	}
}

func TestObserve_StaleAboveWarn(t *testing.T) {
	m := integrity.NewStalenessMonitor(integrity.DefaultStalenessConfig())
	r := m.Observe(lagEnv(30 * time.Second)) // warn=10s, critical=60s
	if r.Level != integrity.StalenessStale {
		t.Fatalf("Level=%v want stale", r.Level)
	}
	if r.Threshold != 10*time.Second {
		t.Errorf("Threshold=%v want WarnAfter 10s", r.Threshold)
	}
	if !r.Stale() {
		t.Error("Stale() false for stale level")
	}
}

func TestObserve_CriticalAboveCritical(t *testing.T) {
	m := integrity.NewStalenessMonitor(integrity.DefaultStalenessConfig())
	r := m.Observe(lagEnv(90 * time.Second))
	if r.Level != integrity.StalenessCritical {
		t.Fatalf("Level=%v want critical", r.Level)
	}
	if r.Threshold != 60*time.Second {
		t.Errorf("Threshold=%v want CriticalAfter 60s", r.Threshold)
	}
	if !r.Stale() {
		t.Error("Stale() false for critical level")
	}
}

// Boundaries are exclusive-above (lag == threshold is the lower level),
// matching RISK-11's "above the budget" discipline.
func TestObserve_BoundariesAreInclusiveOfLowerLevel(t *testing.T) {
	m := integrity.NewStalenessMonitor(integrity.DefaultStalenessConfig())
	if r := m.Observe(lagEnv(10 * time.Second)); r.Level != integrity.StalenessFresh {
		t.Errorf("lag==WarnAfter Level=%v want fresh", r.Level)
	}
	if r := m.Observe(lagEnv(60 * time.Second)); r.Level != integrity.StalenessStale {
		t.Errorf("lag==CriticalAfter Level=%v want stale", r.Level)
	}
}

// --- Clock skew / future-dated ----------------------------------------

func TestObserve_FutureDatedBeyondSkewTolerance(t *testing.T) {
	m := integrity.NewStalenessMonitor(integrity.DefaultStalenessConfig())
	r := m.Observe(lagEnv(-5 * time.Second)) // event_time 5s ahead of ingestion
	if r.Level != integrity.StalenessFuture {
		t.Errorf("Level=%v want future", r.Level)
	}
	if r.Stale() {
		t.Error("Stale() true for future-dated (it's skew, not staleness)")
	}
}

func TestObserve_SmallNegativeLagToleratedAsFresh(t *testing.T) {
	m := integrity.NewStalenessMonitor(integrity.DefaultStalenessConfig())
	// Within SkewTolerance (1s) → treated as fresh, not future.
	r := m.Observe(lagEnv(-500 * time.Millisecond))
	if r.Level != integrity.StalenessFresh {
		t.Errorf("Level=%v want fresh (within skew tolerance)", r.Level)
	}
}

// --- Missing data -----------------------------------------------------

func TestObserve_NilEnvelopeIsUnknown(t *testing.T) {
	m := integrity.NewStalenessMonitor(integrity.DefaultStalenessConfig())
	if r := m.Observe(nil); r.Level != integrity.StalenessUnknown {
		t.Errorf("Level=%v want unknown", r.Level)
	}
}

func TestObserve_MissingTimestampsAreUnknown(t *testing.T) {
	m := integrity.NewStalenessMonitor(integrity.DefaultStalenessConfig())
	noEvent := &envelopepb.Envelope{
		EventType:     "market.equity.trade",
		IngestionTime: timestamppb.New(base),
	}
	if r := m.Observe(noEvent); r.Level != integrity.StalenessUnknown {
		t.Errorf("missing event_time Level=%v want unknown", r.Level)
	}
	noIngest := &envelopepb.Envelope{
		EventType: "market.equity.trade",
		EventTime: timestamppb.New(base),
	}
	if r := m.Observe(noIngest); r.Level != integrity.StalenessUnknown {
		t.Errorf("missing ingestion_time Level=%v want unknown", r.Level)
	}
}

// --- Frontier tracking ------------------------------------------------

func TestObserve_FrontierTracksMostRecentEventTime(t *testing.T) {
	m := integrity.NewStalenessMonitor(integrity.DefaultStalenessConfig())

	// Observe in increasing event_time order; frontier follows.
	r1 := m.Observe(stalenessEnv("market.equity.trade", "AAPL", base, base.Add(time.Second)))
	if !r1.LastEventTime.Equal(base) {
		t.Errorf("frontier=%v want %v", r1.LastEventTime, base)
	}
	later := base.Add(5 * time.Minute)
	r2 := m.Observe(stalenessEnv("market.equity.trade", "AAPL", later, later.Add(time.Second)))
	if !r2.LastEventTime.Equal(later) {
		t.Errorf("frontier=%v want %v", r2.LastEventTime, later)
	}

	// An out-of-order older event must NOT move the frontier backwards.
	r3 := m.Observe(stalenessEnv("market.equity.trade", "AAPL", base, base.Add(time.Second)))
	if !r3.LastEventTime.Equal(later) {
		t.Errorf("frontier moved backwards to %v want %v", r3.LastEventTime, later)
	}
}

func TestObserve_FrontierIsolatedPerSubjectAndPartition(t *testing.T) {
	m := integrity.NewStalenessMonitor(integrity.DefaultStalenessConfig())
	aapl := base.Add(time.Hour)
	m.Observe(stalenessEnv("market.equity.trade", "AAPL", aapl, aapl.Add(time.Second)))

	// Different partition_key: its own frontier, starting at this event.
	rMsft := m.Observe(stalenessEnv("market.equity.trade", "MSFT", base, base.Add(time.Second)))
	if !rMsft.LastEventTime.Equal(base) {
		t.Errorf("MSFT frontier=%v want %v (isolated from AAPL)", rMsft.LastEventTime, base)
	}
	// Different subject: own frontier too.
	rQuote := m.Observe(stalenessEnv("market.equity.quote", "AAPL", base, base.Add(time.Second)))
	if !rQuote.LastEventTime.Equal(base) {
		t.Errorf("quote frontier=%v want %v (isolated from trade)", rQuote.LastEventTime, base)
	}
}

// --- Config -----------------------------------------------------------

func TestNewStalenessMonitor_ZeroFieldsFallBackToDefaults(t *testing.T) {
	// Only override WarnAfter; the others should default.
	m := integrity.NewStalenessMonitor(integrity.StalenessConfig{WarnAfter: 100 * time.Millisecond})
	// 200ms lag now exceeds the 100ms warn but is under the default 60s
	// critical → stale.
	r := m.Observe(lagEnv(200 * time.Millisecond))
	if r.Level != integrity.StalenessStale {
		t.Errorf("Level=%v want stale with custom WarnAfter", r.Level)
	}
	if r.Threshold != 100*time.Millisecond {
		t.Errorf("Threshold=%v want 100ms", r.Threshold)
	}
}

func TestObserve_ResultCarriesSubjectAndPartition(t *testing.T) {
	m := integrity.NewStalenessMonitor(integrity.DefaultStalenessConfig())
	r := m.Observe(lagEnv(1 * time.Second))
	if r.Subject != "market.equity.trade" || r.PartitionKey != "AAPL" {
		t.Errorf("Subject=%q PartitionKey=%q want market.equity.trade/AAPL", r.Subject, r.PartitionKey)
	}
}
