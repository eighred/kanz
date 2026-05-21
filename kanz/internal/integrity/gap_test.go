package integrity_test

import (
	"sync"
	"testing"

	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"

	"github.com/kanz-eng/kanz/internal/integrity"
)

// env builds a minimal envelope carrying only the fields the gap
// detector keys on.
func env(source, eventType, partitionKey string, seq uint64) *envelopepb.Envelope {
	return &envelopepb.Envelope{
		Source:           source,
		EventType:        eventType,
		PartitionKey:     partitionKey,
		ProducerSequence: seq,
	}
}

func mktEnv(seq uint64) *envelopepb.Envelope {
	return env("market-ingest/inst-1", "market.equity.trade", "AAPL", seq)
}

// --- Baseline + in-order ----------------------------------------------

func TestObserve_FirstSeenEstablishesBaseline(t *testing.T) {
	d := integrity.NewGapDetector()
	r := d.Observe(mktEnv(5))
	if r.Status != integrity.StatusFirstSeen {
		t.Errorf("Status=%v want first_seen", r.Status)
	}
	if r.Sequence != 5 {
		t.Errorf("Sequence=%d want 5", r.Sequence)
	}
	if r.MissingCount() != 0 {
		t.Errorf("MissingCount=%d want 0 (baseline is not a gap)", r.MissingCount())
	}
	// Next contiguous sequence is OK against the baseline of 5.
	if got := d.Observe(mktEnv(6)); got.Status != integrity.StatusOK {
		t.Errorf("Status=%v want ok", got.Status)
	}
}

func TestObserve_ContiguousSequenceIsOK(t *testing.T) {
	d := integrity.NewGapDetector()
	for seq := uint64(1); seq <= 5; seq++ {
		r := d.Observe(mktEnv(seq))
		want := integrity.StatusOK
		if seq == 1 {
			want = integrity.StatusFirstSeen
		}
		if r.Status != want {
			t.Errorf("seq=%d Status=%v want %v", seq, r.Status, want)
		}
	}
}

// --- Gaps -------------------------------------------------------------

func TestObserve_GapReportsMissingRange(t *testing.T) {
	d := integrity.NewGapDetector()
	d.Observe(mktEnv(1))
	r := d.Observe(mktEnv(5)) // 2,3,4 missing
	if r.Status != integrity.StatusGap {
		t.Fatalf("Status=%v want gap", r.Status)
	}
	if r.MissingFrom != 2 || r.MissingTo != 4 {
		t.Errorf("missing range = [%d,%d] want [2,4]", r.MissingFrom, r.MissingTo)
	}
	if r.MissingCount() != 3 {
		t.Errorf("MissingCount=%d want 3", r.MissingCount())
	}
	if r.Previous != 1 || r.Sequence != 5 {
		t.Errorf("Previous=%d Sequence=%d want 1,5", r.Previous, r.Sequence)
	}
}

func TestObserve_SingleMissingSequence(t *testing.T) {
	d := integrity.NewGapDetector()
	d.Observe(mktEnv(10))
	r := d.Observe(mktEnv(12)) // 11 missing
	if r.Status != integrity.StatusGap || r.MissingCount() != 1 {
		t.Errorf("Status=%v MissingCount=%d want gap,1", r.Status, r.MissingCount())
	}
	if r.MissingFrom != 11 || r.MissingTo != 11 {
		t.Errorf("missing = [%d,%d] want [11,11]", r.MissingFrom, r.MissingTo)
	}
}

func TestObserve_GapReportedOnceThenResumesOK(t *testing.T) {
	d := integrity.NewGapDetector()
	d.Observe(mktEnv(1))
	if got := d.Observe(mktEnv(4)); got.Status != integrity.StatusGap {
		t.Fatalf("Status=%v want gap", got.Status)
	}
	// After advancing the high-water mark to 4, 5 is contiguous — the
	// gap is not re-reported.
	if got := d.Observe(mktEnv(5)); got.Status != integrity.StatusOK {
		t.Errorf("Status=%v want ok (gap should not re-report)", got.Status)
	}
}

// --- Duplicates + regressions -----------------------------------------

func TestObserve_DuplicateAtHighWaterMark(t *testing.T) {
	d := integrity.NewGapDetector()
	d.Observe(mktEnv(1))
	d.Observe(mktEnv(2))
	r := d.Observe(mktEnv(2)) // redelivery of 2
	if r.Status != integrity.StatusDuplicate {
		t.Errorf("Status=%v want duplicate", r.Status)
	}
	// Duplicate must not advance the mark — 3 is still OK next.
	if got := d.Observe(mktEnv(3)); got.Status != integrity.StatusOK {
		t.Errorf("Status=%v want ok after duplicate", got.Status)
	}
}

func TestObserve_RegressionBelowHighWaterMark(t *testing.T) {
	d := integrity.NewGapDetector()
	d.Observe(mktEnv(1))
	d.Observe(mktEnv(5)) // gap, mark advances to 5
	r := d.Observe(mktEnv(3))
	if r.Status != integrity.StatusRegression {
		t.Errorf("Status=%v want regression", r.Status)
	}
	if r.Sequence != 3 || r.Previous != 5 {
		t.Errorf("Sequence=%d Previous=%d want 3,5", r.Sequence, r.Previous)
	}
	// The mark stays at 5: 6 is the next contiguous value.
	if got := d.Observe(mktEnv(6)); got.Status != integrity.StatusOK {
		t.Errorf("Status=%v want ok (mark held at 5)", got.Status)
	}
}

// --- Stream isolation -------------------------------------------------

func TestObserve_StreamsKeyedBySourceEventTypeAndPartition(t *testing.T) {
	d := integrity.NewGapDetector()

	// Two sources writing the SAME partition_key each run their own
	// 1-based sequence; interleaving them must NOT manufacture a gap.
	a := env("ingest-a", "market.equity.trade", "AAPL", 1)
	b := env("ingest-b", "market.equity.trade", "AAPL", 1)
	if got := d.Observe(a); got.Status != integrity.StatusFirstSeen {
		t.Errorf("a seq1 Status=%v want first_seen", got.Status)
	}
	if got := d.Observe(b); got.Status != integrity.StatusFirstSeen {
		t.Errorf("b seq1 Status=%v want first_seen (distinct source)", got.Status)
	}
	if got := d.Observe(env("ingest-a", "market.equity.trade", "AAPL", 2)); got.Status != integrity.StatusOK {
		t.Errorf("a seq2 Status=%v want ok", got.Status)
	}

	// Same source + partition, different event_type: independent counter.
	if got := d.Observe(env("ingest-a", "market.equity.quote", "AAPL", 1)); got.Status != integrity.StatusFirstSeen {
		t.Errorf("different event_type Status=%v want first_seen", got.Status)
	}

	// Different partition_key: independent counter.
	if got := d.Observe(env("ingest-a", "market.equity.trade", "MSFT", 1)); got.Status != integrity.StatusFirstSeen {
		t.Errorf("different partition Status=%v want first_seen", got.Status)
	}
}

// --- Not applicable ---------------------------------------------------

func TestObserve_ZeroSequenceIsNotApplicable(t *testing.T) {
	d := integrity.NewGapDetector()
	// seq 0 = producer did not sequence (no partition_key).
	r := d.Observe(env("svc", "platform.config.changed", "", 0))
	if r.Status != integrity.StatusNotApplicable {
		t.Errorf("Status=%v want not_applicable", r.Status)
	}
	// Zero observations never seed state, so a later real sequence on a
	// keyed stream still first-seens cleanly.
	if got := d.Observe(mktEnv(1)); got.Status != integrity.StatusFirstSeen {
		t.Errorf("Status=%v want first_seen", got.Status)
	}
}

func TestObserve_NilEnvelopeIsNotApplicable(t *testing.T) {
	d := integrity.NewGapDetector()
	if got := d.Observe(nil); got.Status != integrity.StatusNotApplicable {
		t.Errorf("Status=%v want not_applicable", got.Status)
	}
}

// --- Concurrency ------------------------------------------------------

func TestObserve_ConcurrentStreamsAreRaceFree(t *testing.T) {
	d := integrity.NewGapDetector()
	const streams, perStream = 8, 200
	var wg sync.WaitGroup
	for s := 0; s < streams; s++ {
		wg.Add(1)
		go func(s int) {
			defer wg.Done()
			source := string(rune('A' + s))
			for seq := uint64(1); seq <= perStream; seq++ {
				d.Observe(env(source, "market.equity.trade", "AAPL", seq))
			}
		}(s)
	}
	wg.Wait()
	// Each stream's next contiguous sequence must be OK — proving state
	// is partitioned per key and no update was lost to a race.
	for s := 0; s < streams; s++ {
		source := string(rune('A' + s))
		r := d.Observe(env(source, "market.equity.trade", "AAPL", perStream+1))
		if r.Status != integrity.StatusOK {
			t.Errorf("stream %s final Status=%v want ok (lost update?)", source, r.Status)
		}
	}
}
