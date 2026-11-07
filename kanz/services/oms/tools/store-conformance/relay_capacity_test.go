package storeconformance

import (
	"bytes"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/outbox"
	"google.golang.org/protobuf/encoding/protowire"
)

// The queue's whole-page refusal remains unchanged. The relay may recover from
// that refusal by reading one record under its existing lock, never claiming an
// unresolved suffix was drained. These exercise real publication and SQL marks.
func TestRustRelayPageCapacity(t *testing.T) {
	for _, suffix := range []string{"valid", "oversized", "corrupt"} {
		t.Run(suffix, func(t *testing.T) {
			f := newFixture(t)
			b := newRelayBroker(t, f)
			key := f.schema
			first := relayRecord(t, key, 1)
			first.Payload = protowire.AppendBytes(protowire.AppendTag(first.Payload, 1000, protowire.BytesType), make([]byte, 600<<10))
			last := relayRecord(t, key, 2)
			switch suffix {
			case "valid":
				last.Payload = protowire.AppendBytes(protowire.AppendTag(last.Payload, 1000, protowire.BytesType), make([]byte, 600<<10))
			case "oversized":
				last.Payload = make([]byte, (1<<20)+1)
			case "corrupt":
				last.Payload = bytes.Repeat([]byte{0xff}, 600<<10)
			}
			enqueueRelay(t, f, outbox.ServiceOMS, first, last)
			page := f.queueRequest("pending")
			page["order_id"] = key
			page["limit"] = 10
			expect(t, run(t, page), "Capacity")
			since := time.Now()
			result := runResult(t, relayRequest(f, "flush", key, 10))
			if suffix == "valid" {
				relayResult(t, result, 2, "")
			} else if suffix == "corrupt" {
				relayResult(t, result, 1, "InvalidRecord")
			} else if result.Status != "error" || result.Error != "Capacity" || result.Sent != 1 || !result.SessionsClosed {
				t.Fatal("oversized suffix did not preserve acknowledged prefix and explicit refusal")
			}
			verifyRelayFrame(t, first, b.next(t, key), 1, since)
			rows := pendingRelay(t, f, key)
			if suffix == "valid" {
				verifyRelayFrame(t, last, b.next(t, key), 2, since)
				if len(rows) != 0 {
					t.Fatal("valid bounded records remain pending")
				}
			} else {
				attempts := 0
				if suffix == "corrupt" {
					attempts = 1
				}
				if len(rows) != 1 || rows[0].Attempts != attempts || !bytes.Equal(rows[0].Record.Payload, last.Payload) {
					t.Fatal("unresolved suffix was lost or mutated")
				}
			}
		})
	}
}
