package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

var covBase = time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)

func cov(bucketMin int, observed time.Duration, attestor string) Coverage {
	return Coverage{
		InstrumentID: "BTC-USDT",
		Venue:        "XBIN",
		Resolution:   Resolution1m,
		BucketStart:  covBase.Add(time.Duration(bucketMin) * time.Minute),
		Observed:     observed,
		Attestor:     attestor,
		RecordedAt:   covBase.Add(time.Hour),
	}
}

func covBar(bucketMin int) Bar {
	return Bar{
		InstrumentID:  "BTC-USDT",
		Venue:         "XBIN",
		Resolution:    Resolution1m,
		BucketStart:   covBase.Add(time.Duration(bucketMin) * time.Minute),
		KnowledgeTime: covBase.Add(time.Hour),
	}
}

// AN OVER-CLAIM IS REFUSED, NOT CLAMPED.
//
// Clamping 90 seconds of "observed" down to the 60 the bucket holds would turn
// an attestor bug into the strongest possible claim about that window — and
// under a reader that gates on Whole(), the strongest claim is the one that lets
// a decision through. Same shape as score.New refusing a probability outside
// [0,1] rather than clamping it (#514).
func TestCoverageRefusesAnObservationLongerThanItsBucket(t *testing.T) {
	err := cov(0, 90*time.Second, "binance:trades:BTCUSDT").Validate()
	if !errors.Is(err, ErrInvalidCoverage) {
		t.Fatalf("Validate() = %v, want ErrInvalidCoverage — an attestor cannot have proven more "+
			"time than the bucket holds", err)
	}
}

// AN UNTRACEABLE CLAIM IS REFUSED. A coverage record is only worth anything
// because it can be held to something; one with no attestor cannot be audited,
// and an unauditable claim about what the platform saw is worse than no claim.
func TestCoverageRefusesAnUnattributedClaim(t *testing.T) {
	c := cov(0, time.Minute, "")
	if err := c.Validate(); !errors.Is(err, ErrInvalidCoverage) {
		t.Fatalf("Validate() = %v, want ErrInvalidCoverage for an empty attestor", err)
	}
	c = cov(0, time.Minute, "binance:trades:BTCUSDT")
	c.Venue = ""
	if err := c.Validate(); !errors.Is(err, ErrInvalidCoverage) {
		t.Fatalf("Validate() = %v, want ErrInvalidCoverage for an empty venue — coverage is per "+
			"venue and a record that did not say which feed it attests vouches for nothing", err)
	}
}

// THE MEMORY STORE MERGES ON max(Observed), NEVER LAST-WRITE-WINS.
//
// Under at-least-once delivery the same attestation arrives twice, and a
// re-publish after a restart can carry a SHORTER claim for a bucket the earlier
// process had already covered in full. Overwriting would silently retract
// coverage that was genuinely proven, turning an observed interval back into an
// unknown one — which is the direction that costs, because unknown is what
// refuses claims.
func TestMemoryCoverageKeepsTheStrongerProof(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	if err := m.PutCoverage(ctx, []Coverage{cov(0, time.Minute, "a")}); err != nil {
		t.Fatal(err)
	}
	if err := m.PutCoverage(ctx, []Coverage{cov(0, 20*time.Second, "a")}); err != nil {
		t.Fatal(err)
	}
	got, err := m.Coverage(ctx, CoverageQuery{InstrumentID: "BTC-USDT", Venue: "XBIN", Resolution: Resolution1m})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	if got[0].Observed != time.Minute {
		t.Fatalf("observed = %s, want 1m — a later, shorter claim must not retract coverage that "+
			"was already proven", got[0].Observed)
	}
}

// TWO ATTESTORS COEXIST. Each can only speak for itself, so collapsing them
// would make the platform's claim whichever pod published second.
func TestMemoryCoverageKeepsEveryAttestorSeparately(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	if err := m.PutCoverage(ctx, []Coverage{cov(0, 20*time.Second, "a"), cov(0, time.Minute, "b")}); err != nil {
		t.Fatal(err)
	}
	if n := m.coverageCount(); n != 2 {
		t.Fatalf("held %d attestations, want 2 — one attestor's claim must not overwrite another's", n)
	}
}

// THE PAYOFF: a whole window and a covered gap are different facts, and an
// UNKNOWN gap is a third.
//
// gaps.go's contract is that a missing bucket proves a window cannot support a
// claim, and a whole window does NOT prove the platform was observing. Attested
// is the other direction, and this is the assertion that it is actually sound:
// the SAME set of bars produces Sound() true or false depending only on whether
// a first-hand attestation exists.
func TestAttestedSeparatesAQuietMarketFromADeadFeed(t *testing.T) {
	from, to := covBase, covBase.Add(5*time.Minute)
	// Four of five minutes traded. Minute 2 is missing.
	bars := []Bar{covBar(0), covBar(1), covBar(3), covBar(4)}

	base, ok := WindowOf(bars, from, to, Resolution1m)
	if !ok {
		t.Fatal("WindowOf refused the window")
	}
	if base.Missing() != 1 {
		t.Fatalf("Missing() = %d, want 1", base.Missing())
	}
	if base.Whole() {
		t.Fatal("Whole() must be false with a bucket missing")
	}

	t.Run("no coverage record at all is UNKNOWN, never quiet", func(t *testing.T) {
		got, ok := AttestedWindowOf(bars, nil, from, to, Resolution1m)
		if !ok {
			t.Fatal("AttestedWindowOf refused the window")
		}
		if got.Unknown != 1 || got.Quiet != 0 {
			t.Fatalf("Unknown=%d Quiet=%d, want 1/0 — with nothing vouching for the interval, the "+
				"gap is unknown; reading it as quiet is exactly the conflation this record ends",
				got.Unknown, got.Quiet)
		}
		if got.Sound() {
			t.Fatal("Sound() must be false — this window cannot support a claim about the missing " +
				"minute, and that is the entire deliverable")
		}
	})

	t.Run("a whole attestation makes the gap a measured quiet", func(t *testing.T) {
		got, ok := AttestedWindowOf(bars, []Coverage{cov(2, time.Minute, "binance:trades:BTCUSDT")},
			from, to, Resolution1m)
		if !ok {
			t.Fatal("AttestedWindowOf refused the window")
		}
		if got.Quiet != 1 || got.Unknown != 0 {
			t.Fatalf("Quiet=%d Unknown=%d, want 1/0 — the subscription was proven live for the "+
				"whole minute, so nothing trading in it is a measurement", got.Quiet, got.Unknown)
		}
		if !got.Sound() {
			t.Fatal("Sound() must be true — every absence in this window is accounted for")
		}
	})

	t.Run("a SHORT attestation is still UNKNOWN, not partial credit", func(t *testing.T) {
		got, _ := AttestedWindowOf(bars, []Coverage{cov(2, 59*time.Second, "binance:trades:BTCUSDT")},
			from, to, Resolution1m)
		if got.Unknown != 1 {
			t.Fatalf("Unknown = %d, want 1 — the record says how MUCH of the bucket was watched, "+
				"not WHICH part, so 59 of 60 seconds cannot rule out a print in the second nobody saw",
				got.Unknown)
		}
	})

	t.Run("one whole attestor is enough even beside a short one", func(t *testing.T) {
		got, _ := AttestedWindowOf(bars, []Coverage{
			cov(2, 10*time.Second, "okx:trades:BTC-USDT"),
			cov(2, time.Minute, "binance:trades:BTCUSDT"),
		}, from, to, Resolution1m)
		if got.Quiet != 1 || got.Unknown != 0 {
			t.Fatalf("Quiet=%d Unknown=%d, want 1/0 — it takes ONE subscription having watched the "+
				"whole bucket for the absence to be explained", got.Quiet, got.Unknown)
		}
	})

	t.Run("coverage for the WRONG bucket explains nothing", func(t *testing.T) {
		got, _ := AttestedWindowOf(bars, []Coverage{cov(3, time.Minute, "binance:trades:BTCUSDT")},
			from, to, Resolution1m)
		if got.Unknown != 1 {
			t.Fatalf("Unknown = %d, want 1 — an attestation for a bucket that HAS a bar does not "+
				"vouch for the one that does not", got.Unknown)
		}
	})

	t.Run("a whole window is Sound without consulting coverage at all", func(t *testing.T) {
		whole := []Bar{covBar(0), covBar(1), covBar(2), covBar(3), covBar(4)}
		got, _ := AttestedWindowOf(whole, nil, from, to, Resolution1m)
		if !got.Sound() || got.Missing() != 0 {
			t.Fatalf("Sound=%v Missing=%d, want true/0", got.Sound(), got.Missing())
		}
	})
}
