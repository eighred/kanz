package cdc_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"google.golang.org/protobuf/proto"
	"testing"
)

func TestRawCaptureExactBytesAndFlush(t *testing.T) {
	e := env("")
	frame, err := proto.Marshal(&envelopepb.EventFrame{Envelope: e, Payload: []byte("opaque")})
	if err != nil {
		t.Fatal(err)
	}
	frame = append(frame, 0x0a, 0x00, 0xa0, 0x06, 0x81, 0x00)
	var decoded envelopepb.EventFrame
	if err := proto.Unmarshal(frame, &decoded); err != nil {
		t.Fatal(err)
	}
	normalized, err := proto.Marshal(&decoded)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(frame, normalized) {
		t.Fatal("fixture must not survive remarshal unchanged")
	}
	fs := &fakeSink{}
	es := newSink(nil, fs)
	delivery := bus.Delivery{Frame: frame, Kafka: &bus.KafkaDelivery{Topic: "observed", Partition: 0, Offset: 0}}
	if err := es.HandleDelivery(context.Background(), e, []byte("opaque"), delivery); err != nil {
		t.Fatal(err)
	}
	row := fs.rows[0]
	sum := sha256.Sum256(frame)
	if row.RawCapture == nil || !bytes.Equal(row.RawCapture.Frame, frame) || row.RawCapture.SHA256 != hex.EncodeToString(sum[:]) || row.RawCapture.Kafka == nil || row.RawCapture.Kafka.Offset != 0 || row.RawCapture.SourceIdentity != nil || fs.flushed != 1 {
		t.Fatal("raw capture/flush differs")
	}
	frame[0] ^= 0xff
	delivery.Kafka.Topic = "changed"
	if row.RawCapture.Kafka.Topic != "observed" || bytes.Equal(row.RawCapture.Frame, frame) {
		t.Fatal("aliased capture")
	}
	fs.flushErr = errors.New("sync failed")
	if err := es.HandleDelivery(context.Background(), e, nil, delivery); err == nil {
		t.Fatal("flush failure accepted")
	}
	if err := es.HandleDelivery(context.Background(), e, nil, bus.Delivery{}); err == nil {
		t.Fatal("absent bytes accepted")
	}
}
func TestRawDisabledAndUnknownCoordinates(t *testing.T) {
	fs := &fakeSink{}
	es := newSink(nil, fs)
	if err := es.Handle(context.Background(), env(""), nil); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(fs.rows[0])
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("raw_capture")) {
		t.Fatal("default output changed")
	}
	if err := es.HandleDelivery(context.Background(), env(""), nil, bus.Delivery{Frame: []byte{1}}); err != nil {
		t.Fatal(err)
	}
	raw := fs.rows[1].RawCapture
	if raw.Kafka != nil || raw.SourceIdentity != nil {
		t.Fatal("fabricated identity")
	}
}
