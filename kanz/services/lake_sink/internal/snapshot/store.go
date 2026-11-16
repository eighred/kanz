package snapshot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"time"

	"github.com/eighred/kanz/internal/lake/snapshotprofile"
	"github.com/eighred/kanz/internal/outbox"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	lakepb "github.com/eighred/kanz/kanz-schemas-go/lake/v1"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const Service = outbox.ServiceLakeSink
const SubjectRequested = snapshotprofile.SubjectRequested

// Store serves exactly one configured tenant through its canonical tenant pool.
// The pool is never selected using a caller field. Admission precedes SQL.
type Store struct {
	pool       *pgxpool.Pool
	tenant     string
	authorizer auth.Authorizer
}

func NewStore(pool *pgxpool.Pool, tenant string, authorizer auth.Authorizer) (*Store, error) {
	if pool == nil || !boundedText(tenant, 256) || authorizer == nil {
		return nil, ErrInvalid
	}
	return &Store{pool: pool, tenant: tenant, authorizer: authorizer}, nil
}

func (s *Store) authorize(ctx context.Context, caller *lakepb.GatewayCaller, action auth.Action, id string) error {
	p, err := Caller(caller)
	if err != nil {
		return ErrNotFound
	}
	return Authorize(ctx, s.authorizer, p, action, id, s.tenant)
}

type storedSnapshot struct {
	public                   *lakepb.Snapshot
	requestWire, requestHash []byte
	generation               int64
	requestedBy              *lakepb.GatewayCaller
}

const snapshotColumns = `snapshot_id,state,request_wire,request_sha256,generation,manifest_sha256,row_count,parquet_bytes,created_at,sealed_at,failure,source_scope,requested_by`

func scanSnapshot(row pgx.Row) (*storedSnapshot, error) {
	var r storedSnapshot
	var state, failure, scope int32
	var rows, parquet int64
	var created time.Time
	var sealed *time.Time
	r.public = &lakepb.Snapshot{Custody: lakepb.SnapshotCustody_SNAPSHOT_CUSTODY_LANDED_ROWS_ONLY}
	var provenance []byte
	err := row.Scan(&r.public.SnapshotId, &state, &r.requestWire, &r.requestHash, &r.generation, &r.public.ManifestSha256, &rows, &parquet, &created, &sealed, &failure, &scope, &provenance)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.requestedBy = &lakepb.GatewayCaller{}
	if len(provenance) > 16384 || proto.Unmarshal(provenance, r.requestedBy) != nil {
		return nil, ErrCustody
	}
	if _, err := Caller(r.requestedBy); err != nil {
		return nil, ErrCustody
	}
	var input lakepb.CreateSnapshotInput
	if len(r.requestWire) > MaxControlBytes || proto.Unmarshal(r.requestWire, &input) != nil || ValidateCreate(&input) != nil || len(r.requestHash) != 32 || r.generation < 0 || rows < 0 || rows > MaxRows || parquet < 0 || parquet > MaxParquetBytes || state < 1 || state > 4 || scope != 1 || failure < 0 || failure > 5 {
		return nil, ErrCustody
	}
	hash := sha256.Sum256(r.requestWire)
	if !bytes.Equal(hash[:], r.requestHash) {
		return nil, ErrCustody
	}
	r.public.Selection = input.Selection
	r.public.State = lakepb.SnapshotState(state)
	r.public.Failure = lakepb.SnapshotFailure(failure)
	r.public.SourceScope = lakepb.SnapshotSourceScope(scope)
	r.public.RowCount, r.public.ParquetBytes = uint64(rows), uint64(parquet)
	r.public.CreatedAt = timestamppb.New(created)
	if sealed != nil {
		r.public.SealedAt = timestamppb.New(*sealed)
	}
	if r.public.CreatedAt.CheckValid() != nil || (r.public.SealedAt != nil && r.public.SealedAt.CheckValid() != nil) {
		return nil, ErrCustody
	}
	return &r, nil
}

func (s *Store) Get(ctx context.Context, caller *lakepb.GatewayCaller, id string) (*lakepb.Snapshot, error) {
	if err := s.authorize(ctx, caller, ActionRead, id); err != nil {
		return nil, err
	}
	if _, err := uuid.Parse(id); err != nil {
		return nil, ErrNotFound
	}
	r, err := scanSnapshot(s.pool.QueryRow(ctx, `SELECT `+snapshotColumns+` FROM lake_snapshots WHERE snapshot_id=$1 AND tenant_id=$2`, id, s.tenant))
	if err != nil {
		return nil, err
	}
	return r.public, nil
}

// Create atomically commits the accepted resource, authenticated provenance and
// Requested FACT. A lost commit response is resolved by the same request ID and
// exact input bytes on retry; no landing file is opened by this transaction.
func (s *Store) Create(ctx context.Context, caller *lakepb.GatewayCaller, input *lakepb.CreateSnapshotInput) (*lakepb.Snapshot, error) {
	if err := s.authorize(ctx, caller, ActionCreate, ""); err != nil {
		return nil, err
	}
	if err := ValidateCreate(input); err != nil {
		return nil, err
	}
	wire, err := (proto.MarshalOptions{Deterministic: true}).Marshal(input)
	if err != nil {
		return nil, ErrInvalid
	}
	provenance, err := (proto.MarshalOptions{Deterministic: true}).Marshal(caller)
	if err != nil || len(provenance) > 16384 {
		return nil, ErrInvalid
	}
	hash := sha256.Sum256(wire)
	id, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}
	stamp := time.Now().UTC().Truncate(time.Microsecond)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	// Serialize only resource admission, scoped to this physical registry and
	// tenant. Reserve a full derived-output budget per accepted request, even
	// after FAILED; no age-based deletion silently renews capacity.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(current_database() || '/' || ('lake_snapshots'::regclass)::oid::text || '/' || app_current_tenant() || '/lake-admission',0))`); err != nil {
		return nil, err
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM lake_snapshots WHERE tenant_id=$1 AND request_id=$2)`, s.tenant, input.RequestId).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		var retained, pending int64
		if err := tx.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE state IN (1,2)) FROM lake_snapshots WHERE tenant_id=$1`, s.tenant).Scan(&retained, &pending); err != nil {
			return nil, err
		}
		if retained >= 16 || pending >= 8 {
			return nil, ErrCapacity
		}
	}
	tag, err := tx.Exec(ctx, `INSERT INTO lake_snapshots (tenant_id,snapshot_id,request_id,request_wire,request_sha256,requested_by,state,source_scope,created_at)
 VALUES ($1,$2,$3,$4,$5,$6,1,1,$7) ON CONFLICT (tenant_id,request_id) DO NOTHING`, s.tenant, id.String(), input.RequestId, wire, hash[:], provenance, stamp)
	if err != nil {
		return nil, err
	}
	r, err := scanSnapshot(tx.QueryRow(ctx, `SELECT `+snapshotColumns+` FROM lake_snapshots WHERE tenant_id=$1 AND request_id=$2`, s.tenant, input.RequestId))
	if err != nil {
		return nil, err
	}
	if r.requestedBy.Subject != caller.Subject || r.requestedBy.Tenant != caller.Tenant {
		return nil, ErrNotFound
	}
	if !bytes.Equal(r.requestWire, wire) || !bytes.Equal(r.requestHash, hash[:]) {
		return nil, ErrConflict
	}
	if tag.RowsAffected() == 1 {
		record, err := lifecycleRecord(ctx, s.tenant, r.public, hash[:], 0, stamp)
		if err != nil {
			return nil, err
		}
		if err := outbox.Enqueue(ctx, tx, outbox.ServiceLakeSink, record); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	// Create is acceptance, not an implicit progress/read grant. Every retry
	// returns the same initial projection; Get/Query require ActionRead.
	return initialAcceptance(r.public), nil
}

func initialAcceptance(snapshot *lakepb.Snapshot) *lakepb.Snapshot {
	return &lakepb.Snapshot{SnapshotId: snapshot.SnapshotId, State: lakepb.SnapshotState_SNAPSHOT_STATE_REQUESTED, Selection: proto.Clone(snapshot.Selection).(*lakepb.SnapshotSelection), CreatedAt: proto.Clone(snapshot.CreatedAt).(*timestamppb.Timestamp), Custody: lakepb.SnapshotCustody_SNAPSHOT_CUSTODY_LANDED_ROWS_ONLY, SourceScope: lakepb.SnapshotSourceScope_SNAPSHOT_SOURCE_SCOPE_ACTIVE_LANDING_RUN}
}

func lifecycleRecord(ctx context.Context, tenant string, snapshot *lakepb.Snapshot, requestHash []byte, generation int64, at time.Time) (outbox.Record, error) {
	subject := SubjectRequested
	switch snapshot.State {
	case lakepb.SnapshotState_SNAPSHOT_STATE_REQUESTED:
	case lakepb.SnapshotState_SNAPSHOT_STATE_CAPTURING:
		subject = snapshotprofile.SubjectCapturing
	case lakepb.SnapshotState_SNAPSHOT_STATE_READY:
		subject = snapshotprofile.SubjectPublished
	case lakepb.SnapshotState_SNAPSHOT_STATE_FAILED:
		subject = snapshotprofile.SubjectFailed
	default:
		return outbox.Record{}, ErrInvalid
	}
	fact := &lakepb.SnapshotLifecycle{SnapshotId: snapshot.SnapshotId, State: snapshot.State, RequestSha256: requestHash, ManifestSha256: snapshot.ManifestSha256, RowCount: snapshot.RowCount, Generation: uint64(generation), At: timestamppb.New(at), Failure: snapshot.Failure, SourceScope: lakepb.SnapshotSourceScope_SNAPSHOT_SOURCE_SCOPE_ACTIVE_LANDING_RUN}
	record, err := outbox.From(ctx, bus.Event{Subject: subject, EventType: subject, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, Domain: "lake", EventTime: at, PartitionKey: snapshot.SnapshotId, TenantID: tenant, Payload: fact})
	if err != nil {
		return outbox.Record{}, err
	}
	return record, nil
}
