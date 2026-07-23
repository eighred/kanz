package app

import (
	"log/slog"

	"github.com/kanz-eng/kanz/internal/risk/engine"
	"github.com/kanz-eng/kanz/internal/risk/state"
)

// ArmPostBootstrapRecomputes triggers exactly one debounced recompute per
// portfolio that Bootstrap.Run just restored, and returns how many it armed.
//
// # The gap this closes
//
// TriggeringApplier's Trigger only sets an in-memory debounce deadline
// (engine.Recomputer.Trigger); the actual recompute — the thing that emits
// the pushed risk.* FACT — fires ~DefaultDebounceInterval later on the
// worker goroutine. Ingestor.Handler, meanwhile, returns nil (acking the bus
// delivery) the instant the state mutation commits, not when the FACT is
// emitted. If the process crashes in that window, the bus event is already
// acked — nothing redelivers it — and the debounced recompute that would
// have produced the FACT never ran. Bootstrap's restore+replay repopulates
// the STATE (via LoadAll + the durable log), so the next live event for that
// portfolio will recompute correctly, but until then the pushed FACT for the
// change that was in flight at crash time is simply missing.
//
// # Why this is not the storm Bootstrap's replay avoids
//
// Bootstrap deliberately replays through the bare state.Store, not a
// TriggeringApplier (see Bootstrap's doc comment): triggering per REPLAYED
// EVENT would emit a recompute — and a risk FACT — for every intermediate
// state a portfolio passed through on its way to "current," most of them
// superseded before they could matter. That trade is still correct. This
// function is a different shape of work: one Trigger per PORTFOLIO, called
// once, after restore+replay has already settled every portfolio to its
// current state. There is no burst to coalesce and no intermediate FACT to
// avoid — it produces exactly the single settled FACT a crash mid-debounce
// would otherwise have swallowed.
//
// # Why a free function, not a Bootstrap method or field
//
// Bootstrap must stay ignorant of the recomputer, full stop — that
// ignorance is what keeps replay side-effect-free (see its doc comment).
// Giving Bootstrap a recomputer, even an optional one, would make "does
// replay trigger recomputes" a runtime configuration question instead of an
// invariant the type system enforces. This function is called by the
// composition root (cmd/risk-engine/main.go) immediately after boot.Run
// succeeds, where both store and recomputer are already in scope.
func ArmPostBootstrapRecomputes(store *state.Store, recomputer *engine.Recomputer, logger *slog.Logger) int {
	if logger == nil {
		logger = slog.Default()
	}
	ids := store.IDs()
	for _, id := range ids {
		recomputer.Trigger(id)
	}
	logger.Info("bootstrap: armed post-replay recomputes", "portfolios", len(ids))
	return len(ids)
}
