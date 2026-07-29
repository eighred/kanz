package compliance

import (
	"context"
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
	l.reg.Put(m)
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
