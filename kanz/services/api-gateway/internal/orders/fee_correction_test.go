package orders

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eighred/kanz/internal/platform/halt"
	commandpb "github.com/eighred/kanz/kanz-schemas-go/command/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/api-gateway/internal/middleware"
	"google.golang.org/protobuf/proto"
)

func TestFeeCommandsRequireSeparateCapabilitiesAndBindAuthenticatedIdentity(t *testing.T) {
	for _, tc := range []struct {
		action, role string
		want         int
	}{
		{"propose", "trader", 202}, {"propose", approverRole, 403}, {"approve", "trader", 403}, {"approve", approverRole, 202}, {"approve", "analyst", 403},
	} {
		t.Run(tc.action+tc.role, func(t *testing.T) {
			producer, capture := realProducer(t)
			mux := approveMux()
			New(producer, approverRole, haltedGate(t)).Routes(mux)
			body := `{"reason":"verified venue statement","metadata":{"issuer":"user:forged","principalPortfolios":["other"]},"orderId":"forged","caseId":"forged"}`
			if tc.action == "approve" {
				body = `{"digest":"` + strings.Repeat("a", 64) + `","metadata":{"issuer":"user:forged","principalPortfolios":["other"]},"orderId":"forged","caseId":"forged"}`
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/orders/o1/execution-recoveries/c1/fee-correction/"+tc.action, strings.NewReader(body))
			req = req.WithContext(middleware.WithPrincipal(req.Context(), &middleware.Principal{Subject: "alice", Tenant: "acme", Roles: []string{tc.role}, Portfolios: []string{"fund"}}))
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, req)
			if rr.Code != tc.want {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
			if tc.want != 202 {
				if len(capture.sent) != 0 {
					t.Fatal("unauthorized command published")
				}
				return
			}
			if len(capture.sent) != 1 {
				t.Fatal("missing command")
			}
			env, data, err := bus.Unframe(capture.sent[0].Body)
			if err != nil {
				t.Fatal(err)
			}
			if err := bus.Validate(env); err != nil {
				t.Fatal(err)
			}
			var md *commandpb.CommandMetadata
			var orderID, caseID string
			if tc.action == "approve" {
				var c orderpb.ApproveExecutionFeeCorrection
				if err := proto.Unmarshal(data, &c); err != nil {
					t.Fatal(err)
				}
				md, orderID, caseID = c.Metadata, c.OrderId, c.CaseId
			} else {
				var c orderpb.ProposeExecutionFeeCorrection
				if err := proto.Unmarshal(data, &c); err != nil {
					t.Fatal(err)
				}
				md, orderID, caseID = c.Metadata, c.OrderId, c.CaseId
			}
			if md.Issuer != "user:alice" || md.TargetId != "o1" || orderID != "o1" || caseID != "c1" || len(md.PrincipalPortfolios) != 1 || md.PrincipalPortfolios[0] != "fund" {
				t.Fatalf("forged identity or target: %v", md)
			}
		})
	}
}

func TestFeeApprovalWithoutConfiguredApproverIsUnavailable(t *testing.T) {
	mux := approveMux()
	New(&fakePub{}, "", halt.OpenGate(nil)).Routes(mux)
	req := asRole(httptest.NewRequest(http.MethodPost, "/v1/orders/o1/execution-recoveries/c1/fee-correction/approve", strings.NewReader(`{}`)), "checker", approverRole)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatal(rr.Code)
	}
}
