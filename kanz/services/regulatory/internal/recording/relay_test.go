package recording

import (
	"context"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/outbox"
	"github.com/eighred/kanz/pkg/bus"
	"google.golang.org/protobuf/types/known/emptypb"
)

type countingPublication struct{ count int }

func (p *countingPublication) Publish(context.Context, bus.Event) error { p.count++; return nil }

// This is a queue-capacity model using the ACTUAL configured canonical relay,
// not broker/schema evidence. The real transaction/broker tests are separate.
func TestPublishMaximumRequestDoesNotTripRelayTailGuard(t *testing.T) {
	ctx := context.Background()
	for _, count := range []int{maxPublicationRecords, relayBatch} {
		queue := outbox.NewMemory()
		publisher := &countingPublication{}
		relay, err := newRelay(queue, publisher, nil)
		if err != nil {
			t.Fatal(err)
		}
		record, err := outbox.From(ctx, bus.Event{Subject: "compliance.filing.recorded", EventType: "compliance.filing.recorded", EventTime: time.Now(), PartitionKey: "acme/pf1", TenantID: "acme", SchemaVersion: 1, Payload: &emptypb.Empty{}})
		if err != nil {
			t.Fatal(err)
		}
		records := make([]outbox.Record, count)
		for i := range records {
			records[i] = record
		}
		if err := queue.Append(records...); err != nil {
			t.Fatal(err)
		}
		sent, err := relay.Flush(ctx, "acme/pf1")
		if sent != count || publisher.count != count || (err != nil) != (count == relayBatch) {
			t.Fatalf("configured tail policy count=%d sent=%d failed=%v", count, sent, err != nil)
		}
		if sent, err := relay.Flush(ctx, "acme/pf1"); err != nil || sent != 0 {
			t.Fatal("empty-tail confirmation did not recover")
		}
	}
}
