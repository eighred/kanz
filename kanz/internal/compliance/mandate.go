package compliance

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"

	"github.com/eighred/kanz/pkg/bus"
)

// ErrMandateTenantUnresolved means the registry COULD NOT SAY WHOSE RULES APPLY,
// so it evaluated none. It is a TERMINAL condition, not a transient load
// failure: retrying resolves nothing, because the ambiguity is in the data.
//
// The caller must refuse the order. Admitting it would be #243 exactly — the
// registry picking one tenant's mandate for another tenant's order because both
// named their portfolio "growth".
var ErrMandateTenantUnresolved = errors.New("mandate: cannot resolve which tenant's mandate governs this portfolio")

// mandateKey is the registry's key. IT IS COMPOSITE, and that is the whole point
// of #243: portfolio_id is a CALLER-CHOSEN STRING off SubmitOrder, so two tenants
// naming a portfolio "growth" is a coincidence, not an attack. Keyed by portfolio
// alone, both landed in one bucket sorted by (effective_at, version) and the last
// effective one governed BOTH — tenant B's order evaluated against tenant A's
// concentration limits and instrument allow-list. Worse, a version-number
// collision took Put's idempotent-replace branch and OVERWROTE A's mandate
// outright, leaving A ungoverned; with OMS_REQUIRE_MANDATE=false that is
// ADMITTED UNCONSTRAINED, not refused.
type mandateKey struct{ tenant, portfolio string }

// MandateRegistry is an in-memory store of the mandate IN FORCE per
// (tenant, portfolio), plus whatever an operator has SCHEDULED to take force
// later — the COMP-01f resolution core. Resolution returns the latest version
// whose effective_at ≤ asOf, so a mandate published today with a date next month
// governs nothing until then.
//
// # It is CURRENT STATE, not a history, and that is the stream's decision
//
// The MANDATE stream is compacted to one message per (tenant, portfolio) subject
// (infra/nats/bootstrap-job.yaml: `--max-msgs-per-subject=1`), and a booting
// consumer arms from DeliverLastPerSubject. SubjectMandateFor says why in full:
// "state that a control depends on must be recoverable in one read, not
// reconstructed from a history nobody keeps" (EXEC-M13).
//
// So a replica that has just started holds exactly ONE version per key. Nothing
// may depend on a superseded version still being resident, because on every
// rolling restart it is not — and a pre-trade control whose answer depends on
// how long its pod has been up is worse than one with no history at all. The
// doc here used to claim "which mandate applied when" was reconstructable from
// this type; it is not, and #884 is the leak that claim was covering. Which
// mandate permitted an order is answered by the DECISION RECORD, which stamps
// ComplianceResult.mandate_version at decision time (engine.go) and carries it
// into the audit fact — not by asking this registry again afterwards.
//
// It satisfies MandateSource, so the pre-trade gate resolves through it directly.
type MandateRegistry struct {
	mu sync.RWMutex
	// byKey holds versions sorted ascending by (effective_at, version), pruned by
	// Put to the ones a lookup can still choose — see retainSelectable.
	byKey map[mandateKey][]*compliancepb.Mandate
	// tenantsByPortfolio names every tenant holding a mandate for a portfolio id.
	// It exists so a lookup that misses can say WHY — "nobody wrote one" and "one
	// exists, filed under another tenant" are different operator problems and must
	// not read the same in a log.
	tenantsByPortfolio map[string]map[string]struct{}
	// rejected names the (tenant, portfolio) pairs whose mandate arrived and could
	// NOT be applied, with the reason. A rejected key is not an absent one: the
	// mandate stream is compacted, so the message that failed is the LAST one on
	// that portfolio's subject and every consumer that boots re-reads it, forever
	// (#619). Kept so Mandate can answer "unreadable" instead of "not found".
	rejected map[mandateKey]error
	// dropped counts messages that could not be attributed to a portfolio at all —
	// a payload that is not a ConfigChanged names no config_key, so nothing can be
	// marked. It is the reason Complete is not simply len(rejected) == 0.
	dropped int
	armed   bool
	onceOn  sync.Once

	// warned is the registry's say-it-once ledger. It carries its own lock, so
	// the resolution path still never takes mu's write lock to say something —
	// the property the separate warnMu used to provide, now a property of the
	// shared type. Both of its keys are (tenant, portfolio) and only ever formed
	// for a portfolio that ALREADY has a published mandate, so they take
	// sayOnce.first: bounded by the estate, never evicted.
	warned sayOnce
	logger *slog.Logger
	// now is the retention instant Put prunes against. It is a field so a test can
	// place a mandate's effective_at on either side of it deterministically; there
	// is no option to override it in production, because the only honest answer
	// there is the wall clock.
	now func() time.Time
}

// MandateRegistryOption customizes the registry.
type MandateRegistryOption func(*MandateRegistry)

// WithMandateLogger supplies the logger the registry announces tenant-resolution
// anomalies on. Without it the registry falls back to slog.Default() — it never
// resolves an anomaly silently, because a mandate resolved for the wrong tenant
// is invisible everywhere else.
func WithMandateLogger(l *slog.Logger) MandateRegistryOption {
	return func(r *MandateRegistry) {
		if l != nil {
			r.logger = l
		}
	}
}

// NewMandateRegistry returns an empty, UNARMED registry.
func NewMandateRegistry(opts ...MandateRegistryOption) *MandateRegistry {
	r := &MandateRegistry{
		byKey:              make(map[mandateKey][]*compliancepb.Mandate),
		tenantsByPortfolio: make(map[string]map[string]struct{}),
		rejected:           make(map[mandateKey]error),
		logger:             slog.Default(),
		now:                time.Now,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Arm marks the initial mandate replay complete — mirrors PositionCache.Arm
// (services/webhook-ingest/internal/ingest/positions.go), the pattern this
// copies rather than invents.
//
// Both the OMS's pre-trade gate and compliance's post-trade monitor used to call
// readiness.Set(true) the instant the mandate subscription GOROUTINE LAUNCHED,
// not once it had actually folded the mandates in force. Between "pod reports
// Ready" and "the replay has landed" every portfolio resolved as having no
// mandate at all — and OMS_REQUIRE_MANDATE defaults to false, so a portfolio
// with no mandate is not refused, it is ADMITTED UNCONSTRAINED (see the OMS's
// own startup warning). That window is exactly EXEC-M13 — "a restarted OMS
// came back with an empty registry and its gate passed every order" — except
// widened from "only on a broken durable-group replay" to "on every single
// rolling restart, for however long the fold takes". The control did not fail
// loudly; it disarmed silently while the health check said everything was
// fine.
//
// Gating readiness on Arm having run closes that window: the pod stays out of
// its Service (kubelet-wise) or the process withholds readiness.Set(true)
// until the registry can truthfully answer "no mandate" instead of merely
// "no mandate YET". sync.Once-guarded and idempotent, same as PositionCache —
// the bus may call it more than once across resubscribes, and a second call
// must not reopen the question of whether the initial replay landed.
func (r *MandateRegistry) Arm() {
	r.onceOn.Do(func() {
		r.mu.Lock()
		r.armed = true
		r.mu.Unlock()
	})
}

// Armed reports whether the initial mandate replay has folded. Both mains gate
// readiness.Set(true) on this: a pod that has not yet learned which mandates
// are in force must not be handed orders to admit against a registry that
// looks empty but is merely not caught up yet (EXEC-M13).
func (r *MandateRegistry) Armed() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.armed
}

// ErrMandateUnreadable means a mandate for this portfolio WAS PUBLISHED and this
// registry could not apply it.
//
// IT IS TERMINAL, and for a sharper reason than ErrMandateTenantUnresolved: the
// mandate stream is COMPACTED, one message per (tenant, portfolio) subject. The
// message that failed to decode is therefore the last one that subject will ever
// serve until an operator republishes, so a redelivery replays the identical bytes
// and every consumer that boots arms itself with the same failure. Retrying cannot
// resolve it; only a new publish can.
//
// A caller MUST refuse on it, and must refuse REGARDLESS of any require-mandate
// posture — see PreTradeGate and Decision.Unreadable for why this is not a policy
// question (#619).
var ErrMandateUnreadable = errors.New("mandate: a published mandate for this portfolio could not be applied")

// Reject records that a mandate for (tenantID, portfolioID) arrived and could not
// be applied. cause is surfaced in the lookup error, so the operator reading a
// refused order learns what was wrong with the mandate rather than only that one
// was.
func (r *MandateRegistry) Reject(tenantID, portfolioID string, cause error) {
	if tenantID == "" || portfolioID == "" {
		// Nothing to file it under. Counted as an unattributable drop instead, so
		// the registry still stops claiming completeness.
		r.Drop()
		return
	}
	if cause == nil {
		cause = errors.New("unspecified")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rejected[mandateKey{tenant: tenantID, portfolio: portfolioID}] = cause
}

// Drop records a mandate message that could not be attributed to any portfolio —
// a payload that is not a ConfigChanged carries no config_key, so there is no key
// to mark. It exists so an unattributable loss still moves Complete off true.
func (r *MandateRegistry) Drop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dropped++
}

// Dropped is the number of unattributable mandate messages this registry lost.
func (r *MandateRegistry) Dropped() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.dropped
}

// Rejections names every portfolio whose published mandate could not be applied,
// sorted, as "tenant/portfolio" — the list an operator has to go and republish.
func (r *MandateRegistry) Rejections() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.rejected))
	for k := range r.rejected {
		out = append(out, k.tenant+"/"+k.portfolio)
	}
	sort.Strings(out)
	return out
}

// Complete reports that every mandate message this registry saw was applied.
//
// IT IS A DIFFERENT QUESTION FROM Armed. Armed says the initial replay finished;
// Complete says nothing was lost doing it. A registry that folded 499 mandates and
// dropped the 500th is ARMED and NOT COMPLETE, and reporting only the first is how
// a service came to announce Ready over a hole in its own registry (#619).
//
// It deliberately does NOT gate readiness on its own. A single unparseable mandate
// would then take the whole service down and stop the other 499 portfolios trading
// — converting one broken mandate into an estate-wide outage. The per-portfolio
// refusal in the gate is what fails closed, precisely, where the risk actually is;
// this is what the health surface reports so the hole is visible while the rest of
// the estate keeps working.
func (r *MandateRegistry) Complete() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.dropped == 0 && len(r.rejected) == 0
}

// Put inserts a mandate version under (tenant, portfolio), keeping that pair's
// history sorted by (effective_at, version). Re-putting the same version
// replaces it (idempotent replay) — and because the key now carries the tenant,
// two tenants' v1 mandates for the same portfolio name no longer collide there.
//
// A mandate missing either id is REFUSED, not dropped. The publisher already
// requires both (publisher.go: "tenant_id and portfolio_id required"), so one
// arriving without them is a defect upstream — and a silent drop here would
// surface as the portfolio being UNGOVERNED, which with OMS_REQUIRE_MANDATE=false
// means its orders are admitted with no constraints at all. The error travels up
// through MandateLoader.Apply to MandateConsumer.Handle, which logs it against
// the config key.
func (r *MandateRegistry) Put(m *compliancepb.Mandate) error {
	if m == nil {
		return errors.New("mandate: nil mandate")
	}
	if m.GetTenantId() == "" || m.GetPortfolioId() == "" {
		return fmt.Errorf("mandate: tenant_id and portfolio_id required, got tenant=%q portfolio=%q — "+
			"filing it under either alone would govern another tenant's portfolio of the same name (#243), "+
			"and dropping it would leave this one UNGOVERNED",
			m.GetTenantId(), m.GetPortfolioId())
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	k := mandateKey{tenant: m.GetTenantId(), portfolio: m.GetPortfolioId()}
	// A REPAIR CLEARS THE MARK. The operator republishes a corrected mandate on the
	// same compacted subject; if the rejection were sticky the portfolio would stay
	// refused for the life of the process and the repair would be unreachable —
	// a fix worse than the defect it replaced.
	delete(r.rejected, k)
	if r.tenantsByPortfolio[k.portfolio] == nil {
		r.tenantsByPortfolio[k.portfolio] = make(map[string]struct{})
	}
	r.tenantsByPortfolio[k.portfolio][k.tenant] = struct{}{}

	// COPY-ON-WRITE, and it is not defensive tidiness: it is the second half of
	// #1048. This used to mutate the array it had ALREADY PUBLISHED — vers[i] = m
	// in place, then sort.Slice permuting it — and retainSelectable hands the same
	// array straight back on its first <= 0 arm, so the array a reader had escaped
	// with was routinely the array the next Put reordered. Building a private copy
	// means every array this registry has ever published is immutable from the
	// moment it is published: a reader holding a slice header of one holds a
	// stable, correctly sorted sequence forever, whatever it does with the lock.
	//
	// It removes the class rather than the call site. Mandate holding its read lock
	// (below) also closes today's window; this is what keeps it closed when some
	// future reader hands a version slice to a caller, or a goroutine, that outlives
	// the lock. The cost is one 1-3 element allocation per republish, on the
	// operator-driven write path — not on the order path.
	published := r.byKey[k]
	next := make([]*compliancepb.Mandate, len(published), len(published)+1)
	copy(next, published)
	replaced := false
	for i, v := range next {
		if v.GetVersion() == m.GetVersion() {
			next[i] = m // idempotent replace
			replaced = true
			break
		}
	}
	if !replaced {
		next = append(next, m)
	}
	// THE RE-SORT RUNS ON THE REPLACE PATH TOO, and that is not tidiness. A
	// republished version carries its own effective_at, and an operator correcting
	// a mandate can move it — the digest the two-signature flow covers includes
	// effective_at precisely because it is allowed to change. Writing it in place
	// and returning, which is what this did, left the slice out of the order both
	// Mandate's "the last match wins" scan and retainSelectable below depend on.
	sort.Slice(next, func(i, j int) bool {
		ti, tj := next[i].GetEffectiveAt().AsTime(), next[j].GetEffectiveAt().AsTime()
		if !ti.Equal(tj) {
			return ti.Before(tj)
		}
		return next[i].GetVersion() < next[j].GetVersion()
	})
	r.byKey[k] = retainSelectable(next, r.now())
	return nil
}

// retainSelectable drops the versions no lookup at or after asOf can choose
// again, and returns what is left. vers must be sorted ascending by
// (effective_at, version).
//
// # The bound, and why it is derived rather than picked
//
// Mandate resolves to the LAST version whose effective_at ≤ the query time. For
// any query at or after asOf, that can only ever be one of:
//
//   - the single latest version already in force at asOf, or
//   - a version dated LATER than asOf, once its date arrives.
//
// Everything strictly before the first of those is unreachable — not "old", not
// "probably fine to drop": no argument to Mandate at or after asOf selects it.
// So the retained count is 1 + the number of mandate changes an operator has
// SCHEDULED and not yet reached, which is a set the estate deliberately created
// and each member of which still has a day on which it governs. There is no
// number to invent here, and a fixed cap would have had to drop one of those.
//
// A REPUBLISH IS WHAT THIS BOUNDS, and it is the only unbounded input: an
// operator correcting the same portfolio's mandate N times in one uptime left N
// versions resident for the life of the process (#884), all but the last of them
// unselectable. After this they collapse to one.
//
// # What it is allowed to cost, stated so it is not discovered later
//
// A query with asOf BEFORE the retention instant can now miss a version this
// process once held. Nothing on the estate makes one: the OMS pre-trade gate,
// the optimization proposal route and the monitor's cash/sweep path all pass
// their own now(). The one backdated argument is the monitor's position-FACT
// path, which passes the FACT's as_of — and on a replayed FACT that read already
// cannot find a superseded version today, because the compacted stream gave the
// booting replica exactly one. This makes a long-lived replica answer as a
// freshly started one does, which is the direction that removes a divergence
// rather than adding one.
//
// The result is a fresh slice rather than a reslice: vers[i:] keeps the whole
// backing array alive, so the dropped versions would remain reachable and the
// leak would survive with a shorter len.
//
// # vers MUST be the caller's PRIVATE array, and the early return is why
//
// The first <= 0 arm returns vers ITSELF, so whatever the caller passed becomes
// the published array. Put therefore hands it a copy it made this call and never
// the array it read out of byKey (#1048): passing the published array here is
// what made "the reader's array" and "the writer's array" the same object, and
// the aliasing is invisible at this call — it shows up as a resolution selecting
// a mandate that is not in force, one goroutine away.
func retainSelectable(vers []*compliancepb.Mandate, asOf time.Time) []*compliancepb.Mandate {
	first := -1
	for i := len(vers) - 1; i >= 0; i-- {
		if !vers[i].GetEffectiveAt().AsTime().After(asOf) {
			first = i
			break
		}
	}
	if first <= 0 {
		// first == 0: already minimal. first == -1: EVERY version is future-dated,
		// so none of them is superseded and all are kept. Dropping here would leave
		// the portfolio with no mandate at all until the earliest date arrived —
		// UNGOVERNED, which under OMS_REQUIRE_MANDATE=false is admitted with no
		// constraints rather than refused.
		return vers
	}
	kept := make([]*compliancepb.Mandate, len(vers)-first)
	copy(kept, vers[first:])
	return kept
}

// TenantsGoverning names every tenant that has published a mandate for
// portfolioID, sorted. More than one means two tenants have independently chosen
// the same portfolio name — legal, and the reason the key is composite.
func (r *MandateRegistry) TenantsGoverning(portfolioID string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return sortedTenants(r.tenantsByPortfolio[portfolioID])
}

func sortedTenants(set map[string]struct{}) []string {
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for t := range set {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// Mandate resolves the mandate version in effect for (tenantID, portfolioID) at
// asOf. A zero asOf resolves to the latest version. ok=false when no version is
// in effect, which the gate treats as UNGOVERNED. A non-nil error wrapping
// ErrMandateTenantUnresolved is TERMINAL — the caller must refuse, never admit.
//
// # Why this is not a plain composite-map read, and why that is not a fudge
//
// The lookup tenant is the ORDER'S (or the position FACT's) tenant, off the
// envelope. On the deployment that exists today that is not always the tenant
// the mandate was filed under, and a strict read would find nothing where the
// old portfolio-only map found something — silently turning governed portfolios
// UNGOVERNED, which with OMS_REQUIRE_MANDATE=false is "admitted unconstrained".
// That is the same trap bus.RequireTenantScope had to avoid, and it is avoided
// the same way: by asking whether the lookup is scoped to a real tenant.
//
//   - tenantID is a REAL tenant and has a mandate for the portfolio — resolved
//     from it. Nothing else is ever consulted, so a second tenant's "growth"
//     mandate cannot reach this order. This is the case #243 is about.
//
//   - tenantID is a real tenant and has NO mandate for the portfolio, but
//     another tenant does — UNGOVERNED, and said out loud naming who does hold
//     one. This is the ONE case whose answer changes from the old behaviour, and
//     it changes to the truthful one: a mandate filed under __system__ is not
//     acme's mandate. The fix is one kanz-mandate run with the right --tenant,
//     and the warning says so rather than leaving an operator to infer it from a
//     portfolio that quietly stopped being checked.
//
//   - tenantID is SystemTenant — the SHARED BUCKET, and the deployment that
//     exists today: the OMS position projector stamps OMS_TENANT="__system__" on
//     every position FACT, so the post-trade monitor's every lookup arrives this
//     way while mandates are published under a customer's tenant. __system__ is
//     not a customer, so a strict read here would disarm the monitor entirely.
//     With exactly one tenant holding a mandate for the portfolio the answer is
//     unambiguous and it is returned — identical to today. With TWO it is not,
//     and guessing is precisely the defect, so it is refused
//     (ErrMandateTenantUnresolved) rather than silently decided.
//
// The middle and last branches are therefore inert on a single-tenant estate and
// load-bearing the moment #97 provisions a second one — which is when nobody is
// re-reading this file.
func (r *MandateRegistry) Mandate(_ context.Context, tenantID, portfolioID string, asOf time.Time) (*compliancepb.Mandate, Governance, error) {
	if tenantID == "" {
		// An untenanted lookup cannot be scoped at all. Terminal, not transient:
		// the caller lost the envelope's tenant somewhere, and no retry restores it.
		return nil, GovernanceUnspecified, fmt.Errorf("%w: no tenant supplied for portfolio %q — the caller "+
			"dropped the envelope's tenant_id, and any mandate returned here would be a guess",
			ErrMandateTenantUnresolved, portfolioID)
	}

	r.mu.RLock()
	// ASKED BEFORE THE VERSIONS, because a rejected key HAS no versions and would
	// otherwise fall out of the len(vers) == 0 branch below as a plain miss — which
	// is the collapse this exists to stop: "nobody wrote a mandate" and "a mandate
	// was written and this registry could not read it" are different facts, and only
	// the first is a posture question (#619).
	if rejErr := r.rejected[mandateKey{tenant: tenantID, portfolio: portfolioID}]; rejErr != nil {
		r.mu.RUnlock()
		return nil, GovernanceUnspecified, fmt.Errorf("%w: portfolio %q of tenant %q has a published mandate this "+
			"registry could not apply (%v). The mandate stream is compacted, so that message is the "+
			"LAST one on this portfolio's subject and every consumer that boots re-reads it: the "+
			"portfolio is not merely un-mandated, it is un-governable until the mandate is "+
			"republished", ErrMandateUnreadable, portfolioID, tenantID, rejErr)
	}
	vers := r.byKey[mandateKey{tenant: tenantID, portfolio: portfolioID}]
	others := sortedTenants(r.tenantsByPortfolio[portfolioID])
	if len(vers) == 0 && tenantID == bus.SystemTenant && len(others) == 1 {
		vers = r.byKey[mandateKey{tenant: others[0], portfolio: portfolioID}]
	}
	// THE SELECTION SCAN RUNS UNDER THE READ LOCK, and that is the whole of #1048.
	// It used to run after the unlock, walking the backing array while Put — which
	// holds the WRITE lock, and was therefore believed to be safe — reordered that
	// same array in place. Holding the write lock buys nothing against a reader
	// that is no longer holding anything.
	//
	// The consequence was not merely the data race. This scan's only claim to
	// correctness is "versions are ascending, so the last match wins"; a reader
	// walking a transiently unsorted sequence selects a mandate that is NOT in
	// force, and the gate then evaluates the order against the wrong limits, the
	// wrong restricted list, the wrong leverage cap — and stamps that version into
	// ComplianceResult.mandate_version as the authority that permitted it (see the
	// type doc above). The audit trail comes out internally consistent and wrong.
	//
	// The cost is nil: a bounded walk over 1 + the scheduled changes (retainSelectable
	// bounds it), no I/O, no allocation. Five sibling methods on this type already
	// hold their lock across the read; this one was the deviation, not the pattern.
	var chosen *compliancepb.Mandate
	for _, v := range vers {
		eff := v.GetEffectiveAt().AsTime()
		if asOf.IsZero() || !eff.After(asOf) {
			chosen = v // versions are ascending, so the last match wins
		}
	}
	resident := len(vers)
	r.mu.RUnlock()
	// Nothing below reads guarded state again: `others` is a fresh slice out of
	// sortedTenants, `resident` is a copy, and `chosen` is a pointer to a mandate
	// Put only ever REPLACES, never mutates. The warnings stay outside the lock so
	// the resolution path never logs while holding it.

	if resident == 0 {
		if tenantID == bus.SystemTenant && len(others) > 1 {
			return nil, GovernanceUnspecified, fmt.Errorf("%w: portfolio %q is under mandate for tenants %v and this "+
				"lookup is the shared %q bucket — two tenants named a portfolio the same, and picking "+
				"one of their mandates for the other's book is the defect, not the fix. Give this "+
				"consumer the real tenant (#97), or rename one portfolio",
				ErrMandateTenantUnresolved, portfolioID, others, bus.SystemTenant)
		}
		if len(others) > 0 {
			r.warnMisfiled(tenantID, portfolioID, others)
		}
		// NOBODY HAS DECIDED what governs this portfolio (#926).
		return nil, NeverMandated, nil
	}
	if tenantID == bus.SystemTenant && len(others) == 1 && others[0] != bus.SystemTenant {
		r.warnSystemFallback(portfolioID, others[0])
	}

	if chosen == nil {
		// SOMEBODY DECIDED AND THE DECISION IS NOT IN FORCE (#926). Versions exist
		// for this key — the portfolio WAS governed — and none of them is effective
		// at asOf. This is the #916 shape when every surviving version is
		// future-dated, and it must not read as the onboarding state above.
		return nil, MandateLapsed, nil
	}
	return chosen, Governed, nil
}

// warnOnce reports whether this is the first time key has been named. The
// resolution path runs on every order and every position tick; a diagnosis
// repeated per event is a log nobody reads.
func (r *MandateRegistry) warnOnce(key string) bool { return r.warned.first(key) }

// warnMisfiled names the one case whose answer #243 changes: the portfolio IS
// under mandate, just not for the tenant asking. Without this the change reads
// as "the portfolio silently stopped being governed".
func (r *MandateRegistry) warnMisfiled(tenantID, portfolioID string, others []string) {
	if !r.warnOnce("misfiled:" + tenantID + ":" + portfolioID) {
		return
	}
	r.logger.Warn("MANDATE FILED UNDER ANOTHER TENANT — this portfolio is being treated as UNGOVERNED",
		"tenant_id", tenantID,
		"portfolio_id", portfolioID,
		"mandate_held_by", others,
		"fix", "re-publish it with `kanz-mandate --tenant "+tenantID+"`; a mandate filed under a "+
			"different tenant is not this tenant's mandate and must not govern its orders (#243)")
}

// warnSystemFallback records that a resolution took the shared-bucket path. It
// is not an error today — it is how the post-trade monitor resolves anything at
// all while position FACTs carry OMS_TENANT="__system__" — but it is the branch
// that stops working the moment a second tenant reuses a portfolio name, so it
// must be visible before that day rather than after.
func (r *MandateRegistry) warnSystemFallback(portfolioID, holder string) {
	if !r.warnOnce("sysfallback:" + portfolioID) {
		return
	}
	r.logger.Warn("resolved a mandate across tenants: the lookup is the shared __system__ bucket and "+
		"exactly one tenant governs this portfolio",
		"portfolio_id", portfolioID,
		"resolved_from_tenant", holder,
		"consequence", "unambiguous only while ONE tenant uses this portfolio name; a second one makes "+
			"this lookup refuse instead of guessing (#243)",
		"fix", "give this consumer the real tenant — per-tenant compute is #97")
}

var _ MandateSource = (*MandateRegistry)(nil)

// MapBookSource is an in-memory BookSource keyed by portfolio id — the
// composition root populates it from the position read model; tests construct it
// directly.
type MapBookSource map[string]*Book

// Book implements BookSource. A missing portfolio returns an empty book (no
// holdings) rather than an error, so an order for a never-traded portfolio is
// evaluated against an empty book rather than failing transiently.
func (m MapBookSource) Book(_ context.Context, portfolioID string) (*Book, error) {
	if b, ok := m[portfolioID]; ok {
		return b, nil
	}
	return &Book{PortfolioID: portfolioID}, nil
}

var _ BookSource = MapBookSource(nil)
