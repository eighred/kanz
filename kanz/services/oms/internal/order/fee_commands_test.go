package order

import (
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dualcontrol"
	"github.com/eighred/kanz/internal/platform/halt"
	commandpb "github.com/eighred/kanz/kanz-schemas-go/command/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"
)

func TestFeeCommandsAuthorizePortfolioAndResumeCommittedApproval(t *testing.T) {
	store, ctx, c, _ := feeRecoveryFixture(t)
	newService := func() *Service {
		t.Helper()
		svc, err := NewService(testTenant, NewPostgres(store.pool), NewEmitter(&fakeBus{}), nil, nil, nil, nil, WithHaltGate(halt.OpenGate(nil)))
		if err != nil {
			t.Fatal(err)
		}
		svc.now = func() time.Time { return t0 }
		return svc
	}
	svc := newService()
	cmd := &orderpb.ProposeExecutionFeeCorrection{OrderId: c.OrderID, CaseId: c.ID, Reason: "verified fee", Metadata: &commandpb.CommandMetadata{Issuer: "user:maker", TargetId: c.OrderID, PrincipalPortfolios: []string{"fund"}}}
	env := &envelopepb.Envelope{TenantId: testTenant, EventType: SubjectFeePropose, EventClass: envelopepb.EventClass_EVENT_CLASS_COMMAND}
	for _, mutate := range []func(*orderpb.ProposeExecutionFeeCorrection, *envelopepb.Envelope){
		func(_ *orderpb.ProposeExecutionFeeCorrection, e *envelopepb.Envelope) { e.TenantId = "other" },
		func(c *orderpb.ProposeExecutionFeeCorrection, _ *envelopepb.Envelope) {
			c.Metadata.PrincipalPortfolios = []string{"other"}
		},
		func(c *orderpb.ProposeExecutionFeeCorrection, _ *envelopepb.Envelope) {
			c.Metadata.Issuer = "strategy:machine"
		},
		func(c *orderpb.ProposeExecutionFeeCorrection, _ *envelopepb.Envelope) { c.Metadata.TargetId = "other" },
	} {
		v := proto.Clone(cmd).(*orderpb.ProposeExecutionFeeCorrection)
		e := proto.Clone(env).(*envelopepb.Envelope)
		mutate(v, e)
		payload, err := proto.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err := svc.HandleFeeCorrection(ctx, e, payload); err == nil {
			t.Fatal("unauthorized correction accepted")
		}
	}
	payload, err := proto.Marshal(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.HandleFeeCorrection(ctx, env, payload); err != nil {
		t.Fatal(err)
	}
	p, err := store.FeeProposal(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	approval := &orderpb.ApproveExecutionFeeCorrection{OrderId: c.OrderID, CaseId: c.ID, Digest: p.Digest, Metadata: proto.Clone(cmd.Metadata).(*commandpb.CommandMetadata)}
	env.EventType = SubjectFeeApprove
	payload, err = proto.Marshal(approval)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.HandleFeeCorrection(ctx, env, payload); err != nil {
		t.Fatalf("record self approval refusal: %v", err)
	}
	refused, err := store.RecoveryCase(ctx, c.ID)
	if err != nil || refused.Status != "blocked" || !strings.Contains(refused.Reason, "different people") {
		t.Fatalf("refusal was not durable: %+v %v", refused, err)
	}
	approval.Metadata.Issuer = "user:checker"
	payload, err = proto.Marshal(approval)
	if err != nil {
		t.Fatal(err)
	}
	if err := newService().HandleFeeCorrection(ctx, env, payload); err != nil {
		t.Fatal(err)
	}
	restarted := newService()
	restarted.now = func() time.Time { return t0.Add(2 * dualcontrol.DefaultTTL) }
	if err := restarted.HandleFeeCorrection(ctx, env, payload); err != nil {
		t.Fatal(err)
	}
	var orders, revisions int
	if err := store.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM orders),(SELECT count(*) FROM execution_fee_revisions WHERE book='oms')`).Scan(&orders, &revisions); err != nil || orders != 1 || revisions != 1 {
		t.Fatalf("orders=%d revisions=%d err=%v", orders, revisions, err)
	}
}
