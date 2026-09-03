package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/services/tv-sync/internal/projection"
)

// BOOT COST WAS PROPORTIONAL TO ALL HISTORY (#809).
//
// EXEC-M21 made a booting pod replay every FACT it had ever folded, because a pod
// roll used to leave a trader looking at an empty account while real positions sat
// open at the exchanges. What that left unbounded is the cost: N facts unmarshalled
// and folded before the pod reports ready, so startup grows with the fund's entire
// history and the outage after each OOM kill is longer than the one before it.
//
// A checkpoint bounds it WITHOUT shortening the horizon —
// `restore(checkpoint) + fold(tail)` reaches the same view as `fold(everything)` —
// which is why this is a checkpoint rather than the retention window #809
// originally proposed. A window would lose every position opened before it and
// report P&L since the window start: EXEC-M21's defect with a shorter horizon.

// checkpointsWritten counts successful checkpoints.
//
// SEEDED AT ZERO by registration, so "this pod has never checkpointed" is a
// readable zero rather than an absent series — and that state is exactly the one
// worth seeing, because it means the next boot pays full replay.
var checkpointsWritten = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "kanz_tvsync_checkpoints_written_total",
	Help: "Fold checkpoints successfully written. Zero after a pod has been folding for a while " +
		"means the next boot replays the fund's entire history (#809).",
})

// checkpointFailures counts checkpoints that could not be written.
//
// A FAILED CHECKPOINT IS NOT A FAILED FOLD. The view is unaffected and the service
// keeps serving; what degrades is the NEXT boot, which falls back to replaying from
// the last good checkpoint — or from the beginning if there is none. That is a
// slow start rather than a wrong book, which is why this is counted and logged
// rather than fatal.
var checkpointFailures = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "kanz_tvsync_checkpoint_failures_total",
	Help: "Fold checkpoints that could not be written. The served view is unaffected; the cost " +
		"lands on the next boot, which replays further back (#809).",
})

// rehydrateSeconds is how long the last boot's rebuild took.
//
// A GAUGE OF THE LAST BOOT, not a histogram: a pod boots once, so a distribution
// over one observation is noise. It is the number that says whether the checkpoint
// is doing its job, and it is the one #809's "Verified when" is written against —
// boot time flat against total history rather than proportional to it.
var rehydrateSeconds = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "kanz_tvsync_rehydrate_seconds",
	Help: "Seconds the last boot spent rebuilding the book. With a checkpoint this is bounded by " +
		"the tail since it was taken; without one it grows with the fund's entire history (#809).",
})

// rehydrateFromCheckpoint reports whether the last boot restored a checkpoint.
//
// 0 IS A LEGITIMATE POSTURE AND A DIAGNOSIS. A fresh deployment has no checkpoint
// and replays everything, which is correct. A pod that has been running for weeks
// and still boots from 0 is a checkpoint loop that is not running, and the boot
// time beside it is the evidence.
var rehydrateFromCheckpoint = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "kanz_tvsync_rehydrate_from_checkpoint",
	Help: "1 when the last boot resumed from a fold checkpoint, 0 when it replayed the whole fact " +
		"log. A long-lived deployment booting at 0 has a checkpoint loop that is not running (#809).",
})

// registerCheckpointMetrics registers the boot-cost series.
//
// CALLED BEFORE THE POSTGRES DIAL, outside any branch that depends on the database
// being reachable. A pod that cannot checkpoint is exactly the pod whose next boot
// is slow, and a rule over an absent series evaluates to nothing — the wiring
// defect #973, #963 and #983 each shipped once.
func registerCheckpointMetrics(r prometheus.Registerer) {
	r.MustRegister(checkpointsWritten, checkpointFailures, rehydrateSeconds, rehydrateFromCheckpoint)
}

// runCheckpoints writes a fold checkpoint on an interval until ctx is done.
//
// IT CHECKPOINTS ONCE MORE ON THE WAY OUT. A graceful shutdown is the cheapest
// possible checkpoint — the fold has stopped, so the state is quiet — and skipping
// it would throw away everything folded since the last tick, which on a long
// interval is most of what the pod did.
//
// IT LOGS AND CONTINUES on error, for the reason the counter's help states: a
// failed checkpoint costs the next boot, not this one, and a loop that exited on
// the first transient error would silently stop checkpointing for the pod's whole
// life.
func runCheckpoints(ctx context.Context, proj *projection.Projection, every time.Duration, logger *slog.Logger) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// A SEPARATE, UNCANCELLED CONTEXT. ctx is already done, so using it here
			// would make the shutdown checkpoint fail every single time — the one
			// checkpoint most likely to matter.
			shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			writeCheckpoint(shutCtx, proj, logger, "shutdown")
			return
		case <-ticker.C:
			writeCheckpoint(ctx, proj, logger, "interval")
		}
	}
}

func writeCheckpoint(ctx context.Context, proj *projection.Projection, logger *slog.Logger, why string) {
	if err := proj.Checkpoint(ctx); err != nil {
		checkpointFailures.Inc()
		logger.Error("could not write the fold checkpoint — the NEXT boot will replay further back, "+
			"the served book is unaffected", "err", err, "trigger", why)
		return
	}
	checkpointsWritten.Inc()
}
