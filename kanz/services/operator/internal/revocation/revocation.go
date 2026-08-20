// Package revocation holds the ORDER in which a tenant is taken off this
// platform, and nothing else (SOV-04, #108).
//
// # What this package is, and what it deliberately is not
//
// Revoking a tenant on Kubernetes means three things happen to it: trading is
// halted, its workloads are scaled to zero, and its Vault-CSI secrets are
// purged. Two of those are Kubernetes API calls and the third is a bus publish.
// NONE OF THE THREE IS IMPLEMENTED HERE. What is implemented is the sequence
// they must run in, the point at which a failure must stop, and the state a
// failed revocation is allowed to leave behind.
//
// That split is the whole point. The three verbs are mechanical and their
// correctness is only observable against a live cluster, which this repository
// has never had reachable from a test. That absence is not new and it is not
// this issue's: #90 and #92 are both open on "verify against a real cluster",
// and #110 declined to enable sharding on the same ground — every link was
// verified statically and the end-to-end behaviour needed a running fleet
// nobody could start. The ORDER is not like that: it is a safety property, it
// costs real money to get wrong, and it is decidable with no cluster at all. So
// it is settled and tested here, BEFORE the mechanical half exists to be
// misused.
//
// # Why halt strictly precedes everything
//
// Scaling a tenant's OMS to zero while orders are live at a venue abandons them
// mid-flight. The exchange still holds the working orders; the process that
// knew about them no longer exists; nothing is left to cancel or reconcile them
// and no operator action inside this platform can reach them any more. The
// position keeps moving with the market and the platform has stopped watching.
//
// Halting first is what makes the rest survivable: with the gate closed no new
// order is emitted, so the set of orders live at the venue stops growing and
// becomes a set someone can work through. This is why Revoke refuses to scale
// anything when the halt did not succeed — see the abort rule below.
//
// # Why nothing here rolls back
//
// Revoke has no compensation path, on purpose. If the scale or the purge fails,
// the tenant is left HALTED — brake engaged, trading stopped, partially
// dismantled — and Revoke says so. It does not resume trading to "restore" the
// tenant.
//
// Resuming would take a tenant whose secrets may already be gone, or whose
// workloads may be half down, and let it trade again. That is the worst of the
// available end states: a tenant nobody has decided is fit to trade, trading,
// with a revocation record saying it was stopped. A halted, half-dismantled
// tenant is inert — it loses money to nothing, and every remaining step is
// still available to an operator. The Estate interface therefore has no undo
// verb at all, which TestEstateExposesNoUndoVerb enforces so a later "helpful"
// rollback cannot be added without arguing with this paragraph.
//
// # Status
//
// THIS PACKAGE DOES NOT REVOKE A TENANT. Wired against NotImplementedEstate it
// fails at the first stage, loudly, and reports Revoked=false — which is the
// correct answer today and the reason the stubs return an error rather than
// nil. It has NO PRODUCTION CALLER: the operator control plane serves no revoke
// RPC, and adding one is #108's remaining half, blocked on the same missing
// cluster as the verbs.
package revocation

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Request is one tenant revocation. Every field is required; see Validate.
type Request struct {
	// Tenant is the tenant id, as it appears in infra/nats/tenancy.yaml and in
	// the per-tenant compute manifest under infra/deploy/tenants/<tenant>/.
	Tenant string
	// By is the operator principal, "{type}:{id}" (e.g. operator:akif). It is
	// the same attribution cmd/kanz-halt demands, for the same reason: taking a
	// tenant off the platform is an attributable act or it is not a record.
	By string
	// Reason is why the tenant is being revoked. Recorded, never defaulted.
	Reason string
}

// Validate rejects an under-specified request BEFORE any stage runs.
//
// It fails here rather than at the first stage that happens to notice, because
// a revocation that gets as far as halting a tenant and only then discovers it
// has no attributable principal has already stopped that tenant's trading with
// nothing to justify it.
func (r Request) Validate() error {
	switch {
	case strings.TrimSpace(r.Tenant) == "":
		return errors.New("tenant is required: a revocation with no subject would halt something unnamed")
	case strings.TrimSpace(r.By) == "":
		return errors.New(`by is required: use "{type}:{id}" (e.g. operator:akif) — a revocation must be attributable`)
	case !strings.Contains(r.By, ":"):
		return fmt.Errorf(`by %q is not a principal: use "{type}:{id}", e.g. operator:akif`, r.By)
	case strings.TrimSpace(r.Reason) == "":
		return errors.New("reason is required: an unexplained revocation is not an audit record")
	}
	return nil
}

// Stage names one step of a revocation. The declaration order IS the execution
// order, and Revoke is the only thing that may run them.
type Stage int

const (
	// StageNone is the zero value: no stage. Result.FailedAt carries it when
	// nothing failed.
	StageNone Stage = iota
	// StageHalt stops the tenant trading. It runs FIRST, always.
	StageHalt
	// StageScaleToZero takes the tenant's workloads to zero replicas.
	StageScaleToZero
	// StagePurgeSecrets removes the tenant's mounted credential material.
	StagePurgeSecrets
)

func (s Stage) String() string {
	switch s {
	case StageNone:
		return "none"
	case StageHalt:
		return "halt"
	case StageScaleToZero:
		return "scale-to-zero"
	case StagePurgeSecrets:
		return "purge-secrets"
	default:
		return fmt.Sprintf("stage(%d)", int(s))
	}
}

// Halter stops a tenant trading. It is the in-band brake, and it is the only
// stage whose failure aborts the revocation.
//
// The brake this describes already exists as a binary: cmd/kanz-halt publishes
// the lifecycle.v1.ModeChanged FACT on platform.mode.changed that the shared
// halt.Gate folds. What does NOT exist is a per-tenant one — read the
// --tenant flag's own help text in cmd/kanz-halt/main.go: "AUDIT/ROUTING ONLY.
// The halt is PLATFORM-WIDE: the gate ignores this field and stops every
// tenant's execution. There is no per-tenant halt today." An implementation of
// this interface must either publish a signal something actually folds
// per-tenant, or be honest that it stops everyone.
type Halter interface {
	HaltTrading(ctx context.Context, req Request) error
}

// Scaler takes a tenant's workloads to zero replicas.
//
// The target is not a namespace: internal/tenantgen renders a tenant's compute
// into the SHARED kanz-services namespace as objects named <name>-<tenant> and
// labelled tenant: <tenant>. So "scale the tenant to zero" is a scale of the
// Deployments matching that label selector, and an implementation that scaled a
// namespace would take every tenant down with it.
type Scaler interface {
	ScaleToZero(ctx context.Context, req Request) error
}

// Purger removes a tenant's mounted credential material — the Vault-CSI
// SecretProviderClass objects and the Secrets shadowing them on the dev rig.
//
// It runs LAST because it is the least reversible of the three and because a
// workload that is already at zero cannot notice its secrets leaving. Purging
// before scaling would instead crash-loop live pods against missing mounts,
// which looks identical to an outage and is much harder to read.
type Purger interface {
	PurgeSecrets(ctx context.Context, req Request) error
}

// Estate is the estate a revocation acts on: the three verbs, and nothing else.
//
// THERE IS DELIBERATELY NO UNDO VERB. See the package doc — a partial
// revocation must be left halted, not resumed. TestEstateExposesNoUndoVerb
// fails if a Resume/Rollback/Restore-shaped method is ever added here.
type Estate interface {
	Halter
	Scaler
	Purger
}

// Result is what a revocation attempt actually did. It is returned on the error
// path too, and on that path it is the more useful half: it names the state the
// tenant was left in.
type Result struct {
	// Done lists the stages that COMPLETED, in the order they ran.
	Done []Stage
	// FailedAt is the stage that returned an error, or StageNone if none did.
	FailedAt Stage
	// BrakeEngaged reports whether trading was stopped AND left stopped. It is
	// false only when the halt itself failed — nothing in this package ever
	// takes the brake back off.
	BrakeEngaged bool
	// Revoked is true only when all three stages completed. A caller that reads
	// nothing else must read this: a Result with BrakeEngaged=true and
	// Revoked=false is a tenant that is stopped but still deployed.
	Revoked bool
}

// String is the operator-facing summary. It states the END STATE rather than
// the failure, because the end state is what the next person has to act on.
func (r Result) String() string {
	done := make([]string, 0, len(r.Done))
	for _, s := range r.Done {
		done = append(done, s.String())
	}
	completed := "none"
	if len(done) > 0 {
		completed = strings.Join(done, ",")
	}
	switch {
	case r.Revoked:
		return fmt.Sprintf("REVOKED — completed %s; brake ENGAGED", completed)
	case r.BrakeEngaged:
		return fmt.Sprintf("PARTIALLY REVOKED — completed %s, failed at %s; brake ENGAGED, "+
			"tenant is stopped but still deployed. Do NOT resume it to tidy up: finish the "+
			"revocation or leave it halted", completed, r.FailedAt)
	default:
		return fmt.Sprintf("NOT REVOKED — failed at %s; brake NOT engaged, the tenant may still "+
			"be trading and nothing was dismantled", r.FailedAt)
	}
}

// Revoke runs a tenant revocation in the only order that is safe: halt, then
// scale to zero, then purge secrets.
//
// THE ABORT RULE: a failed halt stops the revocation dead. Nothing is scaled and
// nothing is purged, because a tenant that could not be stopped must not be
// dismantled around its own live orders — that is the sequence that abandons
// working orders at a venue with no process left to reconcile them.
//
// THE NO-ROLLBACK RULE: a failure at any LATER stage leaves the brake engaged
// and returns. See the package doc for why resuming would be the worse end
// state.
//
// A nil error means all three stages completed. Result.Revoked says the same
// thing and is the field to assert on: a caller that checks only the error of
// some future partial variant would read "answered" as "succeeded".
func Revoke(ctx context.Context, estate Estate, req Request) (Result, error) {
	var res Result
	if estate == nil {
		return res, errors.New("revoke: no estate: a nil estate would report a revocation that " +
			"touched nothing")
	}
	if err := req.Validate(); err != nil {
		return res, fmt.Errorf("revoke: %w", err)
	}

	// STAGE 1 — the brake. Everything after this is conditional on it.
	if err := ctx.Err(); err != nil {
		res.FailedAt = StageHalt
		return res, fmt.Errorf("revoke tenant %q: %s: %w", req.Tenant, StageHalt, err)
	}
	if err := estate.HaltTrading(ctx, req); err != nil {
		res.FailedAt = StageHalt
		return res, fmt.Errorf("revoke tenant %q: %s failed, nothing was scaled or purged: %w",
			req.Tenant, StageHalt, err)
	}
	res.Done = append(res.Done, StageHalt)
	res.BrakeEngaged = true

	// From here the brake stays on whatever happens. Each remaining stage is
	// attempted only if the context is still live, so a deadline expiring
	// mid-revocation stops at a stage boundary rather than half-way through a
	// Kubernetes call.
	for _, step := range []struct {
		stage Stage
		run   func(context.Context, Request) error
	}{
		{StageScaleToZero, estate.ScaleToZero},
		{StagePurgeSecrets, estate.PurgeSecrets},
	} {
		if err := ctx.Err(); err != nil {
			res.FailedAt = step.stage
			return res, fmt.Errorf("revoke tenant %q: %s: %w", req.Tenant, step.stage, err)
		}
		if err := step.run(ctx, req); err != nil {
			res.FailedAt = step.stage
			return res, fmt.Errorf("revoke tenant %q: %s failed; the tenant is left HALTED and "+
				"is NOT resumed: %w", req.Tenant, step.stage, err)
		}
		res.Done = append(res.Done, step.stage)
	}

	res.Revoked = true
	return res, nil
}
