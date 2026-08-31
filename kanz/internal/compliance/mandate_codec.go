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

// ParseMandateConfigKey is the INVERSE of MandateConfigKey: it recovers the
// (tenant, portfolio) a mandate ConfigChanged was filed under. ok is false for a
// key outside the mandate namespace or one missing either half.
//
// IT EXISTS SO A FAILURE CAN BE ATTRIBUTED. When a mandate cannot be applied, the
// config key is the ONLY thing left that names which portfolio just lost its
// governance — the value did not decode, so the Mandate inside it cannot be asked
// (#619). Without this the drop could only ever be a bare count.
//
// It splits on the FIRST "/", matching MandateConfigKey's construction: a tenant
// id containing a slash would be ambiguous, and is rejected at publish time by
// ValidateMandate rather than guessed at here.
func ParseMandateConfigKey(key string) (tenantID, portfolioID string, ok bool) {
	rest, found := strings.CutPrefix(key, MandateConfigKeyPrefix)
	if !found {
		return "", "", false
	}
	tenantID, portfolioID, found = strings.Cut(rest, "/")
	if !found || tenantID == "" || portfolioID == "" {
		return "", "", false
	}
	return tenantID, portfolioID, true
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
// shared decode-and-store step both services run so both resolve against the same
// mandate.
//
// IT DOES NOT RECONSTRUCT A HISTORY, which is what this said until #884. The
// stream it reads is compacted to one message per (tenant, portfolio) subject
// (SubjectMandateFor), so a replay delivers exactly the mandate in force — a
// consumer that wanted every past version could not get one from here, and the
// registry no longer keeps versions this fold can never re-supply.
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
	loader   *MandateLoader
	reg      *MandateRegistry
	logger   *slog.Logger
	onReject func(tenantID, portfolioID string)
}

// MandateConsumerOption customizes a MandateConsumer.
type MandateConsumerOption func(*MandateConsumer)

// WithMandateRejectionObserver is called for every mandate this consumer could
// not apply, with the portfolio it belonged to.
//
// IT TAKES THE PORTFOLIO because the operator's question is which mandate to go
// and republish, and a bare total cannot answer it. Cardinality is bounded by the
// number of portfolios under mandate — a set this platform already labels metrics
// by — unlike a caller-supplied string.
//
// Nil ⇒ not counted. The loss is still recorded without it: the registry marks the
// portfolio unreadable and the pre-trade gate refuses its orders, so a deployment
// that forgets this seam still cannot trade through the hole.
func WithMandateRejectionObserver(fn func(tenantID, portfolioID string)) MandateConsumerOption {
	return func(c *MandateConsumer) { c.onReject = fn }
}

// NewMandateConsumer wraps a registry (logger defaults to slog.Default()).
func NewMandateConsumer(reg *MandateRegistry, logger *slog.Logger, opts ...MandateConsumerOption) *MandateConsumer {
	if logger == nil {
		logger = slog.Default()
	}
	c := &MandateConsumer{loader: NewMandateLoader(reg), reg: reg, logger: logger}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Handle decodes a lifecycle.v1.ConfigChanged and applies it.
//
// THE ACK IS STILL CORRECT, AND IS NOT WHAT WAS WRONG. Nacking poison on a
// COMPACTED stream replays the identical bytes forever, so a bad mandate would
// wedge the subscription and stop every LATER mandate from arriving — one broken
// portfolio taking the rest down with it. What was wrong is what the ack left
// behind: a log line, and nothing else.
//
// Because the stream is compacted, the message that just failed is the LAST one on
// that portfolio's subject. Every consumer that boots from here on re-reads it and
// fails the same way, so the portfolio is not un-mandated until someone notices —
// it is un-governable until someone REPUBLISHES. That state is now recorded
// against the portfolio, which makes its orders refuse rather than pass (#619).
func (c *MandateConsumer) Handle(ctx context.Context, _ *envelopepb.Envelope, payload []byte) error {
	var cc lifecyclepb.ConfigChanged
	if err := proto.Unmarshal(payload, &cc); err != nil {
		// NO config_key SURVIVES A PAYLOAD THAT DID NOT DECODE, so there is no
		// portfolio to mark — this is the one loss that can only ever be a count.
		// It still has to move the registry off "complete", or the drop is exactly
		// as invisible as it was before.
		c.logger.ErrorContext(ctx, "compliance: malformed ConfigChanged — a mandate message was lost "+
			"and no portfolio can be named for it, because the payload carries no readable config_key",
			"err", err, "bytes", len(payload))
		c.reg.Drop()
		return nil
	}
	if _, err := c.loader.Apply(&cc); err != nil {
		tenantID, portfolioID, ok := ParseMandateConfigKey(cc.GetConfigKey())
		if !ok {
			// The key is outside the mandate namespace or malformed. Apply only
			// errors on a mandate key, so this means the key itself is corrupt.
			c.logger.ErrorContext(ctx, "compliance: a mandate could not be applied and its config_key "+
				"does not name a (tenant, portfolio), so the portfolio it governs cannot be marked",
				"err", err, "config_key", cc.GetConfigKey())
			c.reg.Drop()
			return nil
		}
		c.logger.ErrorContext(ctx, "UNGOVERNABLE: a published mandate could not be applied, and the "+
			"mandate stream is compacted — this portfolio has no readable mandate until one is "+
			"republished, and its orders are now REFUSED rather than admitted",
			"err", err, "tenant_id", tenantID, "portfolio_id", portfolioID,
			"config_key", cc.GetConfigKey(),
			"fix", "republish the mandate with `kanz-mandate --tenant "+tenantID+"`")
		c.reg.Reject(tenantID, portfolioID, err)
		if c.onReject != nil {
			c.onReject(tenantID, portfolioID)
		}
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
