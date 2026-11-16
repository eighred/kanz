package compliance_test

import (
	"context"
	"errors"
	"testing"

	comp "github.com/eighred/kanz/internal/compliance"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
)

func TestPublisherRefusesApprovedUnknownSeverityBeforeRetainedRead(t *testing.T) {
	m := mandateFixture()
	m.Rules = []*compliancepb.Rule{{RuleId: "bad-severity", OnViolation: 99}}
	// A valid approval proves this refuses the unreadable mandate rather than an
	// absent approval. The retained reader must remain untouched.
	b := &countingBus{readErr: errors.New("retained state must not be read")}
	approval := approvalFor(t, m, "test unreadable severity")
	err := comp.NewPublisher(b, b).Publish(context.Background(), m, approval, "test unreadable severity")
	if !errors.Is(err, comp.ErrMandateIncomplete) || len(b.events) != 0 {
		t.Fatalf("approved unreadable mandate reached state/publish path: err=%v events=%d", err, len(b.events))
	}
}
