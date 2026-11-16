package api_test

import (
	"net/http"
	"strings"
	"testing"
)

func TestUnreadableRuleCannotCreateGovernanceProposal(t *testing.T) {
	for _, rule := range []string{
		`{"rule_id":"missing","type":"RULE_TYPE_CONCENTRATION","on_violation":"COMPLIANCE_STATUS_WARN"}`,
		`{"rule_id":"wrong","type":"RULE_TYPE_CONCENTRATION","buying_power":{}}`,
		`{"rule_id":"domain","type":"RULE_TYPE_CONCENTRATION","concentration":{"max_weight":{"coefficient":"1","exponent":65}}}`,
	} {
		r := newRig(t)
		body := `{"mandate":` + strings.TrimSuffix(mandateJSON(t, 3, 0), "}") + `,"rules":[` + rule + `]},"reason":"invalid rule"}`
		response := r.do(t, http.MethodPost, "/v1/portfolios/"+portfolio+"/mandate", proposerAkif, tenantA, body)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid proposal = %d %s", response.Code, response.Body.String())
		}
		pending := r.do(t, http.MethodGet, "/v1/mandates/pending-changes", proposerAkif, tenantA, "")
		if pending.Code != http.StatusOK || strings.Contains(pending.Body.String(), "proposal_id") || len(r.bus.published()) != 0 {
			t.Fatalf("invalid mandate created governance state or FACT: pending=%d %s", pending.Code, pending.Body.String())
		}
	}
}
