//go:build linux && amd64

package snapshot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/lake/snapshotprofile"
	lakepb "github.com/eighred/kanz/kanz-schemas-go/lake/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/eighred/kanz/services/lake-sink/internal/config"
	"github.com/eighred/kanz/services/lake-sink/internal/sink"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// This proves the actual lake runtime/gateway-peer edge, not public gateway JWT
// authentication. The separate gateway composition must establish end-user auth.
func TestSnapshotActualRuntimeDurablePublicationQueryAndEstablishedChannelFailure(t *testing.T) {
	path, digest := actualWorker(t)
	a, _ := snapshotStores(t)
	b := newSnapshotBroker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	admin, err := b.connect(b.adminA)
	if err != nil {
		t.Fatal("fixture admin unavailable")
	}
	defer admin.Close()
	js, err := jetstream.New(admin)
	if err != nil {
		t.Fatal("fixture JetStream unavailable")
	}
	profile := snapshotprofile.Stream()
	if _, err := js.CreateStream(ctx, profile); err != nil {
		t.Fatal("stream provisioning failed")
	}
	provisionSnapshotRegisteredDependencies(t, ctx, js)
	landingRoot, outputRoot := t.TempDir(), t.TempDir()
	landing, err := sink.NewFileSink(landingRoot, "", sink.EnableActiveRunCapture)
	if err != nil {
		t.Fatal("active landing unavailable")
	}
	t.Cleanup(func() {
		if landing.Close() != nil {
			t.Error("landing close failed")
		}
	})
	row := custodyRow(t, "tenant-a")
	foreign := custodyRow(t, "tenant-b")
	for _, r := range []sink.Row{row, foreign, row} {
		if landing.Write(ctx, r) != nil {
			t.Fatal("raw row landing failed")
		}
	}
	if landing.Flush() != nil {
		t.Fatal("raw custody flush failed")
	}
	policyPath := filepath.Join(t.TempDir(), "policy.json")
	if os.WriteFile(policyPath, []byte(`{"roles":{"creator":["lake.snapshot.create"],"reader":["lake.snapshot.read"]}}`), 0600) != nil {
		t.Fatal("fixture policy unavailable")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("fixture address unavailable")
	}
	address := listener.Addr().String()
	_ = listener.Close()
	cfg := config.Snapshot{Tenant: "tenant-a", Listen: address, OutputDir: outputRoot, DatabaseURL: a.pool.Config().ConnConfig.ConnString(), NATSURL: "nats://" + b.address, NATSAccount: "acme", PolicyFile: policyPath, WorkerPath: path, WorkerSHA256: digest, GatewayID: spiffeid.RequireFromString("spiffe://kanz.internal/ns/kanz-services/sa/api-gateway")}
	// Seed a durable request, then hold its real lease key. Initial catch-up
	// cannot complete until release; the HTTP readiness flag alone is not an
	// RPC admission fence.
	initialInput := snapshotInput("request-before-startup")
	initialInput.Selection.EventTimeFrom = timestamppb.New(row.EventTime)
	initialInput.Selection.EventTimeUntil = timestamppb.New(row.EventTime.Add(time.Second))
	initial, err := a.Create(ctx, creator("tenant-a", "operator"), initialInput)
	if err != nil {
		t.Fatal("initial durable request unavailable")
	}
	leaseBlocker, err := a.pool.Acquire(ctx)
	if err != nil {
		t.Fatal("initial replay lock unavailable")
	}
	lockHeld := true
	t.Cleanup(func() {
		if lockHeld {
			cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			_ = leaseBlocker.Hijack().Close(cleanup)
		}
	})
	const leaseKey = `hashtextextended(current_database() || '/' || ('lake_snapshots'::regclass)::oid::text || '/' || app_current_tenant() || '/lake-snapshot/' || $1,0)`
	if _, err = leaseBlocker.Exec(ctx, `SELECT pg_advisory_lock(`+leaseKey+`)`, initial.SnapshotId); err != nil {
		t.Fatal("initial replay lock failed")
	}
	runCtx, stopOwner := context.WithCancel(ctx)
	var ready atomic.Bool
	done := make(chan error, 1)
	go func() {
		done <- Run(runCtx, cfg, b.source, landing, landingRoot, slog.New(slog.NewTextHandler(io.Discard, nil)), bus.NewBusMetrics(prometheus.NewRegistry()), ready.Store)
	}()
	joined := false
	t.Cleanup(func() {
		stopOwner()
		if !joined {
			select {
			case err := <-done:
				if err != nil {
					t.Error("cancelled snapshot owner join failed")
				}
			case <-time.After(35 * time.Second):
				t.Error("actual snapshot owner did not join")
			}
		}
	})
	waitCondition := func(label string, check func() bool) {
		t.Helper()
		for !check() {
			select {
			case <-ctx.Done():
				t.Fatal(label)
			case err := <-done:
				joined = true
				_ = err
				t.Fatal("snapshot owner stopped before proof")
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
	waitCondition("initial replay was not observed", func() bool {
		consumer, e := js.Consumer(ctx, snapshotprofile.Name, snapshotprofile.Durable)
		if e != nil {
			return false
		}
		info, e := consumer.Info(ctx)
		return e == nil && info.NumAckPending == 1
	})
	conn, err := grpc.NewClient(address, transport.ClientDialOption(b.gateway, transport.AuthorizeServices(spiffeid.RequireFromString("spiffe://kanz.internal/ns/kanz-services/sa/lake-sink"))))
	if err != nil {
		t.Fatal("gateway channel unavailable")
	}
	defer conn.Close()
	client := lakepb.NewLakeSnapshotServiceClient(conn)
	reader := &lakepb.GatewayCaller{Subject: "operator", Tenant: "tenant-a", Roles: []string{"reader"}}
	for _, operation := range []string{"Create", "Get", "Query"} {
		attempt, stop := context.WithTimeout(ctx, 300*time.Millisecond)
		var callErr error
		switch operation {
		case "Create":
			_, callErr = client.CreateSnapshot(attempt, &lakepb.CreateSnapshotRequest{Caller: creator("tenant-a", "operator"), Input: snapshotInput("request-before-barrier")})
		case "Get":
			_, callErr = client.GetSnapshot(attempt, &lakepb.GetSnapshotRequest{Caller: reader, SnapshotId: initial.SnapshotId})
		case "Query":
			_, callErr = client.QuerySnapshot(attempt, &lakepb.QuerySnapshotRequest{Caller: reader, SnapshotId: initial.SnapshotId, Query: &lakepb.SnapshotQuery{Operation: lakepb.SnapshotQueryOperation_SNAPSHOT_QUERY_OPERATION_LIST_EVENTS, PageSize: 10}})
		}
		stop()
		// Serve is withheld: this proves refusal, not completed TLS authentication.
		code := status.Code(callErr)
		if (code != codes.DeadlineExceeded && code != codes.Unavailable) || ready.Load() {
			t.Fatalf("pre-barrier %s admitted or incorrectly ready: %s", operation, status.Code(callErr))
		}
	}
	var beforeRequests, beforeFacts int
	if a.pool.QueryRow(ctx, `SELECT count(*) FROM lake_snapshots`).Scan(&beforeRequests) != nil || a.pool.QueryRow(ctx, `SELECT count(*) FROM outbox`).Scan(&beforeFacts) != nil || beforeRequests != 1 || beforeFacts != 1 {
		t.Fatal("pre-barrier RPC changed durable state")
	}
	var unlocked bool
	if leaseBlocker.QueryRow(ctx, `SELECT pg_advisory_unlock(`+leaseKey+`)`, initial.SnapshotId).Scan(&unlocked) != nil || !unlocked {
		t.Fatal("initial replay release failed")
	}
	leaseBlocker.Release()
	lockHeld = false
	waitCondition("runtime never became ready", ready.Load)
	positive, err := client.GetSnapshot(ctx, &lakepb.GetSnapshotRequest{Caller: reader, SnapshotId: initial.SnapshotId})
	if err != nil || positive.GetSnapshot().GetState() != lakepb.SnapshotState_SNAPSHOT_STATE_READY {
		t.Fatal("same valid gateway client did not authenticate after initial replay")
	}
	runActualGatewayAndCLI(t, ctx, b, address, row)
	input := snapshotInput("request-actual-runtime")
	input.Selection.EventTimeFrom = timestamppb.New(row.EventTime)
	input.Selection.EventTimeUntil = timestamppb.New(row.EventTime.Add(time.Second))
	created, err := client.CreateSnapshot(ctx, &lakepb.CreateSnapshotRequest{Caller: creator("tenant-a", "operator"), Input: input})
	if err != nil || created.GetSnapshot().GetState() != lakepb.SnapshotState_SNAPSHOT_STATE_REQUESTED {
		t.Fatal("actual durable request failed")
	}
	var accepted *lakepb.Snapshot
	waitCondition("snapshot did not publish", func() bool {
		r, e := client.GetSnapshot(ctx, &lakepb.GetSnapshotRequest{Caller: reader, SnapshotId: created.Snapshot.SnapshotId})
		if e != nil {
			return false
		}
		accepted = r.Snapshot
		return accepted.State == lakepb.SnapshotState_SNAPSHOT_STATE_READY
	})
	if accepted.RowCount != 2 || len(accepted.ManifestSha256) != 32 {
		t.Fatal("foreign row or custody manifest mismatch")
	}
	if landing.Write(ctx, row) != nil || landing.Flush() != nil {
		t.Fatal("post-cut append failed")
	}
	result, err := client.QuerySnapshot(ctx, &lakepb.QuerySnapshotRequest{Caller: reader, SnapshotId: accepted.SnapshotId, Query: &lakepb.SnapshotQuery{Operation: lakepb.SnapshotQueryOperation_SNAPSHOT_QUERY_OPERATION_LIST_EVENTS, PageSize: 10}})
	if err != nil || len(result.GetResult().GetEvents()) != 2 || !result.Result.Exhausted {
		t.Fatal("registered cut changed after source append")
	}
	for i, event := range result.Result.Events {
		if event.Ordinal != uint64(i) || event.EventId != row.EventID || !proto.Equal(event.EventTime, timestamppb.New(row.EventTime)) {
			t.Fatal("selected tenant duplicate order/time changed")
		}
	}
	foreignReader := proto.Clone(reader).(*lakepb.GatewayCaller)
	foreignReader.Tenant = "tenant-b"
	if _, err := client.GetSnapshot(ctx, &lakepb.GetSnapshotRequest{Caller: foreignReader, SnapshotId: accepted.SnapshotId}); status.Code(err) != codes.NotFound {
		t.Fatal("foreign resource existence exposed")
	}
	waitCondition("published FACT not marked", func() bool {
		var pending int
		return a.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE published_at IS NULL`).Scan(&pending) == nil && pending == 0
	})
	// Fail a real publication after an authenticated channel has been established.
	// Accepted SQL/outbox state survives; readiness and the existing channel do not.
	broken := profile
	broken.MaxMsgSize = 1
	if _, err := js.UpdateStream(ctx, broken); err != nil {
		t.Fatal("publication refusal fixture unavailable")
	}
	second := proto.Clone(input).(*lakepb.CreateSnapshotInput)
	second.RequestId = "request-refused-publication"
	response, err := client.CreateSnapshot(ctx, &lakepb.CreateSnapshotRequest{Caller: creator("tenant-a", "operator"), Input: second})
	if err == nil && response.GetSnapshot() == nil {
		t.Fatal("durable acceptance response missing")
	}
	if err != nil && status.Code(err) != codes.Canceled && status.Code(err) != codes.Unavailable {
		t.Fatal("unexpected acceptance refusal")
	}
	// The publisher may cancel the owner after commit but before the response
	// reaches this channel. Prove durable acceptance independently; do not infer
	// rollback or successful creation from a transport result.
	var acceptedRows int
	if a.pool.QueryRow(ctx, `SELECT count(*) FROM lake_snapshots WHERE request_id=$1`, second.RequestId).Scan(&acceptedRows) != nil || acceptedRows != 1 {
		t.Fatal("request was not durably accepted before publication refusal")
	}
	select {
	case err := <-done:
		joined = true
		if err == nil {
			t.Fatal("publication failure reported healthy shutdown")
		}
	case <-ctx.Done():
		t.Fatal("publication failure did not stop owner")
	}
	if ready.Load() {
		t.Fatal("failed owner remained ready")
	}
	if _, err := client.GetSnapshot(ctx, &lakepb.GetSnapshotRequest{Caller: reader, SnapshotId: accepted.SnapshotId}); err == nil {
		t.Fatal("established channel continued after failure")
	}
	var pending int
	if a.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE published_at IS NULL`).Scan(&pending) != nil || pending < 1 {
		t.Fatal("unconfirmed publication lost durable outbox")
	}
}

type snapshotProcessOutput struct{ bytes.Buffer }

func (b *snapshotProcessOutput) Write(p []byte) (int, error) {
	if len(p) > (64<<10)-b.Len() {
		return 0, errors.New("bounded child output exceeded")
	}
	return b.Buffer.Write(p)
}

func runActualGatewayAndCLI(t *testing.T, ctx context.Context, b *snapshotBroker, address string, row sink.Row) {
	t.Helper()
	child, cli := os.Getenv("TEST_LAKE_GATEWAY_TEST_BIN"), os.Getenv("TEST_LAKE_CLI_BIN")
	if !filepath.IsAbs(child) || !filepath.IsAbs(cli) {
		t.Fatal("configured runtime proof requires actual gateway/CLI executables")
	}
	leaf, err := b.gateway.GetX509SVID()
	if err != nil {
		t.Fatal("synthetic gateway identity unavailable")
	}
	cert, key, err := leaf.Marshal()
	if err != nil {
		t.Fatal("synthetic gateway leaf encoding failed")
	}
	bundle, err := b.gateway.GetX509BundleForTrustDomain(leaf.ID.TrustDomain())
	if err != nil {
		t.Fatal("synthetic gateway trust unavailable")
	}
	trust, err := bundle.Marshal()
	if err != nil {
		t.Fatal("synthetic gateway trust encoding failed")
	}
	input, err := json.Marshal(struct {
		Address, CLI, From, Until string
		Cert, Key, Bundle         []byte
	}{address, cli, row.EventTime.Format(time.RFC3339Nano), row.EventTime.Add(time.Second).Format(time.RFC3339Nano), cert, key, trust})
	if err != nil || len(input) > 64<<10 {
		t.Fatal("bounded gateway process input invalid")
	}
	cmd := exec.CommandContext(ctx, child, "-test.run=^TestLakeGatewaySnapshotProcess$", "-test.count=1")
	cmd.Env = []string{"KANZ_LAKE_GATEWAY_CHILD=1"}
	cmd.Stdin = bytes.NewReader(input)
	cmd.WaitDelay = 5 * time.Second
	var output snapshotProcessOutput
	cmd.Stdout = &output
	cmd.Stderr = io.Discard
	if cmd.Run() != nil || !bytes.Contains(output.Bytes(), []byte("PASS")) {
		t.Fatal("actual authenticated gateway/CLI process failed")
	}
	// Child output and ephemeral credentials are deliberately not emitted into
	// evidence. The parent case only passes after the real child joined cleanly.
}
