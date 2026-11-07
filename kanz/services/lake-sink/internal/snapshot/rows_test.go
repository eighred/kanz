package snapshot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	lakepb "github.com/eighred/kanz/kanz-schemas-go/lake/v1"
	"github.com/eighred/kanz/services/lake-sink/internal/sink"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestConvertLandingBoundsForeignRowsBeforeSelection(t *testing.T) {
	stamp := &timestamppb.Timestamp{Seconds: 1}
	env := &envelopepb.Envelope{EventId: "e", TenantId: "b", EventType: "a.b", EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, Domain: "a", SchemaVersion: 1, EnvelopeVersion: 1, EventTime: stamp, IngestionTime: stamp, PublishTime: stamp, CorrelationId: "c", Source: "s", ProducerVersion: "v", IdempotencyKey: "e", PayloadSchemaRef: "x:1"}
	frame, err := proto.Marshal(&envelopepb.EventFrame{Envelope: env})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(frame)
	line, err := json.Marshal(map[string]any{"event_id": "e", "tenant_id": "b", "event_type": "a.b", "event_time": stamp.AsTime(), "raw_capture": &sink.RawCapture{Frame: frame, SHA256: hex.EncodeToString(sum[:])}})
	if err != nil {
		t.Fatal(err)
	}
	line = append(line, '\n')
	selection := &lakepb.SnapshotSelection{EventTypes: []string{"a.b"}, EventTimeFrom: stamp, EventTimeUntil: &timestamppb.Timestamp{Seconds: 2}}
	control, err := ConvertLanding(context.Background(), "a", selection, [][]byte{line})
	if err != nil || len(control.GetConvert().Rows) != 0 {
		t.Fatal("valid foreign-row positive control refused")
	}
	input := bytes.Repeat(line, MaxRows+1)
	if len(input) >= MaxCaptureBytes {
		t.Fatal("row-limit fixture instead exceeds byte limit")
	}
	if _, err := ConvertLanding(context.Background(), "a", selection, [][]byte{input}); !errors.Is(err, ErrCapacity) {
		t.Fatal("foreign unselected rows bypassed scan limit")
	}
}

func custodyRow(t *testing.T, tenant string) sink.Row {
	t.Helper()
	stamp := timestamppb.New(time.Date(2026, 11, 7, 1, 2, 3, 987654321, time.UTC))
	env := &envelopepb.Envelope{EventId: "fixture-event", TenantId: tenant, EventType: "market.trade", EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, Domain: "market", SchemaVersion: 1, EnvelopeVersion: 1, EventTime: stamp, IngestionTime: stamp, PublishTime: stamp, CorrelationId: "fixture-correlation", Source: "fixture", ProducerVersion: "fixture", IdempotencyKey: "fixture-event", PayloadSchemaRef: "common.v1.Decimal:1"}
	frame, err := proto.Marshal(&envelopepb.EventFrame{Envelope: env, Payload: []byte{8, 2}})
	if err != nil {
		t.Fatal(err)
	}
	frame = append(frame, 0x0a, 0x00, 0xa0, 0x06, 0x81, 0x00)
	var decoded envelopepb.EventFrame
	if err := proto.Unmarshal(frame, &decoded); err != nil {
		t.Fatal(err)
	}
	normalized, err := proto.Marshal(&decoded)
	if err != nil || bytes.Equal(frame, normalized) {
		t.Fatal("fixture does not discriminate raw preservation")
	}
	sum := sha256.Sum256(frame)
	return sink.Row{EventID: env.EventId, EventType: env.EventType, TenantID: tenant, EventTime: stamp.AsTime(), RawCapture: &sink.RawCapture{Frame: frame, SHA256: hex.EncodeToString(sum[:]), Kafka: &sink.KafkaCoordinates{Topic: "market", Partition: 0, Offset: 0}}}
}

func landingBytes(t *testing.T, rows ...sink.Row) []byte {
	t.Helper()
	var b []byte
	for _, row := range rows {
		line, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		b = append(b, line...)
		b = append(b, '\n')
	}
	return b
}

func selectionFor(row sink.Row) *lakepb.SnapshotSelection {
	return &lakepb.SnapshotSelection{EventTypes: []string{row.EventType}, EventTimeFrom: timestamppb.New(row.EventTime), EventTimeUntil: timestamppb.New(row.EventTime.Add(time.Nanosecond))}
}

func TestConvertLandingTenantCustodyAndDuplicateOrdinal(t *testing.T) {
	row := custodyRow(t, "tenant-a")
	foreign := custodyRow(t, "tenant-b")
	request, err := ConvertLanding(context.Background(), "tenant-a", selectionFor(row), [][]byte{landingBytes(t, foreign, row, row)})
	if err != nil {
		t.Fatal(err)
	}
	rows := request.GetConvert().Rows
	if len(rows) != 2 || rows[0].Ordinal != 0 || rows[1].Ordinal != 1 || !bytes.Equal(rows[0].RawFrame, row.RawCapture.Frame) || rows[0].Kafka == nil || rows[0].Kafka.Offset != 0 || rows[0].EventTime.Nanos != 987654321 {
		t.Fatal("custody, duplicate order, coordinates or exact time changed")
	}
	baseline, err := ConvertLanding(context.Background(), "tenant-a", selectionFor(row), [][]byte{landingBytes(t, row, row)})
	if err != nil || !proto.Equal(request, baseline) {
		t.Fatal("foreign interleaving changed tenant-pure request")
	}
	interleaved, err := ConvertLanding(context.Background(), "tenant-a", selectionFor(row), [][]byte{landingBytes(t, row, foreign, foreign, row, foreign)})
	if err != nil || !proto.Equal(interleaved, baseline) {
		t.Fatal("foreign counts leaked through selected ordinal")
	}
	row.RawCapture.Kafka = nil
	row.DecodeError = "private decoder path and detail"
	request, err = ConvertLanding(context.Background(), "tenant-a", selectionFor(row), [][]byte{landingBytes(t, row)})
	if err != nil || request.GetConvert().Rows[0].Kafka != nil || request.GetConvert().Rows[0].DecodeStatus != lakepb.SnapshotDecodeStatus_SNAPSHOT_DECODE_STATUS_FAILED {
		t.Fatal("unknown coordinates or closed decode status changed")
	}
	wire, err := proto.Marshal(request)
	if err != nil || bytes.Contains(wire, []byte(row.DecodeError)) {
		t.Fatal("decoder detail escaped projection")
	}
}

func TestConvertLandingRefusesUntrustedProjection(t *testing.T) {
	for _, mutate := range []func(*sink.Row){
		func(r *sink.Row) { r.TenantID = "tenant-b" },
		func(r *sink.Row) { r.EventID = "another" },
		func(r *sink.Row) { r.EventTime = r.EventTime.Add(time.Nanosecond) },
		func(r *sink.Row) { r.RawCapture.SHA256 = "wrong" },
		func(r *sink.Row) { r.RawCapture.Frame[0] ^= 1 },
		func(r *sink.Row) { r.RawCapture = nil },
		func(r *sink.Row) { r.RawCapture.Kafka.Offset = -1 },
	} {
		row := custodyRow(t, "tenant-a")
		selection := selectionFor(row)
		mutate(&row)
		if _, err := ConvertLanding(context.Background(), "tenant-a", selection, [][]byte{landingBytes(t, row)}); err == nil {
			t.Fatal("invalid custody accepted")
		}
	}
}
