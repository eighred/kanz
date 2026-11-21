package main

import (
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/factormodel"
)

// newFactorModelProvider returns the ONE provider shared by scalar factor
// measures and vector queries. Extracted from runEngine's shrink-only ratchet;
// the model, return source, cadence, recorder and lookup options are unchanged.
func newFactorModelProvider(universe compute.UniverseFunc, provider factormodel.ReturnsProvider, artifacts *modelRecorder) *compute.LiveModelProvider {
	return compute.NewLiveModelProvider(
		// STATISTICAL, EXPLICITLY: the zero model type is FUNDAMENTAL, whose
		// production industry inputs are absent. PCA needs this existing dated
		// returns source; no new fit policy or input authority is introduced.
		factormodel.Config{Type: factormodel.Statistical},
		universe,
		factormodel.Providers{Returns: provider},
		// Fit daily and record each instance before use; modelrecord.go (#1039).
		artifacts.FitOptions()...,
	)
}
