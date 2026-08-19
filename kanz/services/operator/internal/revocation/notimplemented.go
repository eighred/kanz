package revocation

import (
	"context"
	"errors"
	"fmt"
)

// ErrNotImplemented is what every stage of a revocation returns today.
//
// IT MUST NOT BECOME nil. A stub that returned nil would make Revoke walk all
// three stages, set Revoked=true and report a completed revocation of a tenant
// that is still trading, still deployed and still holding its exchange
// credentials — the exact shape this repository's standard forbids: "nothing
// configured" and "checked, and fine" must never look the same. The refusal is
// the feature; these types exist so the orchestrator has something honest to be
// wired against before the verbs exist.
var ErrNotImplemented = errors.New("not implemented on this estate")

// NotImplementedHalter refuses to halt a tenant.
//
// WHY IT IS A STUB AND NOT A CALL INTO cmd/kanz-halt: the brake exists, but it
// is PLATFORM-WIDE. kanz-halt publishes ModeChanged{component: "system"}, and
// its own --tenant flag documents that "the gate ignores this field and stops
// every tenant's execution. There is no per-tenant halt today." Shelling out to
// it here would halt every tenant on the platform to revoke one of them, and
// would do it under a Result that says the revocation was orderly. Building a
// per-tenant gate is a change to the signal contract (internal/platform/mode
// plus internal/signal/translate), not to this file.
type NotImplementedHalter struct{}

func (NotImplementedHalter) HaltTrading(_ context.Context, req Request) error {
	return fmt.Errorf("halt trading for tenant %q: %w: the brake is cmd/kanz-halt and it is "+
		"PLATFORM-WIDE (lifecycle.v1.ModeChanged{component: %q}) — there is no per-tenant halt, "+
		"so no implementation can stop this tenant alone. An operator must run kanz-halt by hand, "+
		"knowing it stops everyone (#108)", req.Tenant, ErrNotImplemented, "system")
}

// NotImplementedScaler refuses to scale a tenant's workloads to zero.
//
// DATED EVIDENCE: on 2026-08-19, before this package existed, `grep -rln
// "Scale\|Replicas" services/operator/internal/` returned NOTHING — the operator
// had no scale verb of any kind. That grep now matches this file, so re-running
// it proves nothing; what it matches here is the NAME of the verb, not the verb.
//
// The target is known and written down so the implementer does not have to
// guess it: internal/tenantgen renders a tenant's compute into the SHARED
// kanz-services namespace as <name>-<tenant>, labelled tenant: <tenant>. The
// verb is a scale of the Deployments matching that label — NOT a namespace
// operation, which would take every other tenant down with it. It is unbuilt
// because its effect is only observable on a cluster, and none is reachable
// here — the same missing cluster #90 and #92 are open on.
type NotImplementedScaler struct{}

func (NotImplementedScaler) ScaleToZero(_ context.Context, req Request) error {
	return fmt.Errorf("scale tenant %q to zero: %w: the operator has no scale verb — the target is "+
		"the Deployments labelled tenant=%s in the shared kanz-services namespace "+
		"(internal/tenantgen), and its effect is only observable on a cluster (#108)",
		req.Tenant, ErrNotImplemented, req.Tenant)
}

// NotImplementedPurger refuses to purge a tenant's mounted credential material.
//
// The target is the tenant's SecretProviderClass objects
// (infra/security/secrets/secretproviderclass.yaml) and, on the dev rig, the
// plain Secrets that shadow them (infra/deploy/rig-dev-secrets.yaml). Unbuilt
// for the same reason as the scaler, with the extra hazard that a purge is the
// one stage of a revocation that cannot be undone by re-running it.
type NotImplementedPurger struct{}

func (NotImplementedPurger) PurgeSecrets(_ context.Context, req Request) error {
	return fmt.Errorf("purge secrets for tenant %q: %w: no purge verb exists in the operator — "+
		"services/operator/internal/secrets is write-only (SetVenueKeys/ListVenues) and has no "+
		"delete. The target is the tenant's SecretProviderClass and the rig Secrets shadowing it, "+
		"and this is the one stage that re-running cannot undo (#108)", req.Tenant, ErrNotImplemented)
}

// NotImplementedEstate is the whole estate, unbuilt. Revoke over it fails at
// StageHalt and reports Revoked=false, BrakeEngaged=false — a truthful "this
// platform cannot revoke a tenant yet" rather than a silent success.
type NotImplementedEstate struct {
	NotImplementedHalter
	NotImplementedScaler
	NotImplementedPurger
}

// Compile-time proof that each stub satisfies exactly the verb it names, and
// that the composite is a whole Estate. Without these a renamed interface
// method would leave the stubs orphaned and the package would still build.
var (
	_ Halter = NotImplementedHalter{}
	_ Scaler = NotImplementedScaler{}
	_ Purger = NotImplementedPurger{}
	_ Estate = NotImplementedEstate{}
)
