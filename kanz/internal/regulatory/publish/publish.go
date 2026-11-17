// Package publish records the actual signed filing and its separate context
// chain witness. Neither record is a supervisory submission or human approval.
package publish

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/big"
	"unicode/utf8"

	"github.com/eighred/kanz/internal/audit/chain"
	"github.com/eighred/kanz/internal/audit/signer"
	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/filing"
	riskv1 "github.com/eighred/kanz/internal/risk/api/v1"
	sustainabilitypublish "github.com/eighred/kanz/internal/sustainability/publish"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	regulatorypb "github.com/eighred/kanz/kanz-schemas-go/regulatory/v1"
	sustainabilitypb "github.com/eighred/kanz/kanz-schemas-go/sustainability/v1"
	"github.com/eighred/kanz/pkg/bus"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	RecordedSubject  = "compliance.filing.recorded"
	BindingVersion   = 1
	MaxManifestBytes = 512 << 10
	MaxBodyBytes     = 768 << 10
	bindingDomain    = "kanz.filing-context.sha256.v1\x00"
)

// FilingMethod names the actual delivered computation. These assemblers fit no
// model; unverified request inputs do not acquire a fitted-model identifier.
func FilingMethod(framework string) riskv1.MeasureMethod {
	switch framework {
	case "FRTB":
		return riskv1.MethodFRTBStandardised
	case "TCFD", "SFDR":
		return riskv1.MethodClimateDisclosure
	case "FORM_PF", "AIFMD":
		return riskv1.MethodPortfolioArithmetic
	}
	return ""
}

// Report projects the filed numbers, not a second rendering of the analytic
// values. The existing filing renderer owns their signed decimal strings.
func Report(r filing.Report, id string, provenance *domainpb.MeasureProvenance) (*regulatorypb.RegReport, error) {
	framework, ok := map[string]regulatorypb.Framework{
		"FRTB": regulatorypb.Framework_FRAMEWORK_FRTB, "FORM_PF": regulatorypb.Framework_FRAMEWORK_FORM_PF,
		"AIFMD": regulatorypb.Framework_FRAMEWORK_AIFMD, "TCFD": regulatorypb.Framework_FRAMEWORK_TCFD, "SFDR": regulatorypb.Framework_FRAMEWORK_SFDR,
	}[r.Framework]
	stamp := timestamppb.New(r.AsOf.UTC())
	if !ok || !filing.UUIDv7(id) || stamp.CheckValid() != nil || !filing.Digest(r.Signature) || len(r.LineItems) == 0 || len(r.LineItems) > 256 {
		return nil, errors.New("regulatory publication: incomplete signed report")
	}
	out := &regulatorypb.RegReport{ReportId: id, Framework: framework, AsOf: stamp, Signature: r.Signature}
	seen := make(map[string]bool, len(r.LineItems))
	for _, li := range r.LineItems {
		if !filing.BoundedIdentifier(li.Code) || seen[li.Code] || len(li.Label) > 512 || !utf8.ValidString(li.Label) {
			return nil, errors.New("regulatory publication: invalid line-item identity")
		}
		seen[li.Code] = true
		value, err := filing.FiledDecimal(li)
		if err != nil {
			return nil, err
		}
		line := &regulatorypb.ReportLineItem{Code: li.Code, Label: li.Label, Value: value}
		if provenance != nil {
			line.Provenance = proto.Clone(provenance).(*domainpb.MeasureProvenance)
		}
		out.LineItems = append(out.LineItems, line)
	}
	if framework == regulatorypb.Framework_FRAMEWORK_FRTB {
		charge := &regulatorypb.CapitalCharge{Framework: framework, AsOf: proto.Clone(stamp).(*timestamppb.Timestamp)}
		sum := new(big.Rat)
		for _, line := range out.LineItems {
			if line.Code == "FRTB_TOTAL" {
				charge.Total = proto.Clone(line.Value).(*commonpb.Decimal)
				continue
			}
			amount, valid := dec.FromProtoChecked(line.Value)
			if !valid {
				return nil, errors.New("regulatory publication: invalid capital component")
			}
			sum.Add(sum, amount)
			charge.Components = append(charge.Components, &regulatorypb.CapitalComponent{Name: line.Code, Amount: line.Value})
		}
		total, valid := dec.FromProtoChecked(charge.Total)
		if !valid || charge.Total == nil {
			return nil, errors.New("regulatory publication: filed capital total is missing")
		}
		// Each existing filed row is independently rendered to eight places.
		// Keep all rows in RegReport; do not invent an economic residual to force
		// their rendered sum to equal the independently rendered total. The
		// CapitalCharge's optional aggregate decomposition is absent in that case.
		if sum.Cmp(total) != 0 {
			charge.Components = nil
		}
		out.CapitalCharge = charge
	}
	return out, nil
}

// BindingCanonical is the global chain's context witness: explicit version,
// full-preimage length and SHA256(deterministic FilingRecordBody). The complete
// preimage stays in the tenant-scoped artifact, not this non-RLS chain. The
// resulting chain hash is outside its own input. Original Canonical is unchanged.
func BindingCanonical(body *regulatorypb.FilingRecordBody) ([]byte, error) {
	if err := ValidateBody(body); err != nil {
		return nil, err
	}
	b, err := (proto.MarshalOptions{Deterministic: true}).Marshal(body)
	if err != nil || len(b) > MaxBodyBytes {
		return nil, errors.New("regulatory publication: context record exceeds its wire budget")
	}
	digest := sha256.Sum256(b)
	out := make([]byte, len(bindingDomain)+8+len(digest))
	copy(out, bindingDomain)
	binary.BigEndian.PutUint64(out[len(bindingDomain):], uint64(len(b)))
	copy(out[len(bindingDomain)+8:], digest[:])
	return out, nil
}

// ValidateBody validates the retained artifact independently of an HTTP caller.
// A digest is evidence identity, never approval of the supplied dataset.
func ValidateBody(body *regulatorypb.FilingRecordBody) error {
	if body == nil || body.Report == nil || body.Report.AsOf == nil || body.Report.AsOf.CheckValid() != nil || body.RecordedAt == nil || body.RecordedAt.CheckValid() != nil || body.InputOrigin != sustainabilitypb.ObservationOrigin_OBSERVATION_ORIGIN_FILING_REQUEST_UNVERIFIED || len(body.InputManifest) == 0 || len(body.InputManifest) > MaxManifestBytes || len(body.ProducerVersion) == 0 || len(body.ProducerVersion) > 128 || !utf8.ValidString(body.ProducerVersion) {
		return errors.New("regulatory publication: incomplete retained context")
	}
	digest := sha256.Sum256(body.InputManifest)
	if hex.EncodeToString(digest[:]) != body.InputManifestDigest {
		return errors.New("regulatory publication: input manifest digest mismatch")
	}
	scope := filing.PublicationScope{Tenant: body.TenantId, Portfolio: body.PortfolioId, ReportID: body.Report.ReportId, AsOf: body.Report.AsOf.AsTime(), RecordedAt: body.RecordedAt.AsTime(), InputDigest: body.InputManifestDigest}
	if err := scope.Validate(); err != nil {
		return err
	}
	report, err := FromWire(body.Report)
	if err != nil || !bytes.Equal(report.Canonical(), body.FilingCanonical) {
		return errors.New("regulatory publication: wire filing differs from the signed artifact")
	}
	switch body.SignatureKind {
	case regulatorypb.FilingSignatureKind_FILING_SIGNATURE_KIND_AUDIT_CHAIN:
		if !predecessor(body.FilingPrevHash) || chain.Next(body.FilingPrevHash, body.FilingCanonical) != body.Report.Signature {
			return errors.New("regulatory publication: original filing chain witness mismatch")
		}
	case regulatorypb.FilingSignatureKind_FILING_SIGNATURE_KIND_CONTENT_HASH:
		h := sha256.Sum256(body.FilingCanonical)
		if body.FilingPrevHash != "" || hex.EncodeToString(h[:]) != body.Report.Signature {
			return errors.New("regulatory publication: original filing content hash mismatch")
		}
	default:
		return errors.New("regulatory publication: original signature kind is unknown")
	}
	ids := map[string]bool{body.Report.ReportId: true}
	check := func(id, reportID, tenant, portfolio, input string, at *timestamppb.Timestamp) error {
		if !filing.UUIDv7(id) || ids[id] || reportID != scope.ReportID || tenant != scope.Tenant || portfolio != scope.Portfolio || input != scope.InputDigest || at == nil || at.CheckValid() != nil || !at.AsTime().Equal(scope.AsOf) {
			return errors.New("regulatory publication: observation scope or identity mismatch")
		}
		ids[id] = true
		return nil
	}
	if len(body.InstrumentObservations) > 2*sustainabilitypublish.MaxHoldings {
		return errors.New("regulatory publication: too many observations")
	}
	for _, observation := range body.InstrumentObservations {
		if observation == nil || observation.RecordedAt == nil || !proto.Equal(observation.RecordedAt, body.RecordedAt) || observation.Origin != body.InputOrigin || (observation.GetEsg() == nil && observation.GetCarbon() == nil) {
			return errors.New("regulatory publication: missing instrument observation")
		}
		if err := check(observation.ObservationId, observation.ReportId, observation.TenantId, observation.PortfolioId, observation.InputManifestDigest, observation.AsOf); err != nil {
			return err
		}
		if err := sustainabilitypublish.ValidateInstrument(observation); err != nil {
			return err
		}
	}
	if observation := body.PortfolioObservation; observation != nil {
		if observation.RecordedAt == nil || !proto.Equal(observation.RecordedAt, body.RecordedAt) || observation.Origin != body.InputOrigin {
			return errors.New("regulatory publication: unknown aggregate origin")
		}
		if err := check(observation.ObservationId, observation.ReportId, observation.TenantId, observation.PortfolioId, observation.InputManifestDigest, observation.AsOf); err != nil {
			return err
		}
		if err := sustainabilitypublish.ValidateAggregate(observation); err != nil {
			return err
		}
	}
	for _, line := range body.Report.LineItems {
		p := line.GetProvenance()
		if p == nil || p.Method != string(FilingMethod(report.Framework)) || p.InputManifestDigest != body.InputManifestDigest || p.InputDigest != "" || len(p.Params) > 16 || p.ModelId != "" || p.ModelAsOf != nil {
			return errors.New("regulatory publication: incomplete computation provenance")
		}
		for k, v := range p.Params {
			if k == "" || len(k) > 64 || len(v) > 256 || !utf8.ValidString(k) || !utf8.ValidString(v) {
				return errors.New("regulatory publication: provenance parameter exceeds its budget")
			}
		}
	}
	finite := func(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }
	if scenario := body.ClimateScenario; scenario != nil {
		if !filing.BoundedIdentifier(scenario.Name) || !finite(scenario.CarbonPrice) || scenario.CarbonPrice < 0 || !finite(scenario.PhysicalSeverity) || scenario.PhysicalSeverity < 0 || scenario.PhysicalSeverity > 1 || scenario.Kind < 0 || scenario.Kind > sustainabilitypb.ClimateScenarioKind_CLIMATE_SCENARIO_KIND_PHYSICAL {
			return errors.New("regulatory publication: invalid retained climate scenario")
		}
	}
	if scenario := body.RegulatoryScenario; scenario != nil {
		if !filing.BoundedIdentifier(scenario.ScenarioId) || len(scenario.MacroShocks) == 0 || len(scenario.MacroShocks) > 64 || !finite(scenario.Severity) || scenario.Severity < 0 {
			return errors.New("regulatory publication: invalid retained regulatory scenario")
		}
		for name, value := range scenario.MacroShocks {
			if !filing.BoundedIdentifier(name) || !finite(value) {
				return errors.New("regulatory publication: invalid retained macro shock")
			}
		}
	}
	return nil
}

// FromWire reconstructs the existing canonical filing for independent checking.
func FromWire(report *regulatorypb.RegReport) (filing.Report, error) {
	if report == nil || report.AsOf == nil || report.AsOf.CheckValid() != nil || !filing.UUIDv7(report.ReportId) || !filing.Digest(report.Signature) {
		return filing.Report{}, errors.New("regulatory publication: invalid report")
	}
	name, ok := map[regulatorypb.Framework]string{regulatorypb.Framework_FRAMEWORK_FRTB: "FRTB", regulatorypb.Framework_FRAMEWORK_FORM_PF: "FORM_PF", regulatorypb.Framework_FRAMEWORK_AIFMD: "AIFMD", regulatorypb.Framework_FRAMEWORK_TCFD: "TCFD", regulatorypb.Framework_FRAMEWORK_SFDR: "SFDR"}[report.Framework]
	if !ok || len(report.LineItems) == 0 || len(report.LineItems) > 256 {
		return filing.Report{}, errors.New("regulatory publication: invalid framework or line inventory")
	}
	out := filing.Report{Framework: name, AsOf: report.AsOf.AsTime(), Signature: report.Signature}
	seen := make(map[string]bool)
	for _, line := range report.LineItems {
		if line == nil || !filing.BoundedIdentifier(line.Code) || seen[line.Code] || len(line.Label) > 512 || !utf8.ValidString(line.Label) {
			return filing.Report{}, errors.New("regulatory publication: invalid line item")
		}
		value, valid := dec.FromProtoChecked(line.Value)
		if !valid || line.Value == nil {
			return filing.Report{}, errors.New("regulatory publication: invalid filed decimal")
		}
		item := filing.LineItem{Code: line.Code, Label: line.Label, Value: value}
		// The wire must carry the value actually filed, not extra precision
		// hidden by Canonical's rendering. Compare rationals, not encodings.
		projected, err := filing.FiledDecimal(item)
		filed, exact := dec.FromProtoChecked(projected)
		if err != nil || !exact || value.Cmp(filed) != 0 {
			return filing.Report{}, errors.New("regulatory publication: wire value differs from the filed decimal")
		}
		seen[line.Code] = true
		out.LineItems = append(out.LineItems, item)
	}
	expected, err := Report(out, report.ReportId, nil)
	if err != nil || !proto.Equal(expected.GetCapitalCharge(), report.CapitalCharge) {
		return filing.Report{}, errors.New("regulatory publication: capital projection differs from filed rows")
	}
	return out, nil
}

func predecessor(s string) bool { return s == chain.Genesis || filing.Digest(s) }

func Recorded(body *regulatorypb.FilingRecordBody, link signer.Link) (*regulatorypb.FilingRecorded, error) {
	canonical, err := BindingCanonical(body)
	if err != nil {
		return nil, err
	}
	if !predecessor(link.Prev) || !bytes.Equal(link.Body, canonical) || chain.Next(link.Prev, canonical) != link.Cur || (body.SignatureKind == regulatorypb.FilingSignatureKind_FILING_SIGNATURE_KIND_AUDIT_CHAIN && link.Prev != body.Report.Signature) {
		return nil, errors.New("regulatory publication: context chain witness mismatch")
	}
	return &regulatorypb.FilingRecorded{Body: proto.Clone(body).(*regulatorypb.FilingRecordBody), BindingVersion: BindingVersion, BindingPrevHash: link.Prev, BindingHash: link.Cur}, nil
}

func Event(record *regulatorypb.FilingRecorded) (bus.Event, error) {
	if record == nil || record.BindingVersion != BindingVersion {
		return bus.Event{}, errors.New("regulatory publication: unsupported context binding")
	}
	canonical, err := BindingCanonical(record.Body)
	if err != nil {
		return bus.Event{}, err
	}
	if !predecessor(record.BindingPrevHash) || chain.Next(record.BindingPrevHash, canonical) != record.BindingHash || (record.Body.SignatureKind == regulatorypb.FilingSignatureKind_FILING_SIGNATURE_KIND_AUDIT_CHAIN && record.BindingPrevHash != record.Body.Report.Signature) {
		return bus.Event{}, errors.New("regulatory publication: context binding was altered")
	}
	b := record.Body
	return bus.Event{EventID: b.Report.ReportId, Subject: RecordedSubject, EventType: RecordedSubject, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, Domain: "compliance", EventTime: b.RecordedAt.AsTime(), PartitionKey: b.TenantId + "/" + b.PortfolioId, CorrelationID: b.Report.ReportId, TenantID: b.TenantId, Payload: record}, nil
}

// Publisher is the local canonical-outbox adapter. Domain IDs are persisted with
// the artifact; Record intentionally leaves envelope identity to its producer.
type Publisher struct {
	producer *bus.Producer
	tenant   string
}

func New(producer *bus.Producer, tenant string) (*Publisher, error) {
	if producer == nil || !filing.BoundedIdentifier(tenant) {
		return nil, errors.New("regulatory publication: producer required")
	}
	return &Publisher{producer: producer, tenant: tenant}, nil
}

func (p *Publisher) Tenant() string {
	if p == nil {
		return ""
	}
	return p.tenant
}

func (p *Publisher) Publish(ctx context.Context, stored bus.Event) error {
	var event bus.Event
	var err error
	switch payload := stored.Payload.(type) {
	case *regulatorypb.FilingRecorded:
		event, err = Event(payload)
	case *sustainabilitypb.InstrumentObserved, *sustainabilitypb.PortfolioAggregated:
		event, err = sustainabilitypublish.Event(payload)
	default:
		return fmt.Errorf("regulatory publication: unsupported persisted payload %T", stored.Payload)
	}
	if err != nil {
		return err
	}
	if p == nil || p.producer == nil || stored.TenantID != p.tenant || stored.Subject != event.Subject || stored.EventType != event.EventType || stored.Domain != event.Domain || stored.TenantID != event.TenantID || stored.PartitionKey != event.PartitionKey || stored.CorrelationID != event.CorrelationID || !stored.EventTime.Equal(event.EventTime) || stored.SchemaVersion != 1 || stored.EventClass != envelopepb.EventClass_EVENT_CLASS_FACT || (stored.EventID != "" && stored.EventID != event.EventID) || stored.ExpectedLastSubjectSeq != nil {
		return errors.New("regulatory publication: persisted event disagrees with its typed artifact")
	}
	event.CausationID, event.TraceContext = stored.CausationID, stored.TraceContext
	return p.producer.Publish(ctx, event)
}
