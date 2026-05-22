package integrity_test

import (
	"math/rand"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"

	"github.com/kanz-eng/kanz/internal/integrity"
)

// DATA-10 — scenario tests that drive the gap/staleness/drift detectors over
// a *simulated bad-data stream* and assert the layer surfaces exactly the
// injected faults (and nothing on clean data). Distinct from the per-detector
// unit tests (gap_test/staleness_test/drift_test), which exercise one
// classification call at a time; these exercise the detectors over a
// realistic event sequence with faults injected deterministically (seeded
// rand, so a failure reproduces).

var badBase = time.Date(2026, 3, 2, 14, 0, 0, 0, time.UTC)

// seqEnv builds a sequenced envelope on a (source, eventType, partitionKey)
// stream with the given producer_sequence and event/ingestion times.
func seqEnv(source, eventType, pk string, seq uint64, eventTime, ingestionTime time.Time) *envelopepb.Envelope {
	return &envelopepb.Envelope{
		Source:           source,
		EventType:        eventType,
		PartitionKey:     pk,
		ProducerSequence: seq,
		EventTime:        timestamppb.New(eventTime),
		IngestionTime:    timestamppb.New(ingestionTime),
	}
}

// --- Gap detection under simulated drops ----------------------------------

func TestBadData_GapDetectorAccountsForEveryDroppedSequence(t *testing.T) {
	const total = 200
	for _, seed := range []int64{1, 2, 3, 4, 5} {
		rng := rand.New(rand.NewSource(seed))
		dropped := map[uint64]bool{}
		// Drop ~15% of sequences 2..total (keep 1 so FirstSeen baselines at 1
		// and every later miss is a real gap, not a mid-stream join).
		for s := uint64(2); s <= total; s++ {
			if rng.Float64() < 0.15 {
				dropped[s] = true
			}
		}

		d := integrity.NewGapDetector()
		var flaggedMissing uint64
		var firstSeenCount int
		for s := uint64(1); s <= total; s++ {
			if dropped[s] {
				continue
			}
			et := badBase.Add(time.Duration(s) * time.Millisecond)
			r := d.Observe(seqEnv("md/pod-1", "market.equity.trade", "AAPL", s, et, et))
			switch r.Status {
			case integrity.StatusGap:
				flaggedMissing += r.MissingCount()
			case integrity.StatusFirstSeen:
				firstSeenCount++
			}
		}

		if firstSeenCount != 1 {
			t.Fatalf("seed %d: FirstSeen count=%d want 1", seed, firstSeenCount)
		}
		if flaggedMissing != uint64(len(dropped)) {
			t.Errorf("seed %d: flagged %d missing, actually dropped %d",
				seed, flaggedMissing, len(dropped))
		}
	}
}

func TestBadData_CleanStreamRaisesNoGaps(t *testing.T) {
	d := integrity.NewGapDetector()
	for s := uint64(1); s <= 500; s++ {
		et := badBase.Add(time.Duration(s) * time.Millisecond)
		r := d.Observe(seqEnv("md/pod-1", "market.equity.trade", "AAPL", s, et, et))
		if r.Status == integrity.StatusGap {
			t.Fatalf("clean stream flagged a gap at seq %d: %+v", s, r)
		}
	}
}

func TestBadData_InterleavedSourcesDoNotManufactureGaps(t *testing.T) {
	// Two sources each writing a clean 1..N on the SAME partition_key. Keyed
	// on (source,event_type,partition_key) the detector must see two clean
	// streams, not one interleaved stream with phantom gaps (DATA-01 key).
	d := integrity.NewGapDetector()
	for s := uint64(1); s <= 100; s++ {
		et := badBase.Add(time.Duration(s) * time.Millisecond)
		for _, src := range []string{"md/pod-1", "md/pod-2"} {
			if r := d.Observe(seqEnv(src, "market.equity.trade", "AAPL", s, et, et)); r.Status == integrity.StatusGap {
				t.Fatalf("phantom gap on %s at seq %d", src, s)
			}
		}
	}
}

// --- Staleness under a lagging feed ---------------------------------------

func TestBadData_StalenessTracksAFeedFallingBehind(t *testing.T) {
	m := integrity.NewStalenessMonitor(integrity.DefaultStalenessConfig()) // 10s / 60s

	// A feed whose ingestion lag grows 0→120s over the run: fresh, then
	// stale, then critical. event_time advances 1s/event; ingestion_time
	// advances faster so lag accumulates.
	var fresh, stale, critical int
	for i := 0; i < 120; i++ {
		et := badBase.Add(time.Duration(i) * time.Second)
		lag := time.Duration(i) * time.Second // 0s..119s
		r := m.Observe(seqEnv("md/pod-1", "market.equity.quote", "MSFT", 0, et, et.Add(lag)))
		switch r.Level {
		case integrity.StalenessFresh:
			fresh++
		case integrity.StalenessStale:
			stale++
		case integrity.StalenessCritical:
			critical++
		default:
			t.Fatalf("unexpected level %s at lag %s", r.Level, lag)
		}
	}

	// Boundaries are exclusive-above: lag<=10s fresh (0..10 ⇒ 11), 10<lag<=60
	// stale (11..60 ⇒ 50), lag>60 critical (61..119 ⇒ 59).
	if fresh != 11 || stale != 50 || critical != 59 {
		t.Errorf("fresh=%d stale=%d critical=%d want 11/50/59", fresh, stale, critical)
	}
}

func TestBadData_OutOfOrderArrivalsDoNotRewindStaleFrontier(t *testing.T) {
	m := integrity.NewStalenessMonitor(integrity.DefaultStalenessConfig())

	// Advance the frontier to a recent event_time...
	recent := badBase.Add(time.Hour)
	m.Observe(seqEnv("md/pod-1", "market.equity.trade", "AAPL", 0, recent, recent))

	// ...then a flood of much older (out-of-order) events. They are stale,
	// but the reported frontier must stay at the recent event_time.
	for i := 0; i < 20; i++ {
		old := badBase.Add(time.Duration(i) * time.Second)
		r := m.Observe(seqEnv("md/pod-1", "market.equity.trade", "AAPL", 0, old, recent))
		if !r.LastEventTime.Equal(recent) {
			t.Fatalf("frontier rewound to %v on out-of-order event", r.LastEventTime)
		}
	}
}

// --- Drift under a distribution shift -------------------------------------

// psiConfig: 4 bins over [0,1,2] edges, uniform baseline (centre-heavy
// distribution generated to match it).
func newDriftDetector(t *testing.T) *integrity.PSIDetector {
	t.Helper()
	d, err := integrity.NewPSIDetector(integrity.PSIConfig{
		Feature:    "spread_bps",
		Edges:      []float64{0, 1, 2},
		Baseline:   []float64{1, 1, 1, 1}, // uniform across 4 bins
		MinSamples: 100,
	})
	if err != nil {
		t.Fatalf("NewPSIDetector: %v", err)
	}
	return d
}

func TestBadData_DriftQuietOnBaselineDistributionThenAlarmsOnShift(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	ws, we := badBase, badBase.Add(time.Hour)

	// Window 1: samples drawn ~uniformly across the 4 bins (matches the
	// uniform baseline). One representative value per bin (edges 0,1,2),
	// bin chosen uniformly, so bin proportions ≈ 25% each ⇒ PSI ≈ 0.
	binValue := []float64{-0.5, 0.5, 1.5, 2.5}
	d := newDriftDetector(t)
	for i := 0; i < 4000; i++ {
		d.Observe(binValue[rng.Intn(4)])
	}
	clean := d.Assess(ws, we)
	if !clean.Sufficient {
		t.Fatal("baseline window not sufficient")
	}
	if clean.Drifted {
		t.Errorf("baseline-matching window flagged drift: score %.4f", clean.Score)
	}

	// Window 2: distribution collapses into the top bin (a feed that started
	// returning a stuck/scaled value). Must drift.
	d.Reset()
	for i := 0; i < 4000; i++ {
		d.Observe(5.0) // all in the > 2 bin
	}
	shifted := d.Assess(ws, we)
	if !shifted.Drifted {
		t.Errorf("collapsed distribution not flagged: score %.4f threshold %.4f",
			shifted.Score, shifted.Threshold)
	}
	if shifted.Score <= clean.Score {
		t.Errorf("shifted score %.4f not greater than clean %.4f", shifted.Score, clean.Score)
	}
}

func TestBadData_DriftWithheldUntilEnoughSamples(t *testing.T) {
	d := newDriftDetector(t) // MinSamples=100
	ws, we := badBase, badBase.Add(time.Minute)

	// A strongly-shifted but tiny window: high score, but too few samples to
	// trust — must not alarm (PSI on a handful of points is noise).
	for i := 0; i < 20; i++ {
		d.Observe(5.0)
	}
	r := d.Assess(ws, we)
	if r.Sufficient {
		t.Fatal("20 samples reported sufficient against MinSamples=100")
	}
	if r.Drifted {
		t.Errorf("under-sampled window flagged drift despite score %.4f", r.Score)
	}
}
