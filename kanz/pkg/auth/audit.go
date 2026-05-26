package auth

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	observationpb "github.com/kanz-eng/kanz-schemas-go/observation/v1"
)

// Authorization-decision audit (AUTH-01d). Every allow AND deny is recorded as
// an observation.v1.DecisionLog so the "why" of an access decision is
// reconstructable — this is the feed AUDIT-01's append-only projection
// materializes into the queryable audit store.
//
// DecisionLog (OBSERVATION class) is reused rather than a bespoke authz schema:
// it already models "what made a decision, the summary, and the structured
// factors", which is exactly an authz verdict. General observations are
// lossy-tolerant (event-class-rules §4), but an authz audit trail wants
// durability — the recommendation (mirroring the FACT-grade DataQualityEvent
// exception) is to publish these on a durable observation subject and treat
// AUDIT-01's projection as the system of record, NOT to rely on the live stream.
const (
	// DefaultAuthzDecider identifies the policy authorizer as the decider when
	// none is configured; "{type}:{id}" per DecisionLog.decider.
	DefaultAuthzDecider = "authz:policy"

	// AuthzDecisionDomain / AuthzDecisionEventType are the envelope fields a
	// bus-backed recorder stamps so AUDIT-01 can select authz decisions off the
	// observation stream. Exported for that wiring adapter.
	AuthzDecisionDomain    = "platform"
	AuthzDecisionEventType = "platform.authz.decision"
)

// DecisionRecorder persists an authorization DecisionLog. Implementations
// SHOULD be non-blocking (buffered/async): Authorize runs on the request hot
// path, so a recorder that blocks on a bus publish would add per-request
// latency. auth ships SlogRecorder; a bus-backed recorder that publishes the
// DecisionLog as an OBSERVATION event lives in the wiring layer (so auth stays
// decoupled from transport, the AUTH-01c stance).
type DecisionRecorder interface {
	Record(ctx context.Context, entry *observationpb.DecisionLog) error
}

// BuildDecisionLog maps an authorization Request + Decision to a DecisionLog.
// Exported so the bus-backed recorder and tests reuse the one mapping rather
// than re-deriving the attribute keys (the same "one mapping, not two" rule the
// RISK-10 proto converters follow).
func BuildDecisionLog(decider string, req Request, d Decision) *observationpb.DecisionLog {
	if decider == "" {
		decider = DefaultAuthzDecider
	}
	verdict := "deny"
	if d.Allow {
		verdict = "allow"
	}
	attrs := map[string]string{
		"decision": verdict,
		"action":   string(req.Action),
	}
	subject := "anonymous"
	if req.Principal != nil {
		if req.Principal.Subject != "" {
			subject = req.Principal.Subject
			attrs["principal.subject"] = req.Principal.Subject
		}
		if req.Principal.Tenant != "" {
			attrs["principal.tenant"] = req.Principal.Tenant
		}
	}
	if req.Resource.Type != "" {
		attrs["resource.type"] = req.Resource.Type
	}
	if req.Resource.ID != "" {
		attrs["resource.id"] = req.Resource.ID
	}
	if req.Resource.Tenant != "" {
		attrs["resource.tenant"] = req.Resource.Tenant
	}
	if d.Reason != "" {
		attrs["reason"] = d.Reason
	}
	return &observationpb.DecisionLog{
		Decider:    decider,
		Summary:    fmt.Sprintf("%s %s on %s for %s: %s", strings.ToUpper(verdict), req.Action, resourceLabel(req.Resource), subject, d.Reason),
		Attributes: attrs,
	}
}

func resourceLabel(r Resource) string {
	switch {
	case r.Type == "":
		return "(none)"
	case r.ID == "":
		return r.Type
	default:
		return r.Type + ":" + r.ID
	}
}

// AuditedAuthorizer decorates an Authorizer so every decision is recorded.
// Wrap the PolicyAuthorizer with this at the composition root; the deny-by-
// default semantics are unchanged — only observability is added.
type AuditedAuthorizer struct {
	inner    Authorizer
	recorder DecisionRecorder
	decider  string
	logger   *slog.Logger
}

// NewAuditedAuthorizer wraps inner so each Authorize result is sent to recorder.
// A nil recorder disables auditing (the decision still flows). decider defaults
// to DefaultAuthzDecider; logger to slog.Default().
func NewAuditedAuthorizer(inner Authorizer, recorder DecisionRecorder, decider string, logger *slog.Logger) *AuditedAuthorizer {
	if decider == "" {
		decider = DefaultAuthzDecider
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &AuditedAuthorizer{inner: inner, recorder: recorder, decider: decider, logger: logger}
}

var _ Authorizer = (*AuditedAuthorizer)(nil)

// Authorize delegates to the wrapped authorizer, then records the decision.
// Auditing is best-effort: a recorder failure is logged loudly but does NOT
// fail the request — an audit-sink outage must not become a service outage.
// (Fail-closed-on-allow is a noted option for high-compliance deployments.)
func (a *AuditedAuthorizer) Authorize(ctx context.Context, req Request) Decision {
	d := a.inner.Authorize(ctx, req)
	if a.recorder != nil {
		if err := a.recorder.Record(ctx, BuildDecisionLog(a.decider, req, d)); err != nil {
			a.logger.ErrorContext(ctx, "authz audit record failed", "err", err, "allow", d.Allow)
		}
	}
	return d
}

// SlogRecorder records decisions as structured log lines. It is a real audit
// sink, not a stub: kanz logs are stdout JSON shipped by the platform (OBS-01a),
// so a decision logged here lands in the same pipeline. It is also the safe
// default when no bus-backed recorder is wired.
type SlogRecorder struct{ logger *slog.Logger }

// NewSlogRecorder returns a recorder writing to logger (slog.Default() if nil).
func NewSlogRecorder(logger *slog.Logger) *SlogRecorder {
	if logger == nil {
		logger = slog.Default()
	}
	return &SlogRecorder{logger: logger}
}

var _ DecisionRecorder = (*SlogRecorder)(nil)

// Record emits the DecisionLog as a structured log record.
func (s *SlogRecorder) Record(ctx context.Context, e *observationpb.DecisionLog) error {
	s.logger.LogAttrs(ctx, slog.LevelInfo, "authz decision",
		slog.String("decider", e.GetDecider()),
		slog.String("summary", e.GetSummary()),
		slog.Any("attributes", e.GetAttributes()),
	)
	return nil
}
