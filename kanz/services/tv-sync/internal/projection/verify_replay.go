package projection

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash"
	"sort"
	"time"

	tvsyncpb "github.com/eighred/kanz/kanz-schemas-go/tvsync/v1"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	ReplayMaxFacts           int64 = 100_000
	ReplayMaxBytes           int64 = 64 << 20
	ReplayMaxFactBytes       int64 = 64 << 10
	ReplayMaxCheckpointBytes int64 = 8 << 20
	ReplayDeadline                 = 120 * time.Second
)

var (
	ErrReplayInput        = errors.New("replay verification: invalid or unsupported input")
	ErrReplayDatabase     = errors.New("replay verification: database snapshot unavailable")
	ErrReplayLimit        = errors.New("replay verification: bounded domain exceeded")
	ErrReplayNoHistory    = errors.New("replay verification: no history is not restored-state proof")
	ErrReplayNoCheckpoint = errors.New("replay verification: checkpoint is absent")
	ErrReplayMismatch     = errors.New("replay verification: checkpoint and full fold differ")
	ErrReplayReadOnly     = errors.New("replay verification: writes are forbidden")
)

// ReplayProof describes only the verified immutable snapshot, never currentness
// after its transaction ends. A recovery operator must bind it to its archive,
// target database and reviewed image. Empty genesis is deliberately unqualified.
type ReplayProof struct {
	SchemaVersion    int       `json:"schema_version"`
	Status           string    `json:"status"`
	SourceVersion    string    `json:"source_version"`
	TenantSHA256     string    `json:"tenant_sha256"`
	SnapshotSHA256   string    `json:"snapshot_sha256"`
	InputSHA256      string    `json:"input_sha256"`
	StateSHA256      string    `json:"state_sha256"`
	CheckedAt        time.Time `json:"checked_at"`
	RetentionSeconds int64     `json:"retention_seconds"`
	FactCount        int64     `json:"fact_count"`
	Watermark        int64     `json:"watermark"`
	CheckpointSeq    int64     `json:"checkpoint_seq"`
}

type replaySnapshot struct {
	at                              time.Time
	count, watermark, checkpointSeq int64
	checkpoint                      []byte
	identity                        string
}

// VerifyPostgresSnapshot performs no application writes. Both rebuilds share a
// REPEATABLE READ READ ONLY transaction and the production Rehydrate path. SQL
// errors are classified without returning record contents or connection details.
func VerifyPostgresSnapshot(ctx context.Context, pool *pgxpool.Pool, tenant string, retention time.Duration) (ReplayProof, error) {
	if pool == nil || tenant == "" || len(tenant) > 128 || retention < 24*time.Hour || retention > 365*24*time.Hour {
		return ReplayProof{}, ErrReplayInput
	}
	ctx, cancel := context.WithTimeout(ctx, ReplayDeadline)
	defer cancel()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return ReplayProof{}, ErrReplayDatabase
	}
	// pgx closes a connection when a failed/cancelled rollback cannot restore
	// idle state; the pool releases it. Cleanup cannot outlive this deadline.
	defer func() { _ = tx.Rollback(ctx) }()
	snap, err := inspectReplaySnapshot(ctx, tx, tenant)
	if err != nil {
		return ReplayProof{}, err
	}
	proof, err := verifyCapturedReplay(ctx, &PostgresLog{db: tx}, tenant, retention, snap)
	if err != nil {
		return ReplayProof{}, err
	}
	// Rollback is the successful end too: this command has nothing to commit.
	if err := tx.Rollback(ctx); err != nil {
		return ReplayProof{}, ErrReplayDatabase
	}
	return proof, nil
}

func inspectReplaySnapshot(ctx context.Context, tx pgx.Tx, tenant string) (replaySnapshot, error) {
	var snap replaySnapshot
	var currentTenant, mode, isolation string
	var unsafe bool
	err := tx.QueryRow(ctx, `SELECT current_setting('app.tenant_id',true), current_setting('transaction_read_only'), current_setting('transaction_isolation'), transaction_timestamp(), txid_current_snapshot()::text, (SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname=current_user)`).Scan(&currentTenant, &mode, &isolation, &snap.at, &snap.identity, &unsafe)
	if err != nil {
		return snap, ErrReplayDatabase
	}
	if currentTenant != tenant || mode != "on" || isolation != "repeatable read" || unsafe {
		return snap, ErrReplayInput
	}
	var protected int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM pg_class WHERE oid IN ('tv_facts'::regclass,'tv_checkpoints'::regclass) AND relrowsecurity AND relforcerowsecurity`).Scan(&protected); err != nil {
		return snap, ErrReplayDatabase
	}
	if protected != 2 {
		return snap, ErrReplayInput
	}
	var bytes, maxBytes, maxID, maxType, foreignFacts, foreignCheckpoints, checkpointRows, checkpointBytes int64
	if err := tx.QueryRow(ctx, `SELECT count(*),coalesce(max(seq),0),coalesce(sum(octet_length(payload)::bigint+octet_length(event_id)+octet_length(event_type)),0),coalesce(max(octet_length(payload)),0),coalesce(max(octet_length(event_id)),0),coalesce(max(octet_length(event_type)),0),count(*) FILTER (WHERE tenant_id IS DISTINCT FROM $1) FROM tv_facts`, tenant).Scan(&snap.count, &snap.watermark, &bytes, &maxBytes, &maxID, &maxType, &foreignFacts); err != nil {
		return snap, ErrReplayDatabase
	}
	if foreignFacts != 0 {
		return snap, ErrReplayInput
	}
	if snap.count == 0 {
		return snap, ErrReplayNoHistory
	}
	if snap.count > ReplayMaxFacts || bytes > ReplayMaxBytes || maxBytes > ReplayMaxFactBytes || maxID > 1024 || maxType > 1024 {
		return snap, ErrReplayLimit
	}
	if err := tx.QueryRow(ctx, `SELECT count(*),coalesce(max(octet_length(payload)),0),count(*) FILTER (WHERE tenant_id IS DISTINCT FROM $1) FROM tv_checkpoints`, tenant).Scan(&checkpointRows, &checkpointBytes, &foreignCheckpoints); err != nil {
		return snap, ErrReplayDatabase
	}
	if foreignCheckpoints != 0 {
		return snap, ErrReplayInput
	}
	if checkpointRows == 0 {
		return snap, ErrReplayNoCheckpoint
	}
	if checkpointRows != 1 || checkpointBytes > ReplayMaxCheckpointBytes {
		return snap, ErrReplayLimit
	}
	var ok bool
	var errLoad error
	snap.checkpoint, snap.checkpointSeq, ok, errLoad = (&PostgresLog{db: tx}).LoadCheckpoint(ctx)
	if errLoad != nil || !ok {
		return snap, ErrReplayDatabase
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM tv_facts WHERE seq=$1)`, snap.checkpointSeq).Scan(&exists); err != nil {
		return snap, ErrReplayDatabase
	}
	if !exists || snap.checkpointSeq <= 0 || snap.checkpointSeq > snap.watermark {
		return snap, ErrReplayInput
	}
	return snap, nil
}

// readOnlyReplay changes only checkpoint selection. It does not implement a
// second fold and cannot append facts or save the comparison state.
type readOnlyReplay struct {
	source     Log
	checkpoint []byte
	seq        int64
	full       bool
	validate   func(Fact) error
}

func (l readOnlyReplay) Append(context.Context, Fact) (bool, int64, error) {
	return false, 0, ErrReplayReadOnly
}
func (l readOnlyReplay) SaveCheckpoint(context.Context, int64, []byte) error {
	return ErrReplayReadOnly
}
func (l readOnlyReplay) LoadCheckpoint(context.Context) ([]byte, int64, bool, error) {
	if l.full {
		return nil, 0, false, nil
	}
	return l.checkpoint, l.seq, true, nil
}
func (l readOnlyReplay) Replay(ctx context.Context, after int64, fn func(Fact) error) error {
	return l.source.Replay(ctx, after, func(f Fact) error {
		if err := l.validate(f); err != nil {
			return err
		}
		return fn(f)
	})
}

func verifyCapturedReplay(ctx context.Context, source Log, tenant string, retention time.Duration, snap replaySnapshot) (ReplayProof, error) {
	if snap.count <= 0 {
		return ReplayProof{}, ErrReplayNoHistory
	}
	if snap.count > ReplayMaxFacts || len(snap.identity) > 64<<10 {
		return ReplayProof{}, ErrReplayLimit
	}
	if err := validateReplayCheckpoint(snap, tenant, retention); err != nil {
		return ReplayProof{}, err
	}
	clock := func() time.Time { return snap.at }
	input := sha256.New()
	hashReplayPart(input, []byte(tenant))
	hashReplayPart(input, snap.checkpoint)
	count, last, bytes := int64(0), int64(0), int64(0)
	validator := func(f Fact) error {
		if err := ctx.Err(); err != nil {
			return ErrReplayDatabase
		}
		if f.Seq <= last || f.Seq > snap.watermark || f.EventID == "" || f.Knowledge.IsZero() || f.Knowledge.After(snap.at) {
			return ErrReplayInput
		}
		bytes += int64(len(f.Payload) + len(f.EventID) + len(f.EventType))
		if int64(len(f.Payload)) > ReplayMaxFactBytes || bytes > ReplayMaxBytes || count >= ReplayMaxFacts || len(f.EventID) > 1024 || len(f.EventType) > 1024 {
			return ErrReplayLimit
		}
		if err := validateReplayFact(f); err != nil {
			return err
		}
		last = f.Seq
		count++
		var number [8]byte
		binary.BigEndian.PutUint64(number[:], uint64(f.Seq))
		hashReplayPart(input, number[:])
		hashReplayPart(input, []byte(f.EventID))
		hashReplayPart(input, []byte(f.EventType))
		hashReplayPart(input, f.Payload)
		hashReplayPart(input, []byte(f.Knowledge.UTC().Format(time.RFC3339Nano)))
		return nil
	}
	fullLog := readOnlyReplay{source: source, full: true, validate: validator}
	full := New(clock, nil, WithLog(fullLog, tenant), WithRetention(retention))
	if err := full.Rehydrate(ctx); err != nil {
		return ReplayProof{}, classifyReplayError(err)
	}
	if count != snap.count || last != snap.watermark {
		return ReplayProof{}, ErrReplayInput
	}
	tailLog := readOnlyReplay{source: source, checkpoint: snap.checkpoint, seq: snap.checkpointSeq, validate: validateReplayFact}
	restored := New(clock, nil, WithLog(tailLog, tenant), WithRetention(retention))
	if err := restored.Rehydrate(ctx); err != nil {
		return ReplayProof{}, classifyReplayError(err)
	}
	full.Evict(snap.at)
	restored.Evict(snap.at)
	exactHash, err := compareReplayExact(full, restored)
	if err != nil {
		return ReplayProof{}, err
	}
	want, err := canonicalReplayState(full, snap, retention)
	if err != nil {
		return ReplayProof{}, ErrReplayInput
	}
	got, err := canonicalReplayState(restored, snap, retention)
	if err != nil {
		return ReplayProof{}, ErrReplayInput
	}
	if !proto.Equal(want, got) {
		return ReplayProof{}, ErrReplayMismatch
	}
	if proto.Size(want) > int(ReplayMaxBytes) {
		return ReplayProof{}, ErrReplayLimit
	}
	state, err := proto.MarshalOptions{Deterministic: true}.Marshal(want)
	if err != nil {
		return ReplayProof{}, ErrReplayInput
	}
	stateDigest := sha256.New()
	hashReplayPart(stateDigest, state)
	hashReplayPart(stateDigest, exactHash[:])
	stateHash := stateDigest.Sum(nil)
	tenantHash := sha256.Sum256([]byte(tenant))
	snapshotHash := sha256.Sum256([]byte(snap.identity))
	return ReplayProof{SchemaVersion: 1, Status: "VERIFIED", TenantSHA256: hex.EncodeToString(tenantHash[:]), SnapshotSHA256: hex.EncodeToString(snapshotHash[:]), InputSHA256: hex.EncodeToString(input.Sum(nil)), StateSHA256: hex.EncodeToString(stateHash), CheckedAt: snap.at.UTC(), RetentionSeconds: int64(retention / time.Second), FactCount: snap.count, Watermark: snap.watermark, CheckpointSeq: snap.checkpointSeq}, nil
}

func canonicalReplayState(p *Projection, snap replaySnapshot, retention time.Duration) (*tvsyncpb.Checkpoint, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	cp, err := p.snapshotLocked()
	if err != nil {
		return nil, err
	}
	// Watermark is independently bound to the SQL snapshot (checkpoint-only boot
	// does not advance Rehydrate's private last-tail seq). Compare at one declared
	// retention horizon, not differing historical eviction scheduler timestamps.
	cp.Seq = snap.watermark
	cp.TakenAt = timestamppb.New(snap.at)
	for _, a := range cp.Accounts {
		sort.Strings(a.SeenFills)
		a.RetainedFrom = timestamppb.New(snap.at.Add(-retention))
	}
	sort.Slice(cp.Accounts, func(i, j int) bool { return cp.Accounts[i].AccountId < cp.Accounts[j].AccountId })
	return cp, nil
}
func hashReplayPart(h hash.Hash, data []byte) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(data)))
	_, _ = h.Write(length[:])
	_, _ = h.Write(data)
}
func classifyReplayError(err error) error {
	for _, known := range []error{ErrReplayInput, ErrReplayLimit, ErrReplayMismatch, ErrReplayDatabase} {
		if errors.Is(err, known) {
			return known
		}
	}
	return ErrReplayDatabase
}
