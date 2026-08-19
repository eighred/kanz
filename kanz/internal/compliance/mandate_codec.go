package compliance

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	lifecyclepb "github.com/eighred/kanz/kanz-schemas-go/lifecycle/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/eighred/kanz/internal/platform/subject"
)

// Compliance-domain subjects and event types. A mandate change is carried as a
// lifecycle.v1.ConfigChanged FACT (COMP-01f) on SubjectMandateChanged; both the
// post-trade monitor and the OMS pre-trade gate subscribe to it, so they share
// one source of truth for which mandate applies. The passive-breach FACT
// (COMP-01d) flows on SubjectBreach.
const (
	Domain = "compliance"

	// MandateConfigKeyPrefix scopes the ConfigChanged keys that carry mandates,
	// so a consumer can tell a mandate change from any other platform config on
	// the same stream.
	MandateConfigKeyPrefix = "compliance.mandate."

	SubjectMandateChanged   = "compliance.mandate.changed"
	EventTypeMandateChanged = "compliance.mandate.changed"

	SubjectBreach   = "compliance.breach.detected"
	EventTypeBreach = "compliance.breach.detected"
)

// MandateConfigKey is the ConfigChanged.config_key for a portfolio's mandate —
// stable per (tenant, portfolio) so the platform.config topic compacts to the
// latest mandate per portfolio while each event still carries its version.
func MandateConfigKey(tenantID, portfolioID string) string {
	return MandateConfigKeyPrefix + tenantID + "/" + portfolioID
}

// SubjectMandateAll is the wildcard every mandate consumer subscribes to.
const SubjectMandateAll = SubjectMandateChanged + ".>"

// SubjectMandateFor is the subject ONE portfolio's mandate rides on.
//
// # A mandate is STATE, not an event, and its subject has to say so
//
// The comment above says the mandate stream "compacts to the latest mandate per
// portfolio". It could not: every portfolio's mandate was published to ONE flat
// subject, and JetStream compacts PER SUBJECT. So the stream was an append-only
// log of changes with no way to ask "what governs portfolio P right now" other
// than replaying all of history — which nothing did.
//
// What that cost: the mandate registry is in-memory and is filled by a durable
// consumer, which resumes at its last ack. A RESTARTED OMS therefore came back with
// an EMPTY registry and its PRE-TRADE COMPLIANCE GATE PASSED EVERY ORDER — the
// control did not fail, it disarmed, silently, on every rolling update (EXEC-M13).
//
// One subject per (tenant, portfolio) makes the stream a compacted CURRENT-STATE
// store: MaxMsgsPerSubject=1 keeps exactly the mandate in force, forever, and a
// booting consumer replays DeliverLastPerSubject to arm itself with all of them.
// State that a control depends on must be recoverable in one read, not reconstructed
// from a history nobody keeps.
func SubjectMandateFor(tenantID, portfolioID string) string {
	return SubjectMandateChanged + "." + subjectToken(tenantID) + "." + subjectToken(portfolioID)
}

// subjectToken is subject.Token, promoted to internal/platform/subject on the
// second-consumer trigger (positions now build a subject the same way — EXEC-M20).
func subjectToken(s string) string { return subject.Token(s) }

// MarshalMandateValue renders a Mandate as the canonical serialized value stored
// in ConfigChanged.new_value. protojson (not binary) keeps the audited value
// human-readable and diffable, which an auditor of "what changed" wants.
func MarshalMandateValue(m *compliancepb.Mandate) (string, error) {
	b, err := protojson.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// UnmarshalMandateValue parses a ConfigChanged.new_value back into a Mandate.
func UnmarshalMandateValue(s string) (*compliancepb.Mandate, error) {
	var m compliancepb.Mandate
	if err := protojson.Unmarshal([]byte(s), &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// MandateLoader applies mandate ConfigChanged FACTs into a MandateRegistry — the
// shared decode-and-store step both services run so a mandate's full version
// history (and thus point-in-time resolution) is reconstructed from the stream.
type MandateLoader struct{ reg *MandateRegistry }

// NewMandateLoader wraps a registry.
func NewMandateLoader(reg *MandateRegistry) *MandateLoader { return &MandateLoader{reg: reg} }

// Apply decodes a ConfigChanged. It ignores config keys outside the mandate
// namespace (returns nil, nil), so it is safe to point at a shared config
// stream. On a mandate key it parses and stores the version, returning it.
func (l *MandateLoader) Apply(cc *lifecyclepb.ConfigChanged) (*compliancepb.Mandate, error) {
	if !strings.HasPrefix(cc.GetConfigKey(), MandateConfigKeyPrefix) {
		return nil, nil
	}
	m, err := UnmarshalMandateValue(cc.GetNewValue())
	if err != nil {
		return nil, err
	}
	// Put's refusal is PROPAGATED, not swallowed. A mandate that names no tenant
	// cannot be filed under (tenant, portfolio) at all, and a registry that
	// dropped it quietly would leave the portfolio reading UNGOVERNED — which
	// under OMS_REQUIRE_MANDATE=false is admitted with no constraints, not
	// refused (#243).
	if err := l.reg.Put(m); err != nil {
		return nil, err
	}
	return m, nil
}

// MandateConsumer is the bus handler that feeds a MandateRegistry from the
// mandate ConfigChanged stream — the shared subscription both the OMS pre-trade
// gate and the post-trade monitor mount, so both resolve against one mandate
// history. Its Handle matches bus.EventHandler.
type MandateConsumer struct {
	loader *MandateLoader
	logger *slog.Logger
}

// NewMandateConsumer wraps a registry (logger defaults to slog.Default()).
func NewMandateConsumer(reg *MandateRegistry, logger *slog.Logger) *MandateConsumer {
	if logger == nil {
		logger = slog.Default()
	}
	return &MandateConsumer{loader: NewMandateLoader(reg), logger: logger}
}

// Handle decodes a lifecycle.v1.ConfigChanged and applies it. A malformed
// payload or unparseable mandate is a permanent defect (poison): logged and
// acked rather than redelivered forever.
func (c *MandateConsumer) Handle(ctx context.Context, _ *envelopepb.Envelope, payload []byte) error {
	var cc lifecyclepb.ConfigChanged
	if err := proto.Unmarshal(payload, &cc); err != nil {
		c.logger.ErrorContext(ctx, "compliance: malformed ConfigChanged", "err", err)
		return nil
	}
	if _, err := c.loader.Apply(&cc); err != nil {
		c.logger.ErrorContext(ctx, "compliance: bad mandate value", "err", err, "config_key", cc.GetConfigKey())
		return nil
	}
	return nil
}

// ErrMandateIncomplete is what ValidateMandate returns. It is a sentinel because
// both publishers of a mandate map it onto a CALLER-FACING refusal — the CLI to
// exit 1 with the message, the compliance API to a 400 — and neither may report
// it as an internal fault, because it is always the operator's input.
var ErrMandateIncomplete = errors.New("compliance: mandate is incomplete")

// ValidateMandate refuses a mandate that must never reach the compacted MANDATE
// stream.
//
// IT VALIDATES BEFORE PUBLISHING, and that ordering is the whole point. The
// stream keeps exactly one message per (tenant, portfolio) subject, so a
// malformed mandate is not a bad event that scrolls past — it is the LAST message
// on that portfolio's subject, and every consumer that boots arms itself with it,
// forever, until somebody publishes a good one.
//
// ONE IMPLEMENTATION, CALLED BY BOTH PUBLISHERS (#562). cmd/kanz-mandate had
// these checks written out in loadMandate, and the compliance service's propose
// route needs the same ones; a second copy is how the two come to accept
// different mandates — the divergence the shared proposal store was promoted to
// end one layer down.
//
// AN EMPTY RULESET IS NOT CHECKED HERE, and that is deliberate rather than an
// omission. It is LEGAL and it MEANS something — this portfolio is governed by a
// mandate that declares no constraints, which is different from having no mandate
// at all — so it is allowed, but each caller says so in the way its own surface
// can: the CLI warns on stderr, the API returns the rule count in its reply.
func ValidateMandate(m *compliancepb.Mandate) error {
	switch {
	case m == nil:
		return fmt.Errorf("%w: no mandate", ErrMandateIncomplete)
	case m.GetTenantId() == "":
		return fmt.Errorf("%w: tenant_id is required — a mandate is filed under (tenant, portfolio), "+
			"and one without a tenant governs nothing", ErrMandateIncomplete)
	case m.GetMandateId() == "":
		return fmt.Errorf("%w: mandate_id is required", ErrMandateIncomplete)
	case m.GetPortfolioId() == "":
		return fmt.Errorf("%w: portfolio_id is required: a mandate governs a portfolio", ErrMandateIncomplete)
	case m.GetVersion() == 0:
		return fmt.Errorf("%w: version is required and monotonic: it is how a consumer orders two "+
			"mandates for the same portfolio", ErrMandateIncomplete)
	case m.GetEffectiveAt() == nil:
		// NEVER DEFAULTED PER CALLER. effective_at is inside the digest via the
		// serialized mandate, so a mandate without one would hash differently in the
		// propose step and the approve step — each stamping its own "now" — and every
		// approval would be refused as a payload change. A control failing for a
		// reason that has nothing to do with the control is worse than one that
		// refuses the input.
		return fmt.Errorf("%w: effective_at is required: it is part of what the approval covers, "+
			"so it cannot be defaulted per-invocation", ErrMandateIncomplete)
	}
	return nil
}
