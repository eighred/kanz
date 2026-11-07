//go:build linux && amd64

package snapshot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/lake/snapshotprofile"
	lakepb "github.com/eighred/kanz/kanz-schemas-go/lake/v1"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/eighred/kanz/services/lake-sink/internal/sink"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestSnapshotActualOwnerKillReplaysPersistedCut(t *testing.T) {
	worker, workerHash := actualWorker(t)
	a, _ := snapshotStores(t)
	b := newSnapshotBroker(t)
	ctx, cancel := context.WithTimeout(context.Background(), snapshotCrashFixtureLifetime)
	defer cancel()
	admin, err := b.connect(b.adminA)
	if err != nil {
		t.Fatal("fixture broker unavailable")
	}
	defer admin.Close()
	js, err := jetstream.New(admin)
	if err != nil {
		t.Fatal("fixture JetStream unavailable")
	}
	if _, err = js.CreateStream(ctx, snapshotprofile.Stream()); err != nil {
		t.Fatal("fixture stream unavailable")
	}
	provisionSnapshotRegisteredDependencies(t, ctx, js)
	landingRoot, outputRoot := t.TempDir(), t.TempDir()
	policy := filepath.Join(t.TempDir(), "policy.json")
	if os.WriteFile(policy, []byte(`{"roles":{"creator":["lake.snapshot.create"],"reader":["lake.snapshot.read"]}}`), 0600) != nil {
		t.Fatal("fixture policy unavailable")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("fixture address unavailable")
	}
	address := listener.Addr().String()
	_ = listener.Close()
	leaf, err := b.source.GetX509SVID()
	if err != nil {
		t.Fatal("fixture identity unavailable")
	}
	cert, key, err := leaf.Marshal()
	if err != nil {
		t.Fatal("fixture identity encoding failed")
	}
	bundle, err := b.source.GetX509BundleForTrustDomain(leaf.ID.TrustDomain())
	if err != nil {
		t.Fatal("fixture trust unavailable")
	}
	trust, err := bundle.Marshal()
	if err != nil {
		t.Fatal("fixture trust encoding failed")
	}
	row := custodyRow(t, "tenant-a")
	var schema string
	if a.pool.QueryRow(ctx, `SELECT current_schema()`).Scan(&schema) != nil {
		t.Fatal("owned schema unavailable")
	}
	application := "lake-crash-" + schema
	childDSN, err := snapshotFixtureDSN(a.pool.Config().ConnConfig.ConnString(), schema, application)
	if err != nil {
		t.Fatal("child scoped database URL unavailable")
	}
	in := snapshotOwnerInput{Address: address, Database: childDSN, NATS: "nats://" + b.address, Landing: landingRoot, Output: outputRoot, Policy: policy, Worker: worker, WorkerSHA: workerHash, Cert: cert, Key: key, Bundle: trust, Rows: []sink.Row{row, custodyRow(t, "tenant-b"), row}}
	// A fixture-only trigger blocks the real READY transaction after immutable
	// filesystem publication. No owner hook fabricates completion or rollback.
	if _, err = a.pool.Exec(ctx, `CREATE FUNCTION fixture_hold_ready() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.state=3 THEN PERFORM pg_advisory_xact_lock(hashtextextended(current_schema() || '/fixture-ready',0)); END IF; RETURN NEW; END $$; CREATE TRIGGER fixture_ready BEFORE UPDATE ON lake_snapshots FOR EACH ROW EXECUTE FUNCTION fixture_hold_ready()`); err != nil {
		t.Fatal("READY witness trigger unavailable")
	}
	blocker, err := a.pool.Acquire(ctx)
	if err != nil {
		t.Fatal("READY blocker unavailable")
	}
	held := true
	t.Cleanup(func() {
		if held {
			clean, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			if blocker.Hijack().Close(clean) != nil {
				t.Error("READY blocker cleanup failed")
			}
		}
	})
	var blockerPID int
	if blocker.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID) != nil {
		t.Fatal("blocker identity unavailable")
	}
	if _, err = blocker.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended(current_schema() || '/fixture-ready',0))`); err != nil {
		t.Fatal("READY blocker failed")
	}
	first := startSnapshotOwner(t, ctx, in)
	first.awaitReady(t, ctx)
	conn, err := grpc.NewClient(address, transport.ClientDialOption(b.gateway, transport.AuthorizeServices(spiffeid.RequireFromString("spiffe://kanz.internal/ns/kanz-services/sa/lake-sink"))))
	if err != nil {
		t.Fatal("gateway channel unavailable")
	}
	defer conn.Close()
	client := lakepb.NewLakeSnapshotServiceClient(conn)
	input := snapshotInput("request-process-crash")
	input.Selection.EventTimeFrom = timestamppb.New(row.EventTime)
	input.Selection.EventTimeUntil = timestamppb.New(row.EventTime.Add(time.Second))
	created, err := client.CreateSnapshot(ctx, &lakepb.CreateSnapshotRequest{Caller: creator("tenant-a", "operator"), Input: input})
	if err != nil || created.GetSnapshot().GetState() != lakepb.SnapshotState_SNAPSHOT_STATE_REQUESTED {
		t.Fatal("authenticated durable request failed")
	}
	id := created.Snapshot.SnapshotId
	wait := func(check func() bool) {
		t.Helper()
		for !check() {
			select {
			case <-ctx.Done():
				t.Fatal("crash proof observation expired")
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
	var ownerPID int
	wait(func() bool {
		return a.pool.QueryRow(ctx, `SELECT pid FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)) AND application_name=$2 AND wait_event_type='Lock' AND wait_event='advisory'`, blockerPID, application).Scan(&ownerPID) == nil
	})
	var beforeCut []byte
	var state, generation int
	if a.pool.QueryRow(ctx, `SELECT cut,state,generation FROM lake_snapshots WHERE snapshot_id=$1`, id).Scan(&beforeCut, &state, &generation) != nil || len(beforeCut) == 0 || state != 2 || generation != 1 {
		t.Fatal("crash witness lacks committed original cut")
	}
	// The trigger runs only after the real worker returned and publication fsync
	// completed. Read both immutable files independently before killing owner.
	manifestWire, err := readArtifactFile(ctx, outputRoot, id+"-1", "manifest.json", MaxControlBytes)
	if err != nil {
		t.Fatal("published orphan manifest absent before crash")
	}
	var manifest Manifest
	if json.Unmarshal(manifestWire, &manifest) != nil || manifest.Rows != 2 {
		t.Fatal("orphan manifest differs")
	}
	parquet, err := readArtifactFile(ctx, outputRoot, id+"-1", "events.parquet", MaxParquetBytes)
	if err != nil || uint64(len(parquet)) != manifest.ParquetBytes || sha256.Sum256(parquet) != manifest.ParquetSHA256 || sha256.Sum256(beforeCut) != manifest.CutSHA256 {
		t.Fatal("published orphan data absent before crash")
	}
	first.kill(t, ctx)
	var unlocked bool
	if blocker.QueryRow(ctx, `SELECT pg_advisory_unlock(hashtextextended(current_schema() || '/fixture-ready',0))`).Scan(&unlocked) != nil || !unlocked {
		t.Fatal("READY blocker release failed")
	}
	blocker.Release()
	held = false
	wait(func() bool {
		var n int
		return a.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE pid=$1 OR application_name=$2`, ownerPID, application).Scan(&n) == nil && n == 0
	})
	var ready, published int
	if a.pool.QueryRow(ctx, `SELECT count(*) FROM lake_snapshots WHERE state=3`).Scan(&ready) != nil || a.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE subject=$1`, "lake.snapshot.published").Scan(&published) != nil || ready != 0 || published != 0 {
		t.Fatal("killed READY transaction committed")
	}
	// Append to the exact known old file, then start a new generated landing run.
	// Either recapturing old suffix or selecting the new run would change two
	// rows to three/one and is rejected by the final canonical query assertion.
	var cut sink.LandingCut
	if json.Unmarshal(beforeCut, &cut) != nil || len(cut.Files) != 1 {
		t.Fatal("unexpected fixture cut inventory")
	}
	part := filepath.FromSlash(cut.Files[0].Path)
	if !filepath.IsLocal(part) {
		t.Fatal("invalid fixture cut path")
	}
	file, err := os.OpenFile(filepath.Join(landingRoot, part), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal("post-cut fixture append unavailable")
	}
	_, writeErr := file.Write(landingBytes(t, row))
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		t.Fatal("post-cut append failed")
	}
	in.Rows = []sink.Row{row}
	second := startSnapshotOwner(t, ctx, in)
	second.awaitReady(t, ctx)
	// Restart uses native retained redelivery. No consumer deletion, manual ACK,
	// AckWait change or synthetic republish accelerates this proof.
	reader := &lakepb.GatewayCaller{Subject: "operator", Tenant: "tenant-a", Roles: []string{"reader"}}
	var current *lakepb.Snapshot
	wait(func() bool {
		r, e := client.GetSnapshot(ctx, &lakepb.GetSnapshotRequest{Caller: reader, SnapshotId: id})
		if e != nil {
			return false
		}
		current = r.Snapshot
		return current.State == lakepb.SnapshotState_SNAPSHOT_STATE_READY
	})
	var afterCut []byte
	if a.pool.QueryRow(ctx, `SELECT cut,generation FROM lake_snapshots WHERE snapshot_id=$1`, id).Scan(&afterCut, &generation) != nil || !bytes.Equal(beforeCut, afterCut) || generation != 2 || current.RowCount != 2 {
		t.Fatal("restart replaced accepted source cut")
	}
	result, err := client.QuerySnapshot(ctx, &lakepb.QuerySnapshotRequest{Caller: reader, SnapshotId: id, Query: &lakepb.SnapshotQuery{Operation: lakepb.SnapshotQueryOperation_SNAPSHOT_QUERY_OPERATION_LIST_EVENTS, PageSize: 10}})
	if err != nil || len(result.GetResult().GetEvents()) != 2 || !result.Result.Exhausted {
		t.Fatal("restart query included new run or appended suffix")
	}
	for i, event := range result.Result.Events {
		if event.Ordinal != uint64(i) || event.EventId != row.EventID {
			t.Fatal("restart changed duplicate order")
		}
	}
	wait(func() bool {
		var n int
		return a.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE subject=$1 AND published_at IS NOT NULL`, "lake.snapshot.published").Scan(&n) == nil && n == 1
	})
	wait(func() bool {
		consumer, e := js.Consumer(ctx, snapshotprofile.Name, snapshotprofile.Durable)
		if e != nil {
			return false
		}
		info, e := consumer.Info(ctx)
		return e == nil && info.Delivered.Consumer >= 2 && info.NumAckPending == 0
	})
	second.close(t)
	wait(func() bool {
		var n int
		return a.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE application_name=$1`, application).Scan(&n) == nil && n == 0
	})
}
