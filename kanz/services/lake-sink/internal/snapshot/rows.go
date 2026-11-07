package snapshot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"time"

	lakepb "github.com/eighred/kanz/kanz-schemas-go/lake/v1"
	workerpb "github.com/eighred/kanz/kanz-schemas-go/lake/worker/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/lake-sink/internal/sink"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

const MaxRawFrameBytes = 16 << 20

// ConvertLanding projects a bounded persisted cut into tenant-pure worker input.
// Authorization must precede opening the cut. Every row's tenant and selection
// fields are checked against the original canonical envelope, never trusted from
// the landing JSON alone. Missing custody refuses rather than inventing history.
func ConvertLanding(ctx context.Context, tenant string, selection *lakepb.SnapshotSelection, files [][]byte) (*workerpb.WorkerRequest, error) {
	if !boundedText(tenant, 256) || ValidateSelection(selection) != nil || len(files) > MaxFiles {
		return nil, ErrInvalid
	}
	var inputBytes int
	for _, file := range files {
		if len(file) > MaxCaptureBytes-inputBytes {
			return nil, ErrCapacity
		}
		inputBytes += len(file)
		if len(file) > 0 && file[len(file)-1] != '\n' {
			return nil, ErrCustody
		}
	}
	types := make(map[string]bool, len(selection.EventTypes))
	for _, typ := range selection.EventTypes {
		types[typ] = true
	}
	converted := &workerpb.ConvertRows{Tenant: tenant, SchemaVersion: SchemaVersion}
	request := &workerpb.WorkerRequest{ProtocolVersion: ProtocolVersion, Operation: &workerpb.WorkerRequest_Convert{Convert: converted}}
	// Dense public ordinals preserve selected tenant-pure order and duplicates,
	// without exposing the positions or number of foreign landing rows.
	var scanned int
	var encodedRows int
	for _, file := range files {
		for len(file) != 0 {
			if scanned == MaxRows {
				return nil, ErrCapacity
			}
			scanned++
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			line, rest, found := bytes.Cut(file, []byte{'\n'})
			if !found || len(line) == 0 {
				return nil, ErrCustody
			}
			file = rest
			// Do not retain decoded financial payloads or free-form decoder errors.
			var row struct {
				EventID     string           `json:"event_id"`
				EventType   string           `json:"event_type"`
				TenantID    string           `json:"tenant_id"`
				EventTime   time.Time        `json:"event_time"`
				Payload     json.RawMessage  `json:"payload"`
				DecodeError string           `json:"decode_error"`
				Raw         *sink.RawCapture `json:"raw_capture"`
			}
			if json.Unmarshal(line, &row) != nil || row.Raw == nil || len(row.Raw.Frame) == 0 || row.Raw.SourceIdentity != nil {
				return nil, ErrCustody
			}
			if len(row.Raw.Frame) > MaxRawFrameBytes {
				return nil, ErrCapacity
			}
			sum := sha256.Sum256(row.Raw.Frame)
			if row.Raw.SHA256 != hex.EncodeToString(sum[:]) {
				return nil, ErrCustody
			}
			env, _, err := bus.Unframe(row.Raw.Frame)
			if err != nil || bus.Validate(env) != nil || !boundedText(env.TenantId, 256) || !boundedText(env.EventId, 256) || !boundedText(env.EventType, 256) {
				return nil, ErrCustody
			}
			if row.TenantID != env.TenantId || row.EventID != env.EventId || row.EventType != env.EventType || !row.EventTime.Equal(env.EventTime.AsTime()) {
				return nil, ErrCustody
			}
			if env.TenantId != tenant || !types[env.EventType] || env.EventTime.AsTime().Before(selection.EventTimeFrom.AsTime()) || !env.EventTime.AsTime().Before(selection.EventTimeUntil.AsTime()) {
				continue
			}
			if len(converted.Rows) == MaxRows {
				return nil, ErrCapacity
			}
			status := lakepb.SnapshotDecodeStatus_SNAPSHOT_DECODE_STATUS_UNAVAILABLE
			if row.DecodeError != "" {
				status = lakepb.SnapshotDecodeStatus_SNAPSHOT_DECODE_STATUS_FAILED
			} else if len(row.Payload) != 0 && !bytes.Equal(row.Payload, []byte("null")) {
				status = lakepb.SnapshotDecodeStatus_SNAPSHOT_DECODE_STATUS_DECODED
			}
			captured := &workerpb.CapturedRow{Ordinal: uint64(len(converted.Rows)), RawFrame: row.Raw.Frame, RawSha256: sum[:], EventId: env.EventId, EventType: env.EventType, EventTime: env.EventTime, DecodeStatus: status}
			if k := row.Raw.Kafka; k != nil {
				if !boundedText(k.Topic, 256) || k.Partition < 0 || k.Partition > math.MaxInt32 || k.Offset < 0 {
					return nil, ErrCustody
				}
				captured.Kafka = &workerpb.KafkaCoordinates{Topic: k.Topic, Partition: int32(k.Partition), Offset: k.Offset}
			}
			// Include each repeated-message tag and length prefix before retaining
			// the row. Final exact request size below covers outer framing fields.
			size := protowire.SizeTag(2) + protowire.SizeBytes(proto.Size(captured))
			if size > MaxCaptureBytes-encodedRows {
				return nil, ErrCapacity
			}
			encodedRows += size
			converted.Rows = append(converted.Rows, captured)
		}
	}
	if proto.Size(request) > MaxCaptureBytes {
		return nil, ErrCapacity
	}
	return request, nil
}
