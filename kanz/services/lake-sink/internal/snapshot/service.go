package snapshot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	lakepb "github.com/eighred/kanz/kanz-schemas-go/lake/v1"
	workerpb "github.com/eighred/kanz/kanz-schemas-go/lake/worker/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/lake-sink/internal/sink"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// Server is registered only on the exact-gateway mTLS listener. Caller is never
// inferred from missing metadata, a workload identity, or a service default.
type Server struct {
	lakepb.UnimplementedLakeSnapshotServiceServer
	store       *Store
	worker      *Worker
	landing     *sink.FileSink
	landingRoot string
	outputRoot  string
	lifetime    context.Context
}

func NewServer(lifetime context.Context, store *Store, worker *Worker, landing *sink.FileSink, landingRoot, outputRoot string) (*Server, error) {
	if lifetime == nil || lifetime.Err() != nil || store == nil || worker == nil || landing == nil || landingRoot == "" || outputRoot == "" {
		return nil, ErrInvalid
	}
	return &Server{store: store, worker: worker, landing: landing, landingRoot: landingRoot, outputRoot: outputRoot, lifetime: lifetime}, nil
}

func rpcError(err error) error {
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrDenied):
		return status.Error(codes.NotFound, "snapshot unavailable")
	case errors.Is(err, ErrInvalid):
		return status.Error(codes.InvalidArgument, "invalid snapshot request")
	case errors.Is(err, ErrCapacity):
		return status.Error(codes.ResourceExhausted, "snapshot capacity exceeded")
	case errors.Is(err, ErrBusy):
		return status.Error(codes.ResourceExhausted, "snapshot operation busy")
	case errors.Is(err, ErrConflict):
		return status.Error(codes.FailedPrecondition, "snapshot state conflicts")
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "snapshot request cancelled")
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "snapshot request deadline")
	default:
		return status.Error(codes.Unavailable, "snapshot unavailable")
	}
}

func (s *Server) CreateSnapshot(ctx context.Context, req *lakepb.CreateSnapshotRequest) (*lakepb.CreateSnapshotResponse, error) {
	if req == nil {
		return nil, rpcError(ErrInvalid)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	snapshot, err := s.store.Create(ctx, req.Caller, req.Input)
	if err != nil {
		return nil, rpcError(err)
	}
	return &lakepb.CreateSnapshotResponse{Snapshot: snapshot}, nil
}

func (s *Server) GetSnapshot(ctx context.Context, req *lakepb.GetSnapshotRequest) (*lakepb.GetSnapshotResponse, error) {
	if req == nil {
		return nil, rpcError(ErrInvalid)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	snapshot, err := s.store.Get(ctx, req.Caller, req.SnapshotId)
	if err != nil {
		return nil, rpcError(err)
	}
	return &lakepb.GetSnapshotResponse{Snapshot: snapshot}, nil
}

func (s *Server) QuerySnapshot(ctx context.Context, req *lakepb.QuerySnapshotRequest) (*lakepb.QuerySnapshotResponse, error) {
	if req == nil {
		return nil, rpcError(ErrInvalid)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// Authorization and actual registry ownership precede every file open,
	// including cached READY generations and otherwise malformed queries.
	snapshot, err := s.store.Get(ctx, req.Caller, req.SnapshotId)
	if err != nil {
		return nil, rpcError(err)
	}
	if err := ValidateQuery(req.Query); err != nil {
		return nil, rpcError(err)
	}
	if snapshot.State != lakepb.SnapshotState_SNAPSHOT_STATE_READY {
		return nil, rpcError(ErrConflict)
	}
	op, err := s.worker.Reserve()
	if err != nil {
		return nil, rpcError(err)
	}
	defer op.Close()
	var name string
	var manifestWire, parquetHash, requestHash, cut []byte
	err = s.store.pool.QueryRow(ctx, `SELECT output_name,manifest,parquet_sha256,request_sha256,cut FROM lake_snapshots WHERE tenant_id=$1 AND snapshot_id=$2 AND state=3`, s.store.tenant, req.SnapshotId).Scan(&name, &manifestWire, &parquetHash, &requestHash, &cut)
	if err != nil {
		return nil, rpcError(err)
	}
	if len(manifestWire) > MaxControlBytes || len(parquetHash) != 32 || len(requestHash) != 32 || len(cut) == 0 || len(cut) > MaxControlBytes {
		return nil, rpcError(ErrCustody)
	}
	manifestHash := sha256.Sum256(manifestWire)
	if !bytes.Equal(manifestHash[:], snapshot.ManifestSha256) {
		return nil, rpcError(ErrCustody)
	}
	var manifest Manifest
	decoder := json.NewDecoder(bytes.NewReader(manifestWire))
	decoder.DisallowUnknownFields()
	cutHash := sha256.Sum256(cut)
	if decoder.Decode(&manifest) != nil || decoder.Decode(new(any)) != io.EOF || manifest.SchemaVersion != SchemaVersion || manifest.SourceScope != 1 || manifest.Tenant != s.store.tenant || manifest.SnapshotID != req.SnapshotId || manifest.Rows != snapshot.RowCount || manifest.ParquetBytes != snapshot.ParquetBytes || !bytes.Equal(manifest.ParquetSHA256[:], parquetHash) || !bytes.Equal(manifest.RequestSHA256[:], requestHash) || manifest.CutSHA256 != cutHash {
		return nil, rpcError(ErrCustody)
	}
	diskManifest, err := readArtifactFile(ctx, s.outputRoot, name, "manifest.json", MaxControlBytes)
	if err != nil || !bytes.Equal(diskManifest, manifestWire) {
		return nil, rpcError(ErrCustody)
	}
	parquet, err := readArtifactFile(ctx, s.outputRoot, name, "events.parquet", MaxParquetBytes)
	if err != nil {
		return nil, rpcError(err)
	}
	actual := sha256.Sum256(parquet)
	if !bytes.Equal(actual[:], parquetHash) || uint64(len(parquet)) != manifest.ParquetBytes {
		return nil, rpcError(ErrCustody)
	}
	response, err := op.Invoke(ctx, &workerpb.WorkerRequest{ProtocolVersion: ProtocolVersion, Operation: &workerpb.WorkerRequest_Query{Query: &workerpb.QueryControl{ParquetBytes: manifest.ParquetBytes, ParquetSha256: parquetHash, Query: req.Query, ExpectedTenant: s.store.tenant, ExpectedRowCount: manifest.Rows, SchemaVersion: SchemaVersion}}}, parquet)
	if err != nil {
		return nil, rpcError(err)
	}
	return &lakepb.QuerySnapshotResponse{Result: response.GetQuery()}, nil
}

// HandleRequested runs behind the existing canonical bus Consumer validation and
// retained delivery policy. It rechecks tenant and exact accepted request hash;
// the FACT does not supply a path, tenant grant, or replacement selection.
func (s *Server) HandleRequested(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
	if bus.RequireTenantScope(env.GetTenantId(), s.store.tenant) != nil {
		return ErrInvalid
	}
	if bus.Validate(env) != nil || env.TenantId != s.store.tenant || env.EventType != SubjectRequested || env.EventClass != envelopepb.EventClass_EVENT_CLASS_FACT || len(payload) > MaxControlBytes {
		return ErrInvalid
	}
	var fact lakepb.SnapshotLifecycle
	if proto.Unmarshal(payload, &fact) != nil || fact.State != lakepb.SnapshotState_SNAPSHOT_STATE_REQUESTED || fact.SourceScope != lakepb.SnapshotSourceScope_SNAPSHOT_SOURCE_SCOPE_ACTIVE_LANDING_RUN || len(fact.RequestSha256) != 32 || env.PartitionKey != fact.SnapshotId {
		return ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	registered, err := scanSnapshot(s.store.pool.QueryRow(ctx, `SELECT `+snapshotColumns+` FROM lake_snapshots WHERE tenant_id=$1 AND snapshot_id=$2`, s.store.tenant, fact.SnapshotId))
	if err != nil {
		return err
	}
	if !bytes.Equal(registered.requestHash, fact.RequestSha256) {
		return ErrCustody
	}
	if registered.public.State == lakepb.SnapshotState_SNAPSHOT_STATE_READY || registered.public.State == lakepb.SnapshotState_SNAPSHOT_STATE_FAILED {
		return nil
	}
	op, err := s.worker.Reserve()
	if err != nil {
		return err
	}
	defer op.Close()
	lease, err := s.store.Acquire(ctx, fact.SnapshotId)
	if err != nil {
		return err
	}
	defer func() { op.afterChildJoined(func() { _ = lease.Close() }) }()
	cut, err := lease.Cut()
	if err != nil {
		return err
	}
	var files [][]byte
	if cut == nil {
		captured, contents, err := s.landing.CaptureCut(ctx)
		if err != nil {
			return lease.Fail(ctx, lakepb.SnapshotFailure_SNAPSHOT_FAILURE_INPUT_UNAVAILABLE)
		}
		if err := lease.PersistCut(ctx, captured); err != nil {
			return err
		}
		files = contents
	} else {
		files, err = sink.ReadLandingCut(ctx, s.landingRoot, *cut)
		if err != nil {
			return lease.Fail(ctx, lakepb.SnapshotFailure_SNAPSHOT_FAILURE_INPUT_UNAVAILABLE)
		}
	}
	request, err := ConvertLanding(ctx, s.store.tenant, lease.row.public.Selection, files)
	if err != nil {
		reason := lakepb.SnapshotFailure_SNAPSHOT_FAILURE_INVALID_CUSTODY
		if errors.Is(err, ErrCapacity) {
			reason = lakepb.SnapshotFailure_SNAPSHOT_FAILURE_CAPACITY
		}
		return lease.Fail(ctx, reason)
	}
	var response *workerpb.WorkerResponse
	err = lease.RunWorker(ctx, func(workCtx context.Context) error {
		var invokeErr error
		response, invokeErr = op.Invoke(workCtx, request, nil)
		return invokeErr
	})
	if err != nil {
		return err
	}
	converted := response.GetConverted()
	manifest, name, err := lease.PreparePublication(converted.Parquet, converted.RowCount)
	if err != nil {
		return err
	}
	publication, err := PublishArtifact(ctx, s.outputRoot, name, manifest, converted.Parquet)
	if err != nil {
		return err
	}
	return lease.Publish(ctx, publication)
}
