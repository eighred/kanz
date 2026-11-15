package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	observationpb "github.com/eighred/kanz/kanz-schemas-go/observation/v1"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/copilot/internal/config"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type agentIdentityAllowRead struct{}

func (agentIdentityAllowRead) Authorize(context.Context, auth.Request) auth.Decision {
	return auth.Decision{Allow: true, Reason: "fixture read grant"}
}

// This binary is copied into the owned SPIRE fixture, never a released image.
// Ordinary tests skip it; the Linux proof requires its explicit result receipt.
func TestAgentIdentityRecorder(t *testing.T) {
	if os.Getenv("AGENT_IDENTITY_RECORDER") != "disposable-spire" {
		t.Skip("requires the owned disposable SPIRE/NATS fixture")
	}
	address := os.Getenv("AGENT_IDENTITY_NATS")
	u, err := url.Parse(address)
	if err != nil || u.Scheme != "tls" || u.User != nil || u.Port() != "4222" ||
		u.Path != "" || u.RawQuery != "" || u.Fragment != "" || !net.ParseIP(u.Hostname()).IsPrivate() {
		t.Fatal("fixture requires one private cluster NATS service IP")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
	defer cancel()
	cfg := config.Config{NATSURL: address, SPIFFESocket: "unix:///run/spiffe/spire-agent.sock", Tenant: "__system__"}
	rec, producer, closeRecorder, err := buildDecisionRecorder(ctx, cfg, prometheus.NewRegistry(), quietLogger())
	if os.Getenv("AGENT_IDENTITY_MODE") == "wrong-server" {
		if err == nil {
			closeRecorder()
			t.Fatal("production recorder accepted a different SPIFFE broker identity")
		}
		// A generic outage does not prove peer rejection. The coordinator also
		// connects successfully while explicitly authorizing this actual peer.
		if !strings.Contains(err.Error(), "unexpected ID") || !strings.Contains(err.Error(), "/sa/nats-wrong") {
			t.Fatal("recorder failed without the exact peer-authorization refusal")
		}
		emitAgentIdentityResult(map[string]bool{"wrong_server_identity_denied": true})
		return
	}
	if err != nil || producer == nil || rec == nil {
		t.Fatal("actual recorder could not connect to the canonical broker")
	}
	closed := false
	defer func() {
		if !closed {
			closeRecorder()
		}
	}()
	request := auth.Request{
		Principal: &auth.Principal{Subject: "agent-identity-analyst", Tenant: "fixture-tenant-a"},
		Action:    auth.ActionRiskRead,
		Resource:  auth.Resource{Type: auth.ResourcePortfolio, ID: "fixture-portfolio", Tenant: "fixture-tenant-a"},
	}
	for _, inner := range []auth.Authorizer{denyAll{}, agentIdentityAllowRead{}} {
		auth.NewAuditedAuthorizer(inner, rec, "copilot", quietLogger()).Authorize(ctx, request)
	}
	answer := &observationpb.AgentAnswerRecord{
		AnswerId: "agent-identity-answer", Agent: "copilot", PrincipalSubject: request.Principal.Subject,
		TenantId: request.Principal.Tenant, Models: []string{"fixture-model"},
		QuestionDigest: "fixture-question-digest", AnswerDigest: "fixture-answer-digest",
		Outcome:    observationpb.AgentAnswerOutcome_AGENT_ANSWER_OUTCOME_ANSWERED,
		AnsweredAt: timestamppb.Now(),
	}
	if err := (&busAnswerRecorder{producer: producer}).RecordAnswer(ctx, answer); err != nil {
		t.Fatal("actual answer recorder did not receive a publish ACK")
	}
	closeRecorder() // wait for both asynchronous decision publish ACKs
	closed = true
	if testutil.ToFloat64(authDecisionsLost.WithLabelValues("publish_error")) != 0 ||
		testutil.ToFloat64(authDecisionsLost.WithLabelValues("queue_full")) != 0 {
		t.Fatal("actual authorization recorder lost a decision")
	}
	emitAgentIdentityResult(map[string]bool{"authz_publish_allowed": true, "answer_publish_allowed": true})
}

func emitAgentIdentityResult(cases map[string]bool) {
	data, _ := json.Marshal(map[string]any{"passed": true, "cases": cases})
	fmt.Printf("AGENT_IDENTITY_RESULT:%s\n", data)
}
