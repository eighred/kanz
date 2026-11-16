package compliance

import (
	"context"
	"errors"
	"fmt"
	"testing"

	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
)

func TestMandateRejectsUnsupportedViolationSeverity(t *testing.T) {
	for _, status := range []compliancepb.ComplianceStatus{-1, 99, compliancepb.ComplianceStatus_COMPLIANCE_STATUS_PASS} {
		t.Run(fmt.Sprint(int32(status)), func(t *testing.T) {
			m := concentrationMandate(60)
			m.Rules[0].OnViolation = status
			if err := ValidateMandate(m); !errors.Is(err, ErrMandateIncomplete) {
				t.Fatalf("unreadable severity %d must refuse mandate, got %v", status, err)
			}
		})
	}
	for _, status := range []compliancepb.ComplianceStatus{
		compliancepb.ComplianceStatus_COMPLIANCE_STATUS_UNSPECIFIED,
		compliancepb.ComplianceStatus_COMPLIANCE_STATUS_WARN,
		compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH,
	} {
		m := concentrationMandate(60)
		m.Rules[0].OnViolation = status
		if err := ValidateMandate(m); err != nil {
			t.Fatalf("documented severity %v refused: %v", status, err)
		}
	}
}

func TestUnreadableSeverityCannotProduceCleanCompliance(t *testing.T) {
	for _, status := range []compliancepb.ComplianceStatus{-1, 99, compliancepb.ComplianceStatus_COMPLIANCE_STATUS_PASS} {
		for _, cap := range []int64{40, 90} {
			t.Run(fmt.Sprintf("%d/cap%d", status, cap), func(t *testing.T) {
				m := concentrationMandate(cap)
				m.Rules[0].OnViolation = status
				result := NewEngine(nil).Evaluate(context.Background(), &Candidate{Book: twoEqualBook(), AsOf: t0}, m)
				if result.GetStatus() != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH ||
					len(result.GetViolations()) != 1 ||
					result.GetViolations()[0].GetSeverity() != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH {
					t.Fatalf("unknown policy cannot become a clean result: %v", result)
				}
			})
		}
	}
}

func TestGateRecordsDenialForUnreadableSeverity(t *testing.T) {
	for _, status := range []compliancepb.ComplianceStatus{-1, 99, compliancepb.ComplianceStatus_COMPLIANCE_STATUS_PASS} {
		t.Run(fmt.Sprint(int32(status)), func(t *testing.T) {
			m := concentrationMandate(60)
			m.Rules[0].OnViolation = status
			reg := NewMandateRegistry()
			mustPut(t, reg, m)
			rec := &recordingRecorder{}
			gate := NewPreTradeGate(NewEngine(nil), MapBookSource{"p1": currentBook()}, reg, nil, rec, nil)
			decision, err := gate.Evaluate(context.Background(), OrderDelta{
				TenantID: "t1", PortfolioID: "p1", InstrumentID: "AAPL", SignedQuantity: dec(100, 0),
				Price: dec(1000, 0), Currency: "USD", OrderID: "severity-order", AsOf: t0,
			})
			if err != nil {
				t.Fatal(err)
			}
			if decision.Allowed || len(rec.records) != 1 || rec.records[0].Allowed ||
				rec.records[0].Result.GetStatus() != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH {
				t.Fatalf("unreadable policy must deny and record one denial: decision=%+v records=%+v", decision, rec.records)
			}
		})
	}
}
