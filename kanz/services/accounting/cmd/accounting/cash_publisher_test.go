package main

// THE COMPOSITION ROOT IS WHERE THIS DEFECT LIVED (#245).
//
// services/accounting/internal/cashmove builds a correct envelope — its Tier-B
// tests prove every field Validate requires. What was wrong was the ONE line
// that wires it: bus.ProducerConfig{Source, ProducerVersion} with no Tenant.
// Nothing in the package's own tests can see that, and buildCashPublisher dials
// a broker before it constructs anything, so the config it passes was untested
// by construction. cashProducerConfig exists to give that line a seam.

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/cashmove"
	"github.com/eighred/kanz/services/accounting/internal/config"
)

type recordingClient struct{ sent []bus.Message }

func (c *recordingClient) Publish(_ context.Context, m bus.Message) error {
	c.sent = append(c.sent, m)
	return nil
}
func (c *recordingClient) Subscribe(context.Context, string, string, bus.Handler) error {
	return errors.New("not implemented")
}
func (c *recordingClient) Close() error { return nil }

// A SUBSCRIPTION WIRED THE WAY main() WIRES IT MUST REACH THE TRANSPORT.
//
// This is the whole bug in one assertion: build the producer from the real
// cashProducerConfig, publish through the real cashmove.Publisher, and require
// that bytes come out. Before the Tenant fallback was added this failed with
// "envelope validation: tenant_id required" and the fund's subscription went
// nowhere.
func TestCashPublisherWiredFromConfigEmitsAValidatedFact(t *testing.T) {
	cfg := config.Config{Source: "accounting", Tenant: "acme-fund"}

	rc := &recordingClient{}
	producer, err := bus.NewProducer(rc, cashProducerConfig(cfg))
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	pub, err := cashmove.NewPublisher(producer, nil)
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}

	err = pub.Publish(context.Background(), cashmove.CashMovement{
		MovementID:  "MV-WIRED",
		PortfolioID: "PORT-1",
		Kind:        cashmove.Subscription,
		Amount:      big.NewRat(100000, 1),
		Currency:    "USD",
		Effective:   time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("a cash movement published through the wired producer was refused: %v\n"+
			"This is the composition root, not the encoder: the endpoint answers 400 and the "+
			"ledger never sees the subscription.", err)
	}
	if len(rc.sent) != 1 {
		t.Fatalf("transport received %d messages, want 1", len(rc.sent))
	}
	env, _, err := bus.Unframe(rc.sent[0].Body)
	if err != nil {
		t.Fatalf("Unframe: %v", err)
	}
	if err := bus.Validate(env); err != nil {
		t.Errorf("wired envelope fails Validate: %v", err)
	}
	if env.GetTenantId() != "acme-fund" {
		t.Errorf("TenantId = %q, want the configured ACCOUNTING_TENANT (acme-fund)", env.GetTenantId())
	}
}

// The tenant the cash producer stamps must be the SAME tenant the folder and the
// RLS pool are pinned to (cfg.Tenant). A cash FACT emitted under a different one
// is refused by consume.Folder's RequireTenantScope (#223) and DLQs — the
// movement is lost just as completely as if it had never been published.
func TestCashProducerTenantMatchesTheFolderTenant(t *testing.T) {
	cfg := config.Config{Source: "accounting", Tenant: "beta-fund"}
	if got := cashProducerConfig(cfg).Tenant; got != cfg.Tenant {
		t.Fatalf("cash producer tenant = %q, want the service tenant %q — the folder would "+
			"reject its own service's cash FACTs", got, cfg.Tenant)
	}
}
