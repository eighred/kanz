package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	"github.com/eighred/kanz/pkg/bus"
)

type bootstrapCaptureClient struct{ sent []bus.Message }

func (c *bootstrapCaptureClient) Publish(_ context.Context, msg bus.Message) error {
	c.sent = append(c.sent, msg)
	return nil
}
func (*bootstrapCaptureClient) Subscribe(context.Context, string, string, bus.Handler) error {
	return errors.New("not implemented")
}
func (*bootstrapCaptureClient) Close() error { return nil }

func TestPortfolioBootstrapRefusesUnreviewedScopeBeforeDial(t *testing.T) {
	t.Setenv("CAPITALPATH_TENANT", "someone-else")
	t.Setenv("CAPITALPATH_PORTFOLIO", "PF1")
	t.Setenv("CAPITALPATH_NATS_URL", "nats://unreachable")
	t.Setenv("CAPITALPATH_SPIFFE_SOCKET", "unix:///unreachable")
	err := runPortfolioBootstrap(context.Background(), time.Now)
	if err == nil || !strings.Contains(err.Error(), "must be __system__") {
		t.Fatalf("unreviewed tenant was not refused before network access: %v", err)
	}
}

func TestPortfolioBootstrapRefusesUnreviewedPortfolioBeforeDial(t *testing.T) {
	t.Setenv("CAPITALPATH_TENANT", "__system__")
	t.Setenv("CAPITALPATH_PORTFOLIO", "PF2")
	t.Setenv("CAPITALPATH_NATS_URL", "nats://unreachable")
	t.Setenv("CAPITALPATH_SPIFFE_SOCKET", "unix:///unreachable")
	err := runPortfolioBootstrap(context.Background(), time.Now)
	if err == nil || !strings.Contains(err.Error(), "must be PF1") {
		t.Fatalf("unreviewed portfolio was not refused before network access: %v", err)
	}
}

func TestPortfolioBootstrapPublishesAValidTypedEnvelope(t *testing.T) {
	client := &bootstrapCaptureClient{}
	producer, err := bus.NewProducer(client, bus.ProducerConfig{
		Source: "portfolio-bootstrap/test", ProducerVersion: "test", Tenant: "__system__",
	})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	at := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	if err := publishPortfolioSnapshot(context.Background(), producer, "__system__", "PF1", at); err != nil {
		t.Fatalf("publishPortfolioSnapshot: %v", err)
	}
	if len(client.sent) != 1 {
		t.Fatalf("messages = %d, want 1", len(client.sent))
	}
	msg := client.sent[0]
	if msg.Subject != "risk.portfolio.snapshot" || string(msg.Key) != "PF1" {
		t.Fatalf("message subject/key = %q/%q, want risk.portfolio.snapshot/PF1", msg.Subject, msg.Key)
	}
	env, payload, err := bus.Unframe(msg.Body)
	if err != nil {
		t.Fatalf("Unframe: %v", err)
	}
	if err := bus.Validate(env); err != nil {
		t.Fatalf("published envelope fails Validate: %v", err)
	}
	if env.GetTenantId() != "__system__" || env.GetPartitionKey() != "PF1" {
		t.Fatalf("envelope tenant/partition = %q/%q, want __system__/PF1", env.GetTenantId(), env.GetPartitionKey())
	}
	var snapshot domainpb.PortfolioSnapshot
	if err := proto.Unmarshal(payload, &snapshot); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if snapshot.GetPortfolio().GetPortfolioId() != "PF1" || snapshot.GetPortfolio().GetBaseCurrency() != "USD" {
		t.Fatalf("unexpected portfolio payload: %v", snapshot.GetPortfolio())
	}
}
