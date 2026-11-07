package snapshot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	"github.com/eighred/kanz/internal/outbox"
	lakepb "github.com/eighred/kanz/kanz-schemas-go/lake/v1"
	"github.com/eighred/kanz/services/lake-sink/internal/sink"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Lease is single-goroutine owned. Its connection is never concurrently used by
// heartbeat and SQL; RunWorker joins the liveness loop before returning it.
// Close destroys the dedicated connection rather than returning a session lock
// to the pool. The shared generation CAS remains decisive after ambiguous I/O.
type Lease struct {
	connection *pgxpool.Conn
	store      *Store
	row        *storedSnapshot
	cut        []byte
}

func (s *Store) Acquire(ctx context.Context, id string) (_ *Lease, err error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	l := &Lease{connection: conn, store: s}
	defer func() {
		if err != nil {
			_ = l.Close()
		}
	}()
	_, err = conn.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended(current_database() || '/' || ('lake_snapshots'::regclass)::oid::text || '/' || app_current_tenant() || '/lake-snapshot/' || $1,0))`, id)
	if err != nil {
		return nil, err
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	l.row, err = scanSnapshot(tx.QueryRow(ctx, `SELECT `+snapshotColumns+` FROM lake_snapshots WHERE tenant_id=$1 AND snapshot_id=$2 FOR UPDATE`, s.tenant, id))
	if err != nil {
		return nil, err
	}
	if l.row.public.State == lakepb.SnapshotState_SNAPSHOT_STATE_READY || l.row.public.State == lakepb.SnapshotState_SNAPSHOT_STATE_FAILED {
		return nil, ErrConflict
	}
	if l.row.generation >= 3 {
		if _, err = tx.Exec(ctx, `UPDATE lake_snapshots SET state=4,failure=3 WHERE tenant_id=$1 AND snapshot_id=$2 AND generation=$3 AND state IN (1,2)`, s.tenant, id, l.row.generation); err != nil {
			return nil, err
		}
		l.row.public.State = lakepb.SnapshotState_SNAPSHOT_STATE_FAILED
		l.row.public.Failure = lakepb.SnapshotFailure_SNAPSHOT_FAILURE_CAPACITY
		record, err := lifecycleRecord(ctx, s.tenant, l.row.public, l.row.requestHash, l.row.generation, time.Now().UTC())
		if err != nil {
			return nil, err
		}
		if err := outbox.Enqueue(ctx, tx, outbox.ServiceLakeSink, record); err != nil {
			return nil, err
		}
		if err = tx.Commit(ctx); err != nil {
			return nil, err
		}
		return nil, ErrCapacity
	}
	err = tx.QueryRow(ctx, `UPDATE lake_snapshots SET state=2,generation=generation+1 WHERE tenant_id=$1 AND snapshot_id=$2 AND generation=$3 RETURNING generation,cut`, s.tenant, id, l.row.generation).Scan(&l.row.generation, &l.cut)
	if err != nil {
		return nil, err
	}
	l.row.public.State = lakepb.SnapshotState_SNAPSHOT_STATE_CAPTURING
	record, err := lifecycleRecord(ctx, s.tenant, l.row.public, l.row.requestHash, l.row.generation, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	if err := outbox.Enqueue(ctx, tx, outbox.ServiceLakeSink, record); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return l, nil
}

// Cut returns the first persisted cut, if one exists. The caller must replay it
// rather than asking a restarted FileSink for a different active run.
func (l *Lease) Cut() (*sink.LandingCut, error) {
	if l.cut == nil {
		return nil, nil
	}
	if len(l.cut) > MaxControlBytes {
		return nil, ErrCustody
	}
	var cut sink.LandingCut
	if json.Unmarshal(l.cut, &cut) != nil {
		return nil, ErrCustody
	}
	return &cut, nil
}

func (l *Lease) PersistCut(ctx context.Context, cut sink.LandingCut) error {
	wire, err := json.Marshal(cut)
	if err != nil || len(wire) > MaxControlBytes {
		return ErrCapacity
	}
	if l.cut != nil {
		if !bytes.Equal(wire, l.cut) {
			return ErrConflict
		}
		return nil
	}
	tag, err := l.connection.Exec(ctx, `UPDATE lake_snapshots SET cut=$1 WHERE tenant_id=$2 AND snapshot_id=$3 AND generation=$4 AND state=2 AND cut IS NULL`, wire, l.store.tenant, l.row.public.SnapshotId, l.row.generation)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	l.cut = wire
	return nil
}

// RunWorker checks only connection liveness, not domain state. Detection is
// bounded rather than instantaneous; on observed loss it cancels work and waits
// for the worker's bounded kill/join before releasing the lease. Every phase
// inherits the same caller deadline, including each liveness exchange.
func (l *Lease) RunWorker(ctx context.Context, work func(context.Context) error) error {
	if _, ok := ctx.Deadline(); !ok || ctx.Err() != nil {
		return ErrInvalid
	}
	workCtx, cancelWork := context.WithCancel(ctx)
	defer cancelWork()
	stopHeartbeat := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stopHeartbeat:
				done <- nil
				return
			case <-ctx.Done():
				done <- nil
				return
			case <-ticker.C:
				probeCtx, cancel := context.WithTimeout(ctx, time.Second)
				var one int
				err := l.connection.QueryRow(probeCtx, `SELECT 1`).Scan(&one)
				cancel()
				if err != nil || one != 1 {
					if ctx.Err() != nil {
						done <- nil
						return
					}
					cancelWork()
					done <- ErrConflict
					return
				}
			}
		}
	}()
	err := work(workCtx)
	// A normal completion does not cancel an in-flight pgx exchange: that can
	// destroy a healthy connection. Join its already bounded probe instead.
	close(stopHeartbeat)
	if heartbeatErr := <-done; heartbeatErr != nil {
		return heartbeatErr
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

type Publication struct {
	Manifest      []byte
	ParquetSHA256 [32]byte
	OutputName    string
	Rows          uint64
	Bytes         uint64
}

// Publish follows fsync and no-replace immutable output publication. The final
// transaction uses this same fenced connection; a stale generation cannot make
// its orphan generation files discoverable through the registry.
func (l *Lease) Publish(ctx context.Context, p Publication) error {
	if l.cut == nil || len(p.Manifest) == 0 || len(p.Manifest) > MaxControlBytes || !idPattern.MatchString(p.OutputName) || p.Rows > MaxRows || p.Bytes == 0 || p.Bytes > MaxParquetBytes {
		return ErrInvalid
	}
	var manifest Manifest
	if json.Unmarshal(p.Manifest, &manifest) != nil || manifest.SchemaVersion != SchemaVersion || manifest.SourceScope != 1 || manifest.Tenant != l.store.tenant || manifest.SnapshotID != l.row.public.SnapshotId || !bytes.Equal(manifest.RequestSHA256[:], l.row.requestHash) || manifest.CutSHA256 != sha256.Sum256(l.cut) || manifest.ParquetSHA256 != p.ParquetSHA256 || manifest.Rows != p.Rows || manifest.ParquetBytes != p.Bytes || p.OutputName != fmt.Sprintf("%s-%d", l.row.public.SnapshotId, l.row.generation) {
		return ErrCustody
	}
	canonical, err := json.Marshal(manifest)
	if err != nil || !bytes.Equal(canonical, p.Manifest) {
		return ErrCustody
	}
	hash := sha256.Sum256(p.Manifest)
	at := time.Now().UTC().Truncate(time.Microsecond)
	tx, err := l.connection.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `UPDATE lake_snapshots SET state=3,manifest=$1,manifest_sha256=$2,parquet_sha256=$3,output_name=$4,row_count=$5,parquet_bytes=$6,sealed_at=$7
 WHERE tenant_id=$8 AND snapshot_id=$9 AND generation=$10 AND state=2 AND cut=$11`, p.Manifest, hash[:], p.ParquetSHA256[:], p.OutputName, int64(p.Rows), int64(p.Bytes), at, l.store.tenant, l.row.public.SnapshotId, l.row.generation, l.cut)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	l.row.public.State = lakepb.SnapshotState_SNAPSHOT_STATE_READY
	l.row.public.ManifestSha256 = hash[:]
	l.row.public.RowCount = p.Rows
	l.row.public.ParquetBytes = p.Bytes
	l.row.public.SealedAt = timestamppb.New(at)
	record, err := lifecycleRecord(ctx, l.store.tenant, l.row.public, l.row.requestHash, l.row.generation, at)
	if err != nil {
		return err
	}
	if err := outbox.Enqueue(ctx, tx, outbox.ServiceLakeSink, record); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (l *Lease) Fail(ctx context.Context, reason lakepb.SnapshotFailure) error {
	if reason < 1 || reason > 5 {
		return ErrInvalid
	}
	tx, err := l.connection.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `UPDATE lake_snapshots SET state=4,failure=$1 WHERE tenant_id=$2 AND snapshot_id=$3 AND generation=$4 AND state=2`, int32(reason), l.store.tenant, l.row.public.SnapshotId, l.row.generation)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	l.row.public.State = lakepb.SnapshotState_SNAPSHOT_STATE_FAILED
	l.row.public.Failure = reason
	record, err := lifecycleRecord(ctx, l.store.tenant, l.row.public, l.row.requestHash, l.row.generation, time.Now().UTC())
	if err != nil {
		return err
	}
	if err := outbox.Enqueue(ctx, tx, outbox.ServiceLakeSink, record); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (l *Lease) Close() error {
	if l.connection == nil {
		return nil
	}
	conn := l.connection.Hijack()
	l.connection = nil
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return conn.Close(ctx)
}
