//go:build linux

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"google.golang.org/protobuf/proto"
)

type sentinelTransport struct{ sent []bus.Message }

func (c *sentinelTransport) Publish(_ context.Context, message bus.Message) error {
	c.sent = append(c.sent, message)
	return nil
}

func (*sentinelTransport) Subscribe(context.Context, string, string, bus.Handler) error {
	return errors.New("sentinel transport is publish-only")
}

func (*sentinelTransport) Close() error { return nil }

func TestSentinelPublishesOwnedCanonicalPosition(t *testing.T) {
	const nonce = "fb693780-14fd-4666-af50-d99ed115e2a5"
	source := strings.Repeat("a", 40)
	client := &sentinelTransport{}
	producer, err := bus.NewProducer(client, sentinelProducerConfig(source, nonce))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 16, 0, 0, 0, 123, time.UTC)
	if err := producer.Publish(context.Background(), sentinelEvent(nonce, now)); err != nil {
		t.Fatal(err)
	}
	if len(client.sent) != 1 {
		t.Fatalf("sentinel transport received %d frames, want one", len(client.sent))
	}
	message := client.sent[0]
	const wantSubject = "risk.position.changed.position-proof-fb693780-14fd-4666-af50-d99ed115e2a5.unrelated.sentinel"
	if message.Subject != wantSubject || string(message.Key) != wantSubject {
		t.Fatal("sentinel escaped its exact unrelated POSITION key")
	}
	env, payload, err := bus.Unframe(message.Body)
	if err != nil {
		t.Fatal(err)
	}
	if err := bus.Validate(env); err != nil {
		t.Fatal(err)
	}
	if env.EventClass != envelopepb.EventClass_EVENT_CLASS_FACT || env.EventType != "risk.position.changed" || env.Domain != "risk" || env.SchemaVersion != 1 || env.PayloadSchemaRef != "domain.v1.PositionState:1" {
		t.Fatal("sentinel canonical FACT contract differs")
	}
	if env.Source != "fixture/position-reclaim-proof" || env.ProducerVersion != source || env.TenantId != "position-proof-"+nonce || env.PartitionKey != wantSubject || env.ProducerSequence != 1 || !env.EventTime.AsTime().Equal(now) {
		t.Fatal("sentinel source, tenant or partition ownership differs")
	}
	if env.EventId == "" || env.IdempotencyKey != env.EventId || message.Headers["Nats-Msg-Id"] != env.EventId {
		t.Fatal("sentinel transport idempotency is not its canonical FACT identity")
	}
	var state domainpb.PositionState
	if err := proto.Unmarshal(payload, &state); err != nil {
		t.Fatal(err)
	}
	if state.PortfolioId != "unrelated" || state.InstrumentId != "sentinel" || state.Venue != "" || state.Quantity == nil || state.Quantity.Coefficient != 7 || state.Quantity.Exponent != 0 || !state.AsOf.AsTime().Equal(now) {
		t.Fatal("sentinel payload changed its exact unrelated holding")
	}
	// The same production builder with a missing observation time must be refused
	// by the real Producer before the transport sees any additional frame.
	if err := producer.Publish(context.Background(), sentinelEvent(nonce, time.Time{})); err == nil || len(client.sent) != 1 {
		t.Fatal("invalid sentinel bypassed canonical validation before transport")
	}
}

func TestReceiptFilesRefuseLinksPipesAndOversize(t *testing.T) {
	dir := t.TempDir()
	ordinary := filepath.Join(dir, "ordinary")
	if err := os.WriteFile(ordinary, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(ordinary, filepath.Join(dir, "symlink")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "oversize"), make([]byte, 16385), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"symlink", "pipe", "oversize"} {
		if _, err := readReceipt(filepath.Join(dir, name)); err == nil {
			t.Errorf("accepted %s", name)
		}
	}
	if err := os.Link(ordinary, filepath.Join(dir, "hardlink")); err != nil {
		t.Fatal(err)
	}
	if _, err := readReceipt(ordinary); err == nil {
		t.Error("accepted multiply linked receipt")
	}
	if err := os.Symlink(dir, filepath.Join(dir, "parentlink")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := parentFD(filepath.Join(dir, "parentlink", "new")); err == nil {
		t.Error("followed linked parent")
	}
}

func TestReceiptExclusiveRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "receipt.json")
	want := receipt{Schema: 1, Phase: "seed", Source: "test"}
	if err := writeReceipt(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := readReceipt(path)
	if err != nil || got != want {
		t.Fatalf("round trip: %#v %v", got, err)
	}
	if err := writeReceipt(path, want); err == nil {
		t.Fatal("overwrote retained receipt")
	}
}
