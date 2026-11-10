// The tenant's own account must serve its acknowledged data under a stream
// name shared by both fixture tenants.
package main

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	ns "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"

	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
)

func TestSeedReadbackCountsOnlySnapshotSubject(t *testing.T) {
	server, err := ns.NewServer(&ns.Options{
		Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir(), NoLog: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	go server.Start()
	t.Cleanup(func() { server.Shutdown(); server.WaitForShutdown() })
	if !server.ReadyForConnections(5 * time.Second) {
		t.Fatal("fixture NATS did not start")
	}
	nc, err := nats.Connect(server.ClientURL(), nats.NoReconnect())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := nc.JetStream(nats.MaxWait(3 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.AddStream(&nats.StreamConfig{Name: "RISK", Subjects: []string{"risk.>"}, Storage: nats.MemoryStorage}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ONBOARDING_PROBE_PORTFOLIO", "PFACME")
	if err := verifySeed(js, "acme", "seed-read"); err == nil {
		t.Fatal("missing seed was accepted")
	}
	if _, err := js.Publish("risk.exposure.fixture", []byte("unrelated risk record")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: server.ClientURL()})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "load-seed/seed", ProducerVersion: "dev", Tenant: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	publish := func() {
		t.Helper()
		if err := producer.Publish(ctx, bus.Event{
			Subject: "risk.portfolio.snapshot", EventType: "risk.portfolio.snapshot",
			EventClass: envelopepb.EventClass_EVENT_CLASS_STATE_SNAPSHOT, SchemaVersion: 1,
			Domain: "risk", EventTime: time.Now().UTC(), PartitionKey: "PFACME",
			PayloadSchemaRef: "domain.v1.PortfolioSnapshot:1",
			Payload: &domainpb.PortfolioSnapshot{
				Portfolio: &domainpb.PortfolioState{PortfolioId: "PFACME"},
				Positions: []*domainpb.PositionState{{InstrumentId: "AAPL"}},
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
	publish()
	if err := verifySeed(js, "acme", "seed-read"); err != nil {
		t.Fatalf("one seed alongside unrelated risk traffic: %v", err)
	}
	if err := verifySeed(js, "bravo", "seed-read"); err == nil {
		t.Fatal("wrong tenant seed was accepted")
	}
	publish()
	if err := verifySeed(js, "acme", "seed-read"); err == nil {
		t.Fatal("duplicate seed was accepted")
	}
}

func verify(url, creds, tenant, mode string) error {
	nc, err := nats.Connect(url, nats.UserCredentials(creds), nats.Timeout(3*time.Second), nats.NoReconnect())
	if err != nil {
		return fmt.Errorf("authenticate probe: %w", err)
	}
	defer nc.Close()
	js, err := nc.JetStream(nats.MaxWait(3 * time.Second))
	if err != nil {
		return err
	}
	info, err := js.AccountInfo()
	if err != nil {
		return err
	}
	if info.Limits.MaxMemory != 64_000_000 || info.Limits.MaxStore != 2_000_000_000 {
		return fmt.Errorf("unexpected JetStream account limits")
	}
	if mode == "seed-prepare" || mode == "seed-read" || mode == "projection-prepare" {
		return verifySeed(js, tenant, mode)
	}
	const stream = "LIFECYCLE"
	const subject = "fixture.lifecycle"
	if mode == "create" {
		if _, err := js.AddStream(&nats.StreamConfig{
			Name: stream, Subjects: []string{subject}, Storage: nats.MemoryStorage,
		}); err != nil {
			return err
		}
		ack, err := js.Publish(subject, []byte(tenant))
		if err != nil {
			return err
		}
		if ack.Stream != stream || ack.Sequence != 1 {
			return fmt.Errorf("first publish did not start a tenant-local stream")
		}
	}
	state, err := js.StreamInfo(stream)
	if err != nil {
		return err
	}
	if state.State.Msgs != 1 || state.State.FirstSeq != 1 || state.State.LastSeq != 1 {
		return fmt.Errorf("expected one tenant-local record")
	}
	record, err := js.GetMsg(stream, 1)
	if err != nil {
		return err
	}
	if record.Subject != subject || string(record.Data) != tenant {
		return fmt.Errorf("stream returned a record outside the tenant's account")
	}
	return nil
}

func verifySeed(js nats.JetStreamContext, tenant, mode string) error {
	const stream = "RISK"
	const subject = "risk.portfolio.snapshot"
	if mode == "projection-prepare" {
		// Match the canonical bootstrap's subjects used by the risk process.
		for _, stream := range []nats.StreamConfig{
			{Name: "RISK", Subjects: []string{"risk.portfolio.>", "risk.exposure.>", "risk.signal.>", "risk.command.>", "risk.curve.>", "risk.factor.>"}},
			{Name: "POSITION", Subjects: []string{"risk.position.>"}, MaxMsgsPerSubject: 1},
			{Name: "PLATFORM", Subjects: []string{"platform.>"}},
			{Name: "COMPLIANCE", Subjects: []string{"compliance.breach.>"}},
			{Name: "MARKET", Subjects: []string{"market.>"}},
			{Name: "OBSERVABILITY", Subjects: []string{"observability.>"}},
		} {
			stream.Storage = nats.MemoryStorage
			if _, err := js.AddStream(&stream); err != nil {
				return fmt.Errorf("prepare %s: %w", stream.Name, err)
			}
		}
		return nil
	}
	if mode == "seed-prepare" {
		_, err := js.AddStream(&nats.StreamConfig{
			Name: stream, Subjects: []string{subject}, Storage: nats.MemoryStorage,
		})
		return err
	}
	// The live projection publishes other subjects into RISK. Require one
	// snapshot, not one message in the entire stream or a particular sequence.
	state, err := js.StreamInfo(stream, &nats.StreamInfoRequest{SubjectsFilter: subject})
	if err != nil {
		return err
	}
	if state.State.Subjects[subject] != 1 {
		return fmt.Errorf("expected one seed record on the tenant's snapshot subject")
	}
	record, err := js.GetLastMsg(stream, subject)
	if err != nil {
		return err
	}
	envelope, payload, err := bus.Unframe(record.Data)
	if err != nil {
		return err
	}
	if err := bus.Validate(envelope); err != nil {
		return err
	}
	portfolio := os.Getenv("ONBOARDING_PROBE_PORTFOLIO")
	if portfolio == "" {
		portfolio = "PF1"
	}
	var snapshot domainpb.PortfolioSnapshot
	if err := proto.Unmarshal(payload, &snapshot); err != nil {
		return err
	}
	if record.Subject != subject || envelope.GetTenantId() != tenant ||
		envelope.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_STATE_SNAPSHOT ||
		snapshot.GetPortfolio().GetPortfolioId() != portfolio || len(snapshot.GetPositions()) == 0 {
		return fmt.Errorf("unexpected tenant portfolio seed")
	}
	return nil
}

func TestOnboardingProbe(t *testing.T) {
	mode := os.Getenv("ONBOARDING_PROBE_MODE")
	switch mode {
	case "":
		t.Skip("requires an owned onboarding NATS resolver")
	case "create", "read", "seed-prepare", "seed-read", "projection-prepare":
	default:
		t.Fatal("unknown onboarding probe mode")
	}
	if err := verify(os.Getenv("ONBOARDING_PROBE_URL"), os.Getenv("ONBOARDING_PROBE_CREDS"),
		os.Getenv("ONBOARDING_PROBE_TENANT"), mode); err != nil {
		t.Fatal(err)
	}
}
