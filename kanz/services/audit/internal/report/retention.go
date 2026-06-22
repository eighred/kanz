package report

import (
	"time"

	"github.com/kanz-eng/kanz/services/audit/internal/audit"
)

// RetentionPolicy is the audit-log retention rule. Retain is the minimum age a
// record must reach before it is even eligible for purge; a zero Retain means
// "keep forever" (the safe default — regulatory logs err toward over-retention).
// Actual deletion is governed by WORM/object-lock at the storage layer
// (AUDIT-01b): this policy decides eligibility, the storage enforces that
// nothing inside the window or under hold can be removed.
type RetentionPolicy struct {
	Retain time.Duration `json:"retain"`
}

// LegalHold pins records from purge regardless of age — a litigation/regulatory
// hold. A record is held if its correlation_id or tenant_id matches. Holds
// override retention: a held record is never purge-eligible.
type LegalHold struct {
	Reason       string   `json:"reason"`
	Correlations []string `json:"correlations,omitempty"`
	Tenants      []string `json:"tenants,omitempty"`
}

// PurgeDecision is the outcome of evaluating retention + holds over a record set.
type PurgeDecision struct {
	Eligible []*audit.Record // past retention and not held — may be purged
	Held     []*audit.Record // past retention but pinned by a legal hold
	Retained int             // still within the retention window
}

// EvaluatePurge classifies records against the policy and holds at time now. It
// is pure (no mutation, no deletion) — the audit log is append-only/WORM, so
// this produces the decision an operator or a storage-lifecycle job acts on, and
// makes the "why is this still here / why was this removable" question
// answerable. A zero Retain keeps everything (all Retained).
func EvaluatePurge(records []*audit.Record, policy RetentionPolicy, holds []LegalHold, now time.Time) PurgeDecision {
	var d PurgeDecision
	heldCorr, heldTenant := indexHolds(holds)
	for _, r := range records {
		if policy.Retain <= 0 || now.Sub(r.RecordedAt) < policy.Retain {
			d.Retained++
			continue
		}
		if heldCorr[r.CorrelationID] || heldTenant[r.TenantID] {
			d.Held = append(d.Held, r)
			continue
		}
		d.Eligible = append(d.Eligible, r)
	}
	return d
}

func indexHolds(holds []LegalHold) (corr, tenant map[string]bool) {
	corr, tenant = map[string]bool{}, map[string]bool{}
	for _, h := range holds {
		for _, c := range h.Correlations {
			corr[c] = true
		}
		for _, t := range h.Tenants {
			tenant[t] = true
		}
	}
	return corr, tenant
}
