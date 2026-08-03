package engine_test

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	risk "github.com/eighred/kanz/internal/risk"
	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/engine"
	"github.com/eighred/kanz/internal/risk/state"
)

// #257 at the surface a caller actually reads.
//
// A USD-base portfolio holding one USD and one EUR position used to come
// back as a well-formed MeasuresResponse in ModeNormal with zero quality
// flags, the EUR leg absent from GrossExposure, NetExposure, VaR99 and
// Delta with nothing saying so. The pre-trade gate, the compliance
// monitor and the TUI all read that as the fund's risk. These tests pin
// that the response now declares its own incompleteness.

func applyPositionCcy(t *testing.T, s *state.Store, id, instrument, ccy string, value int64, asOf time.Time) {
	t.Helper()
	ctx := context.Background()
	if err := s.ApplyPortfolioRevalued(ctx,
		&envelopepb.Envelope{EventId: "evt-port-" + id, IdempotencyKey: "evt-port-" + id},
		&domainpb.PortfolioState{PortfolioId: id, BaseCurrency: "USD", AsOf: timestamppb.New(asOf)},
	); err != nil {
		t.Fatalf("ApplyPortfolioRevalued: %v", err)
	}
	key := "evt-" + id + "-" + instrument
	if err := s.ApplyPositionChanged(ctx,
		&envelopepb.Envelope{EventId: key, IdempotencyKey: key},
		&domainpb.PositionState{
			PortfolioId:  id,
			InstrumentId: instrument,
			MarketValue:  money(value, ccy),
			AsOf:         timestamppb.New(asOf),
		}); err != nil {
		t.Fatalf("ApplyPositionChanged: %v", err)
	}
}

func hasFlag(flags []v1.QualityFlag, want v1.QualityFlag) bool {
	for _, f := range flags {
		if f == want {
			return true
		}
	}
	return false
}

func TestMeasures_MixedCurrencyBookIsFlaggedCurrencyExcluded(t *testing.T) {
	e, s := newEngine()
	fresh := time.Now().Add(-1 * time.Second)
	applyPositionCcy(t, s, "PORT-1", "AAPL", "USD", 1000, fresh)
	applyPositionCcy(t, s, "PORT-1", "SAP", "EUR", 9000, fresh)

	resp, err := e.Measures(context.Background(), v1.MeasuresRequest{PortfolioID: "PORT-1"})
	if err != nil {
		t.Fatalf("Measures: %v", err)
	}

	if !hasFlag(resp.QualityFlags, v1.QualityFlagCurrencyExcluded) {
		t.Errorf("QualityFlags=%v, missing %s\n\n"+
			"The EUR leg is in no measure on this response. Without the flag the "+
			"caller cannot distinguish a 1000 gross exposure over the whole book "+
			"from a 1000 gross exposure over one tenth of it, and the error is "+
			"always downward — a concentration limit passes when it should breach (#257).",
			resp.QualityFlags, v1.QualityFlagCurrencyExcluded)
	}

	ex := resp.Set.CurrencyExclusions()
	if len(ex) != 1 || ex[0].InstrumentID != "SAP" || ex[0].Currency != "EUR" {
		t.Errorf("CurrencyExclusions()=%v want the EUR SAP position — the flag says "+
			"something is missing, the exclusions say what", ex)
	}

	// The state is one second old, so nothing else should be flagged: the
	// coverage signal must be its own signal, not an alias for staleness.
	if hasFlag(resp.QualityFlags, v1.QualityFlagStale) || hasFlag(resp.QualityFlags, v1.QualityFlagDegraded) {
		t.Errorf("QualityFlags=%v — fresh state must not be STALE/DEGRADED; "+
			"CURRENCY_EXCLUDED is about coverage, not freshness", resp.QualityFlags)
	}
}

func TestMeasures_SingleCurrencyBookCarriesNoCoverageFlag(t *testing.T) {
	e, s := newEngine()
	fresh := time.Now().Add(-1 * time.Second)
	applyPositionCcy(t, s, "PORT-1", "AAPL", "USD", 1000, fresh)
	applyPositionCcy(t, s, "PORT-1", "MSFT", "USD", 500, fresh)

	resp, err := e.Measures(context.Background(), v1.MeasuresRequest{PortfolioID: "PORT-1"})
	if err != nil {
		t.Fatalf("Measures: %v", err)
	}
	if hasFlag(resp.QualityFlags, v1.QualityFlagCurrencyExcluded) {
		t.Errorf("QualityFlags=%v — a fully measured book must come back clean, or "+
			"the flag is noise and callers learn to ignore it", resp.QualityFlags)
	}
	if ex := resp.Set.CurrencyExclusions(); len(ex) != 0 {
		t.Errorf("CurrencyExclusions()=%v want empty", ex)
	}
}

func TestMeasures_FilteredSubsetKeepsTheCoverageSignal(t *testing.T) {
	// Narrowing WHICH measures are returned does not change which positions
	// went into them. A subset that dropped the exclusions would be a
	// partial number that no longer says it is partial — and asking for
	// just VaR99 is exactly what a gate does.
	e, s := newEngine()
	fresh := time.Now().Add(-1 * time.Second)
	applyPositionCcy(t, s, "PORT-1", "AAPL", "USD", 1000, fresh)
	applyPositionCcy(t, s, "PORT-1", "SAP", "EUR", 9000, fresh)

	resp, err := e.Measures(context.Background(), v1.MeasuresRequest{
		PortfolioID: "PORT-1",
		Measures:    []v1.MeasureName{"VaR99"},
	})
	if err != nil {
		t.Fatalf("Measures: %v", err)
	}
	if !hasFlag(resp.QualityFlags, v1.QualityFlagCurrencyExcluded) {
		t.Errorf("QualityFlags=%v, missing %s on a filtered response",
			resp.QualityFlags, v1.QualityFlagCurrencyExcluded)
	}
	if ex := resp.Set.CurrencyExclusions(); len(ex) != 1 {
		t.Errorf("CurrencyExclusions()=%v want 1 — the narrowed set must carry the "+
			"same coverage record as the full one", ex)
	}
}

func TestEvaluateScenario_MixedCurrencyBookIsFlaggedCurrencyExcluded(t *testing.T) {
	// A scenario projects measures over the same filtered book, so a
	// what-if answer is partial in exactly the same way and must say so.
	e, s := newEngine()
	fresh := time.Now().Add(-1 * time.Second)
	applyPositionCcy(t, s, "PORT-1", "AAPL", "USD", 1000, fresh)
	applyPositionCcy(t, s, "PORT-1", "SAP", "EUR", 9000, fresh)

	resp, err := e.EvaluateScenario(context.Background(), v1.ScenarioRequest{PortfolioID: "PORT-1"})
	if err != nil {
		t.Fatalf("EvaluateScenario: %v", err)
	}
	if !hasFlag(resp.QualityFlags, v1.QualityFlagCurrencyExcluded) {
		t.Errorf("QualityFlags=%v, missing %s on a scenario projection",
			resp.QualityFlags, v1.QualityFlagCurrencyExcluded)
	}
}

func TestMeasures_DegradedCacheReadStillReportsExclusions(t *testing.T) {
	// The cache holds the derived MeasureSet, not the portfolio, so a
	// fallback read has no positions to re-examine. The coverage record
	// rides with the set for exactly this path — otherwise the response
	// that is LEAST trustworthy would be the one that stopped admitting
	// it was partial.
	// Two engines over the SAME cache: the first primes it from real state,
	// the second has an empty store so its read can only be served from the
	// cache — the store-miss branch, without needing to un-apply state.
	cache := risk.NewCache()
	primed := state.NewStore()
	warm := engine.New(primed, compute.DefaultRegistry(), cache, risk.NewDetector())
	fresh := time.Now().Add(-1 * time.Second)
	applyPositionCcy(t, primed, "PORT-1", "AAPL", "USD", 1000, fresh)
	applyPositionCcy(t, primed, "PORT-1", "SAP", "EUR", 9000, fresh)
	if _, err := warm.Measures(context.Background(), v1.MeasuresRequest{PortfolioID: "PORT-1"}); err != nil {
		t.Fatalf("priming Measures: %v", err)
	}

	cold := engine.New(state.NewStore(), compute.DefaultRegistry(), cache, risk.NewDetector())
	resp, err := cold.Measures(context.Background(), v1.MeasuresRequest{PortfolioID: "PORT-1"})
	if err != nil {
		t.Fatalf("cached Measures: %v", err)
	}
	if !hasFlag(resp.QualityFlags, v1.QualityFlagCurrencyExcluded) {
		t.Errorf("QualityFlags=%v, missing %s on a cache-fallback read",
			resp.QualityFlags, v1.QualityFlagCurrencyExcluded)
	}
}
