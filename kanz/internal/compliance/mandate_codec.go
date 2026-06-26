package compliance

import (
	"context"
	"log/slog"
	"strings"

	compliancepb "github.com/kanz-eng/kanz-schemas-go/compliance/v1"
	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
	lifecyclepb "github.com/kanz-eng/kanz-schemas-go/lifecycle/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
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
