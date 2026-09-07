package agentgate

import (
	"context"
	"errors"
	"log/slog"

	"github.com/eighred/kanz/pkg/auth"
)

// THE AGENT-TOOL AUTHORIZATION GATE, STANDING ON ITS OWN (#743).
//
// This is the deny-by-default decision every agent-invoked tool runs before it
// touches any data: resolve which tenant owns the resource, ask the AUTH-01
// authorizer, and render a refusal from a closed set. It is the same decision an
// MCP server would need, which is why #743 names extracting it as the step
// before an MCP surface exists at all — writing a second copy is how a fix stops
// spreading (17 secret() helpers, 15 of them wrong).
//
// IT LIVES HERE BECAUSE A SECOND CONSUMER ARRIVED. It was kept inside
// services/copilot for exactly as long as copilot was its only caller —
// AGENTS.md promotes shared code to internal/ or pkg/ only when a second
// consumer appears, and a package built for a caller nobody has written is the
// speculative abstraction this repository keeps deleting. services/mcp is that
// second consumer, so the promotion happens WITH it rather than ahead of it.
//
// It was made to stand alone before it moved, which is why moving it was a file
// move: it depends on nothing in either service, takes its collaborators as
// narrow interfaces, and its tests construct it directly.
//
// WHAT IT DEPENDS ON, AND WHAT IT DELIBERATELY DOES NOT. It needs an authorizer
// and an answer to "who owns this". It does NOT need the governed read surface —
// it used to hold a whole governed.Client to call one method of it, which tied
// the authorization decision to the copilot's particular data source and is
// exactly the coupling that would have made this unmovable.

// OwnerResolver answers which tenant owns a resource, and returns nothing else
// about it.
//
// RETURNING ONLY THE TENANT IS THE POINT: a caller that is about to be refused
// must not have already been handed the data. The copilot's implementation reads
// an exposure and discards the payload for this reason.
//
// It reports ErrResourceNotVisible when the resource is not visible to this
// caller. That answer MUST be indistinguishable from "no such resource" — see
// Authorize for why the gate then refuses in the same words either way.
type OwnerResolver interface {
	OwnerTenant(ctx context.Context, resourceID string) (string, error)
}

// ErrResourceNotVisible is an OwnerResolver's answer for a resource this caller
// cannot see — whether because it does not exist or because it belongs to
// somebody else. The two are ONE sentinel on purpose: a resolver that
// distinguished them would push a cross-tenant existence oracle into the gate,
// which then could not un-learn it.
var ErrResourceNotVisible = errors.New("agentgate: resource not visible")

// Verdict is the gate's answer: whether the call may proceed, and the text a
// caller may be shown if not.
//
// MESSAGE IS ALREADY SAFE TO RENDER. It is drawn from a closed set keyed on the
// authorizer's deny code, never from Decision.Reason — that reason names the
// resource's OWNING TENANT on a cross-tenant deny, and concatenating it into
// agent-visible content published one tenant's id into another tenant's model
// context (#741). The reason goes to the log and the audit stream instead.
type Verdict struct {
	Allowed bool
	Message string
}

// Gate renders the authorization decision for one agent-invoked tool call.
type Gate struct {
	authz  auth.Authorizer
	owner  OwnerResolver
	logger *slog.Logger
}

// NewGate builds the gate. A nil logger falls back to slog.Default(): the real
// reason for a refusal is written there and NOWHERE the caller can see, so an
// operator asking "why did the agent say there is no data" has that line or
// nothing.
func NewGate(authz auth.Authorizer, owner OwnerResolver, logger *slog.Logger) *Gate {
	if logger == nil {
		logger = slog.Default()
	}
	return &Gate{authz: authz, owner: owner, logger: logger}
}

// Authorize is the deny-by-default gate.
//
// IT NEVER SUBSTITUTES A TENANT IT DOES NOT KNOW. Every error from the resolver
// used to collapse to owner = "", and "" was the exact value auth.Authorize read
// as "this resource is not tenant-scoped, skip the boundary". So a deadline, an
// UNAVAILABLE or a 500 on the ownership lookup removed the cross-tenant guard
// for that invocation, the remaining gates passed on their own terms, and the
// tool went on to read the data — an availability blip on a dependency
// dissolving the estate's strongest boundary (#741).
//
// The unresolved owner is now passed through AS "" and refused by the
// authorizer, so the failure mode is a refusal rather than a bypass, and the
// attempt is still audited because the authorizer still runs.
func (g *Gate) Authorize(ctx context.Context, p *auth.Principal, action auth.Action,
	resourceType, resourceID string) Verdict {

	owner, err := g.owner.OwnerTenant(ctx, resourceID)
	// The authorizer runs on every path, including the ones already destined to
	// refuse: the AUDIT-01 record of an attempt is worth more than the call it
	// costs, and a probe that is never authorized is never audited either.
	dec := g.authz.Authorize(ctx, auth.Request{
		Principal: p,
		Action:    action,
		Resource:  auth.Resource{Type: resourceType, ID: resourceID, Tenant: owner},
	})
	switch {
	case err != nil && !errors.Is(err, ErrResourceNotVisible):
		// Fails closed regardless of what dec said — and dec is a refusal here
		// anyway, because owner is "".
		g.logger.WarnContext(ctx, "agent tool: resource ownership unresolved, refused",
			"resource_type", resourceType, "resource_id", resourceID, "action", string(action), "err", err)
		return Verdict{Message: OwnerUnavailable(resourceType)}
	case !dec.Allow:
		// The authorizer's own sentence goes to the log and the audit stream.
		// The caller gets the closed-set rendering and nothing else.
		g.logger.InfoContext(ctx, "agent tool: denied",
			"resource_type", resourceType, "resource_id", resourceID, "action", string(action),
			"code", string(dec.Code), "reason", dec.Reason)
		return Verdict{Message: refusalFor(dec.Code, resourceType, resourceID)}
	}
	// There is no "authorized, but the resource is unknown" branch below any
	// more, and there cannot be one: ErrResourceNotVisible leaves owner empty,
	// and an empty tenant on a typed resource is a refusal. The unknown resource
	// is answered by refusalFor, in the same words as a cross-tenant one.
	return Verdict{Allowed: true}
}

// Refusal text a caller may read. It is a CLOSED SET chosen by deny code —
// never a passthrough of the authorizer's Reason, which names the resource's
// owning tenant on a cross-tenant deny and once went straight into agent-visible
// content (#741).
//
// THE VOCABULARY IS BUILT FROM THE RESOURCE TYPE rather than hardcoded to one
// noun, because this gate now serves more than one kind of resource. The
// resource type IS the noun — auth.ResourcePortfolio is "portfolio" — so a
// portfolio refusal reads exactly as it did when this lived in copilot, and a
// dataset refusal reads about datasets instead of claiming to be about a
// portfolio.
//
// notVisible deliberately answers TWO different questions with one sentence:
// the resource does not exist, and it belongs to another tenant. A caller able
// to tell those apart can enumerate another tenant's resources by id, which is
// the discovery AGENTS.md puts out of reach. The identical wording is the
// control; keep it identical.
// NotVisible is exported so a consumer can assert what its callers actually
// see without copying the sentence — a duplicated refusal string drifts, and the
// whole control is that two different questions get ONE answer.
func NotVisible(resourceType, resourceID string) string {
	return "no governed data for " + resourceType + " " + resourceID
}

// NotAuthorized is the grant refusal, exported for the same reason.
func NotAuthorized(resourceType string) string {
	return "not authorized to read this " + resourceType
}

// ownerUnavailable is NOT collapsed into notVisible. A failed ownership lookup
// is an outage of the read surface — uncorrelated with any tenant, so stating it
// is not an oracle — and presenting an outage as an absent resource is the
// silent-default this estate refuses.
// OwnerUnavailable is the outage refusal, exported for the same reason.
func OwnerUnavailable(resourceType string) string {
	return "the " + resourceType + "'s owning tenant could not be established, so this read is refused"
}

// refusalFor maps a deny code onto the fixed text the caller may read. The
// isolation refusals collapse into the not-found answer; everything else is
// about the caller's own token and may be stated.
func refusalFor(code auth.DenyCode, resourceType, resourceID string) string {
	if code.IsIsolation() {
		return NotVisible(resourceType, resourceID)
	}
	return NotAuthorized(resourceType)
}
