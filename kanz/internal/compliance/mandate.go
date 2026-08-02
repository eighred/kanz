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

// MandateRegistry is an in-memory, point-in-time store of mandate versions per
// (tenant, portfolio) — the COMP-01f resolution core. Each mandate change is
// appended as a new version with its effective_at; resolution returns the version
// in effect at a query time (the latest version whose effective_at ≤ asOf). The
// compliance service loads it by replaying the lifecycle.v1.ConfigChanged FACTs
// that carry each serialized Mandate, so "which mandate applied when" is
// reconstructable.
//
// It satisfies MandateSource, so the pre-trade gate resolves through it directly.
type MandateRegistry struct {
	mu sync.RWMutex
	// byKey holds versions sorted ascending by (effective_at, version).
	byKey map[mandateKey][]*compliancepb.Mandate
	// tenantsByPortfolio names every tenant holding a mandate for a portfolio id.
	// It exists so a lookup that misses can say WHY — "nobody wrote one" and "one
	// exists, filed under another tenant" are different operator problems and must
	// not read the same in a log.
	tenantsByPortfolio map[string]map[string]struct{}
	armed              bool
	onceOn             sync.Once

	// warnMu guards warned only; it is separate from mu so the resolution path
	// never takes a write lock to say something.
	warnMu sync.Mutex
	warned map[string]bool
	logger *slog.Logger
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
		warned:             make(map[string]bool),
		logger:             slog.Default(),
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
	if r.tenantsByPortfolio[k.portfolio] == nil {
		r.tenantsByPortfolio[k.portfolio] = make(map[string]struct{})
	}
	r.tenantsByPortfolio[k.portfolio][k.tenant] = struct{}{}

	vers := r.byKey[k]
	for i, v := range vers {
		if v.GetVersion() == m.GetVersion() {
			vers[i] = m // idempotent replace
			r.byKey[k] = vers
			return nil
		}
	}
	vers = append(vers, m)
	sort.Slice(vers, func(i, j int) bool {
		ti, tj := vers[i].GetEffectiveAt().AsTime(), vers[j].GetEffectiveAt().AsTime()
		if !ti.Equal(tj) {
			return ti.Before(tj)
		}
		return vers[i].GetVersion() < vers[j].GetVersion()
	})
	r.byKey[k] = vers
	return nil
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
func (r *MandateRegistry) Mandate(_ context.Context, tenantID, portfolioID string, asOf time.Time) (*compliancepb.Mandate, bool, error) {
	if tenantID == "" {
		// An untenanted lookup cannot be scoped at all. Terminal, not transient:
		// the caller lost the envelope's tenant somewhere, and no retry restores it.
		return nil, false, fmt.Errorf("%w: no tenant supplied for portfolio %q — the caller "+
			"dropped the envelope's tenant_id, and any mandate returned here would be a guess",
			ErrMandateTenantUnresolved, portfolioID)
	}

	r.mu.RLock()
	vers := r.byKey[mandateKey{tenant: tenantID, portfolio: portfolioID}]
	others := sortedTenants(r.tenantsByPortfolio[portfolioID])
	if len(vers) == 0 && tenantID == bus.SystemTenant && len(others) == 1 {
		vers = r.byKey[mandateKey{tenant: others[0], portfolio: portfolioID}]
	}
	r.mu.RUnlock()

	if len(vers) == 0 {
		if tenantID == bus.SystemTenant && len(others) > 1 {
			return nil, false, fmt.Errorf("%w: portfolio %q is under mandate for tenants %v and this "+
				"lookup is the shared %q bucket — two tenants named a portfolio the same, and picking "+
				"one of their mandates for the other's book is the defect, not the fix. Give this "+
				"consumer the real tenant (#97), or rename one portfolio",
				ErrMandateTenantUnresolved, portfolioID, others, bus.SystemTenant)
		}
		if len(others) > 0 {
			r.warnMisfiled(tenantID, portfolioID, others)
		}
		return nil, false, nil
	}
	if tenantID == bus.SystemTenant && len(others) == 1 && others[0] != bus.SystemTenant {
		r.warnSystemFallback(portfolioID, others[0])
	}

	var chosen *compliancepb.Mandate
	for _, v := range vers {
		eff := v.GetEffectiveAt().AsTime()
		if asOf.IsZero() || !eff.After(asOf) {
			chosen = v // versions are ascending, so the last match wins
		}
	}
	if chosen == nil {
		return nil, false, nil
	}
	return chosen, true, nil
}

// warnOnce reports whether this is the first time key has been named. The
// resolution path runs on every order and every position tick; a diagnosis
// repeated per event is a log nobody reads.
func (r *MandateRegistry) warnOnce(key string) bool {
	r.warnMu.Lock()
	defer r.warnMu.Unlock()
	if r.warned[key] {
		return false
	}
	r.warned[key] = true
	return true
}

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
