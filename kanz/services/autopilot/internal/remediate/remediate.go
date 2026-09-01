// Package remediate is the autopilot's data-plane remediation (AUTO-01b): data
// quarantine on a DATA-05 reconciliation divergence (or a critical gap), and
// model auto-rollback on drift — MLOPS-01e promotion run in reverse. The actions
// are decoupled from any concrete client behind Quarantiner / ModelRoller seams
// (the repo's "inject the side-effect, log by default" stance, as DEBT-02): the
// default impls log, and a deployment wires the real command publisher (a
// quarantine COMMAND on the subject's domain, a model-rollback COMMAND on
// platform.model).
//
// # NEITHER DEFAULT IMPL KEEPS A LEDGER FOR A TEST TO READ (#892)
//
// Both of them used to. LogQuarantiner held `set map[string]string` and
// LogModelRoller held `rolled map[string]string` — no cap, no TTL, no eviction —
// and their keys come off the wire: for KindDataGap, KindStaleness and KindDrift
// the signal's Subject is `dqe.GetSubject()` read straight off a
// DataQualityEvent (signal/classify.go), with no check against a bounded
// universe, and RollbackAction falls back to that same subject when the
// `model_id` attribute is absent. So the fill rate peaked during exactly the
// degraded period the autopilot exists to handle — a data-quality storm wrote
// one permanent entry per distinct subject in it — which is when an OOM kill of
// the remediation controller costs the most and explains the least.
//
// THE RULE APPLIED HERE, ONCE, IS "RETAIN ONLY WHAT PRODUCTION READS", and it
// gives the two impls different answers because production reads different
// amounts of them:
//
//   - LogModelRoller.rolled was read by NOTHING on the live path. Its only
//     reader was RolledBack(), whose only callers were assertions. So it is
//     deleted outright, and the mutex went with it — the #844 repair of
//     LogEscalator.seen, same argument, same file neighbourhood.
//   - LogQuarantiner.set WAS read on the live path, by Quarantine itself, to say
//     it once per subject. That behaviour is kept; the unbounded memory it stood
//     on is not. See LogQuarantiner.
//
// Nothing could ever have read either map from outside the process anyway:
// autopilot serves /healthz, /readyz and /metrics and nothing else
// (internal/server/server.go), so there was no surface an operator could reach
// them through.
package remediate

import (
	"context"
	"log/slog"
	"sync"

	"github.com/eighred/kanz/services/autopilot/internal/runbook"
	"github.com/eighred/kanz/services/autopilot/internal/signal"
)

// Quarantiner halts processing of a data subject so divergent/bad data stops
// propagating into derived state until a human clears it.
//
// IT ASKS FOR NO LEDGER, AND THAT IS THE POINT OF #892. The interface used to
// carry `Quarantined(subject string) bool`, documented "(idempotency +
// inspection)". No caller on the live path ever asked it — the only callers were
// assertions — but every implementation had to answer it, and the only way to
// answer it is to remember every subject ever quarantined, keyed by a string off
// a DataQualityEvent. The leak was in the CONTRACT, not in one implementation:
// deleting the map and leaving the method would have left the next Quarantiner —
// including the real command-publishing one a deployment wires — to grow the
// same map again to satisfy the same unread question.
//
// It could not have survived the bound either. Once the say-it-once set is a
// window rather than a complete history, a bool answer for an evicted subject is
// "not quarantined" about a subject that IS quarantined — a fabricated
// "checked, and fine" where the truth is unknown, which is the one thing this
// platform's state model is not allowed to say.
type Quarantiner interface {
	Quarantine(ctx context.Context, subject, reason string) error
}

// ModelRoller reverts a model to its last-known-good version — the MLOPS-01e
// promotion gate run backwards.
type ModelRoller interface {
	Rollback(ctx context.Context, modelID, reason string) error
}

// QuarantineAction wraps a Quarantiner as a runbook step. Idempotent: the
// default impl only logs, and a real one publishes a quarantine COMMAND naming
// the desired end state, so a re-quarantine of an already-quarantined subject
// converges rather than compounding.
func QuarantineAction(q Quarantiner) runbook.Action {
	return runbook.Action{
		Name: "quarantine_subject",
		Run: func(ctx context.Context, s signal.Signal) error {
			return q.Quarantine(ctx, s.Subject, string(s.Kind)+": "+s.Summary)
		},
	}
}

// RollbackAction wraps a ModelRoller as a runbook step. The model id is the
// signal subject (drift's subject is the model), or the `model_id` attribute.
func RollbackAction(r ModelRoller) runbook.Action {
	return runbook.Action{
		Name: "model_rollback",
		Run: func(ctx context.Context, s signal.Signal) error {
			modelID := s.Attr("model_id")
			if modelID == "" {
				modelID = s.Subject
			}
			return r.Rollback(ctx, modelID, string(s.Kind)+": "+s.Summary)
		},
	}
}

// maxQuarantineWarnings is the ceiling on subjects one LogQuarantiner remembers
// having already warned about.
//
// THE NUMBER IS TAKEN, NOT INVENTED. internal/risk/state's dedupWindow —
// defaultDedupMax — is this estate's own figure for an in-process seen-set whose
// count ceiling exists for MEMORY PROTECTION rather than to size a working set,
// and internal/compliance's say-it-once ledger takes its ceiling the same way
// from internal/refdata. A set that exists only to suppress duplicate LOG LINES
// on a controller that handles one signal at a time has no claim on more than
// the estate's dedup windows, so it takes the same number and not a byte more.
//
// IT IS A BACKSTOP AND NOT THE WORKING BOUND. The working set is the data
// subjects that are quarantined right now — a handful on a healthy platform,
// and at worst the traded book during a feed outage. At roughly 48 bytes an
// entry (the key held twice, once in the map and once in the FIFO) the ceiling
// costs well under a megabyte, so it is cheap to set far above the working set
// and let it stay unreached.
const maxQuarantineWarnings = 10_000

// LogQuarantiner is the default Quarantiner: it logs the quarantine, once per
// subject. A real deployment swaps in a command-publishing impl.
//
// # What it retains and why that is not the #892 leak
//
// The warn line is the only production record that names the SUBJECT — the
// kanz_autopilot_outcomes_total counter beside it (controller/metrics.go) is
// labelled by condition and outcome, so it says a data gap was remediated and
// how often, never which instrument — and it has always been emitted ONCE per
// subject rather than once per signal. That
// suppression is a live read of retained state, so unlike LogEscalator (#844)
// and unlike LogModelRoller below, this type cannot simply drop its map without
// changing what the autopilot does. What it can drop — and did — is the
// unboundedness and the reason string held per subject.
//
// So `warned` is a fixed-capacity FIFO of subjects rather than a complete
// history. Under any workload with fewer than maxQuarantineWarnings distinct
// subjects, which is every non-pathological one, the emitted lines are exactly
// what they were before #892. Past that the oldest subject is evicted and
// re-warns if it arrives again.
//
// EVICTION IS THE RIGHT DIRECTION TO FAIL, and the alternatives are both worse:
// remembering forever is the leak this replaces, and dropping the suppression
// entirely turns a data-quality storm's memory growth into a log flood, which is
// a trade this repair is not allowed to make. Being told a second time about a
// subject that has not been mentioned in the last 10,000 quarantines is a
// reminder, not noise — internal/compliance's firstAbout reaches the same
// conclusion about the same trade.
//
// EVICTION IS BY INSERTION AND A REPEAT SIGHTING DOES NOT REFRESH IT, which is
// deliberate: a sighting that finds the subject present emits no line and so
// tells an operator nothing, and refreshing on it would keep a chatty subject
// resident at the cost of one that has been quarantined and silent all day.
type LogQuarantiner struct {
	logger *slog.Logger
	mu     sync.Mutex
	// warned is the say-it-once set: subjects whose warn line has already been
	// emitted. It holds no reason — nothing read the reasons, and a reason per
	// remembered subject was most of the retained bytes.
	warned map[string]struct{}
	// order is the same subjects in insertion order, so the eviction at the
	// ceiling is O(1) rather than a scan of warned for a victim.
	order []string
}

func NewLogQuarantiner(logger *slog.Logger) *LogQuarantiner {
	if logger == nil {
		logger = slog.Default()
	}
	return &LogQuarantiner{logger: logger, warned: map[string]struct{}{}}
}

func (q *LogQuarantiner) Quarantine(ctx context.Context, subject, reason string) error {
	q.mu.Lock()
	_, already := q.warned[subject]
	if !already {
		q.warned[subject] = struct{}{}
		q.order = append(q.order, subject)
		if len(q.order) > maxQuarantineWarnings {
			delete(q.warned, q.order[0])
			q.order = q.order[1:]
		}
	}
	q.mu.Unlock()
	if !already {
		q.logger.WarnContext(ctx, "autopilot quarantined data subject", "subject", subject, "reason", reason)
	}
	return nil
}

// LogModelRoller is the default ModelRoller: it logs the rollback.
//
// IT RETAINS NOTHING (#892). It used to write `rolled map[string]string` —
// modelID to reason, under a mutex, with no cap and no eviction — keyed by a
// `model_id` attribute that FALLS BACK to the wire subject when absent
// (RollbackAction above), so the real model population was a convention rather
// than a bound. The only reader was a RolledBack() helper that nothing outside a
// test called.
//
// It was deleted rather than capped, exactly as LogEscalator.seen was (#844): a
// bounded ring would have kept a share of the heap cost and a third answer about
// what has been rolled back, while still having no reader. The warn line below
// is the sink production actually has, and it is unaffected. The mutex went with
// the map — a lock over immutable state advertises a protection that is not
// doing anything, and *slog.Logger is safe for concurrent use.
type LogModelRoller struct {
	logger *slog.Logger
}

func NewLogModelRoller(logger *slog.Logger) *LogModelRoller {
	if logger == nil {
		logger = slog.Default()
	}
	return &LogModelRoller{logger: logger}
}

func (r *LogModelRoller) Rollback(ctx context.Context, modelID, reason string) error {
	r.logger.WarnContext(ctx, "autopilot rolled back model", "model_id", modelID, "reason", reason)
	return nil
}
