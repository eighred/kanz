package compliance_test

import (
	"context"
	"errors"
	comp "github.com/eighred/kanz/internal/compliance"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	"testing"
	"time"
)

func TestPublisherRefusesApprovedUnreadableRuleBeforeRetainedRead(t *testing.T) {
	for _, r := range []*compliancepb.Rule{
		{RuleId: "missing", Type: compliancepb.RuleType_RULE_TYPE_CONCENTRATION},
		{RuleId: "wrong", Type: compliancepb.RuleType_RULE_TYPE_CONCENTRATION, Params: &compliancepb.Rule_BuyingPower{BuyingPower: &compliancepb.BuyingPowerLimit{}}},
		{RuleId: "domain", Type: compliancepb.RuleType_RULE_TYPE_CONCENTRATION, Params: &compliancepb.Rule_Concentration{Concentration: &compliancepb.ConcentrationLimit{MaxWeight: &commonpb.Decimal{Coefficient: 1, Exponent: 65}}}},
	} {
		t.Run(r.RuleId, func(t *testing.T) {
			m := mandateFixture()
			m.Rules = []*compliancepb.Rule{r}
			b := &countingBus{readErr: errors.New("retained-read-canary")}
			err := comp.NewPublisher(b, b).Publish(context.Background(), m, approvalFor(t, m, "unreadable rule"), "unreadable rule")
			if !errors.Is(err, comp.ErrMandateIncomplete) || len(b.events) != 0 {
				t.Fatalf("approved unreadable rule reached retained state: err=%v events=%d", err, len(b.events))
			}
		})
	}
}
func TestUnreadableRuleJetStreamReplayRefusalAndRepair(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	rig := newMandateBrokerRig(t, ctx)
	const tenant = "acme"
	portfolio := "pf-contract-" + rig.suffix
	bad := mandateVersion(tenant, portfolio, 1, time.Now().UTC().Add(-time.Hour))
	bad.Rules = []*compliancepb.Rule{{RuleId: "bad", Type: compliancepb.RuleType_RULE_TYPE_CONCENTRATION, OnViolation: compliancepb.ComplianceStatus_COMPLIANCE_STATUS_WARN}}
	subject := comp.SubjectMandateFor(tenant, portfolio)
	// Bypass authoring only inside the fixture to represent retained historical bytes.
	if err := rig.producer.Publish(ctx, mandateEvent(tenant, portfolio, subject, bad, nil)); err != nil {
		t.Fatal(err)
	}
	for restart := 0; restart < 2; restart++ {
		reg := rig.bootAGate(t, ctx, tenant, portfolio)
		_, _, err := reg.Mandate(ctx, tenant, portfolio, time.Now().UTC())
		if !errors.Is(err, comp.ErrMandateUnreadable) {
			t.Fatalf("fresh replica %d accepted poisoned retained rule: %v", restart, err)
		}
	}
	repaired := mandateVersion(tenant, portfolio, 2, time.Now().UTC().Add(-time.Hour))
	repaired.Rules = []*compliancepb.Rule{{RuleId: "readable", Type: compliancepb.RuleType_RULE_TYPE_BUYING_POWER, Params: &compliancepb.Rule_BuyingPower{BuyingPower: &compliancepb.BuyingPowerLimit{}}}}
	// Publisher must read the poisoned retained value and replace it only under
	// a real two-person approval; a blank in-memory registry cannot prove recovery.
	if err := rig.publish(t, ctx, repaired); err != nil {
		t.Fatal(err)
	}
	reg := rig.bootAGate(t, ctx, tenant, portfolio)
	got, found, err := reg.Mandate(ctx, tenant, portfolio, time.Now().UTC())
	if err != nil || found.NoMandate() || got.GetVersion() != 2 {
		t.Fatalf("repaired retained rule did not recover fresh replica: got=%v err=%v", got, err)
	}
}
