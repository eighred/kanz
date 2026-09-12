package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/transport"
)

// runPortfolioBootstrap is the one-shot, typed source of the first testnet
// portfolio. Its workload identity is allowed to publish only this subject;
// the ordinary capital-path certifier runs under a different ServiceAccount.
func runPortfolioBootstrap(parent context.Context, now func() time.Time) error {
	tenant := os.Getenv("CAPITALPATH_TENANT")
	portfolio := os.Getenv("CAPITALPATH_PORTFOLIO")
	natsURL := os.Getenv("CAPITALPATH_NATS_URL")
	spiffeSocket := os.Getenv("CAPITALPATH_SPIFFE_SOCKET")
	if tenant != "__system__" {
		return fmt.Errorf("portfolio-bootstrap: CAPITALPATH_TENANT must be __system__, got %q", tenant)
	}
	if portfolio != "PF1" {
		return fmt.Errorf("portfolio-bootstrap: CAPITALPATH_PORTFOLIO must be PF1, got %q", portfolio)
	}
	if natsURL == "" || spiffeSocket == "" {
		return errors.New("portfolio-bootstrap: NATS URL and SPIFFE workload socket are required")
	}
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	mesh, err := transport.NewMesh(ctx, spiffeSocket)
	if err != nil {
		return fmt.Errorf("portfolio-bootstrap: acquire workload identity: %w", err)
	}
	defer mesh.Close()
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: natsURL, Name: "portfolio-bootstrap", TLSConfig: mesh.Client, MaxReconnects: 1})
	if err != nil {
		return fmt.Errorf("portfolio-bootstrap: dial mTLS event spine: %w", err)
	}
	defer client.Close()
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "portfolio-bootstrap", ProducerVersion: "1", Tenant: tenant})
	if err != nil {
		return fmt.Errorf("portfolio-bootstrap: producer: %w", err)
	}
	at := now().UTC()
	event := bus.Event{
		Subject: "risk.portfolio.snapshot", EventType: "risk.portfolio.snapshot",
		EventClass: envelopepb.EventClass_EVENT_CLASS_STATE_SNAPSHOT, SchemaVersion: 1,
		Domain: "risk", EventTime: at, PartitionKey: portfolio,
		PayloadSchemaRef: "domain.v1.PortfolioSnapshot:1",
		Payload: &domainpb.PortfolioSnapshot{Portfolio: &domainpb.PortfolioState{
			PortfolioId: portfolio, DisplayName: "Tokyo Testnet Portfolio", BaseCurrency: "USD", AsOf: timestamppb.New(at),
		}},
	}
	if err := producer.Publish(ctx, event); err != nil {
		return fmt.Errorf("portfolio-bootstrap: publish typed snapshot: %w", err)
	}
	fmt.Printf("portfolio-bootstrap: published risk.portfolio.snapshot tenant=%s portfolio=%s at=%s\n", tenant, portfolio, at.Format(time.RFC3339))
	return nil
}
