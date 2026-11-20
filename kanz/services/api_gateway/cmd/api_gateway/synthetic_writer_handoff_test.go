package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"testing"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/oms/tools/syntheticvenue"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
)

const crashDurable = "synthetic-writer-handoff"

func signedFrameDigest(t *testing.T, frame *envelopepb.EventFrame) string {
	t.Helper()
	raw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func requestFromMessage(t *testing.T, msg jetstream.Msg, expected string) nativeSubmit {
	t.Helper()
	var frame envelopepb.EventFrame
	if err := proto.Unmarshal(msg.Data(), &frame); err != nil {
		t.Fatal(err)
	}
	if err := bus.Validate(frame.GetEnvelope()); err != nil || signedFrameDigest(t, &frame) != expected {
		t.Fatal("broker did not deliver the authenticated gateway frame")
	}
	var order orderpb.SubmitOrder
	if err := proto.Unmarshal(frame.Payload, &order); err != nil {
		t.Fatal(err)
	}
	return nativeSubmit{Version: 1, Op: "submit", Tenant: frame.GetEnvelope().GetTenantId(), Portfolio: order.GetPortfolioId(), Principal: order.GetMetadata().GetIssuer(), Key: frame.GetEnvelope().GetIdempotencyKey(), Command: frame.Payload}
}

// This process owns the first broker delivery. The parent kills it while it
// holds an unacknowledged message after the native financial transaction commits.
func TestSyntheticCommandCrashWorker(t *testing.T) {
	if os.Getenv("KANZ_SYNTHETIC_CRASH_CHILD") != "1" {
		return
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	broker := os.Getenv("KANZ_SYNTHETIC_CRASH_BROKER")
	u, err := url.Parse(broker)
	if err != nil || u.Scheme != "nats" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" || !net.ParseIP(u.Hostname()).IsLoopback() || !isolatedOrderDSN(os.Getenv("SYNTHETIC_OMS_RUNTIME_URL"), "kanz_synthetic_oms_runtime") {
		t.Fatal("crash worker requires the disposable local broker and database")
	}
	nc, err := nats.Connect(broker, nats.NoReconnect())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := js.Consumer(ctx, "SYNTHETIC_ORDERS", crashDurable)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := consumer.Next(jetstream.FetchMaxWait(4 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	req := requestFromMessage(t, msg, os.Getenv("KANZ_SYNTHETIC_FRAME_DIGEST"))
	committed := requireNativeReceipt(t, ctx, req, "ACCEPTED")
	if err = json.NewEncoder(os.Stdout).Encode(committed); err != nil {
		t.Fatal(err)
	}
	// A normal release of this barrier would ACK. The drill instead kills
	// this actual process and independently inspects JetStream's consumer floor.
	var permission [1]byte
	if _, err = io.ReadFull(os.Stdin, permission[:]); err != nil {
		t.Fatal("consumer interrupted before ACK")
	}
	if err = msg.DoubleAck(ctx); err != nil {
		t.Fatal(err)
	}
}

func waitForLock(t *testing.T, ctx context.Context, pool *pgxpool.Pool, query string, args ...any) {
	t.Helper()
	until := time.Now().Add(3 * time.Second)
	for {
		var blocked bool
		if err := pool.QueryRow(ctx, query, args...).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			return
		}
		if time.Now().After(until) {
			t.Fatal("expected PostgreSQL lock dependency was never observed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func useWriterGeneration(t *testing.T, generation string) {
	t.Helper()
	for _, kind := range []string{"RUNTIME", "ADAPTER"} {
		t.Setenv("SYNTHETIC_OMS_"+kind+"_URL", os.Getenv("SYNTHETIC_OMS_"+generation+"_"+kind+"_URL"))
	}
}

func exerciseFencedRestart(t *testing.T, ctx context.Context, s syntheticSession, request nativeSubmit, runtimePool *pgxpool.Pool) syntheticvenue.Receipt {
	t.Helper()
	stream, err := s.js.Stream(ctx, "SYNTHETIC_ORDERS")
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{Durable: crashDurable, FilterSubject: bus.TenantRoutedSubject(request.Tenant, "order.order.submit"), AckPolicy: jetstream.AckExplicitPolicy, AckWait: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	oldURL := os.Getenv("SYNTHETIC_OMS_RUNTIME_URL")
	child := exec.CommandContext(ctx, exe, "-test.run=^TestSyntheticCommandCrashWorker$")
	child.Env = []string{"KANZ_SYNTHETIC_CRASH_CHILD=1", "KANZ_SYNTHETIC_FRAME_DIGEST=" + signedFrameDigest(t, s.frame), "KANZ_SYNTHETIC_CRASH_BROKER=" + s.broker, "KANZ_SYNTHETIC_OMS_BIN=" + os.Getenv("KANZ_SYNTHETIC_OMS_BIN"), "SYNTHETIC_OMS_RUNTIME_URL=" + oldURL}
	barrier, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer barrier.Close()
	pipe, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = child.Start(); err != nil {
		t.Fatal(err)
	}
	joined := false
	defer func() {
		if !joined {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
	}()
	raw, err := bufio.NewReader(io.LimitReader(pipe, 131073)).ReadBytes('\n')
	if err != nil {
		t.Fatal("first consumer exited before its commit checkpoint")
	}
	var accepted syntheticvenue.Receipt
	if json.Unmarshal(raw, &accepted) != nil || !accepted.OK || accepted.Code != "ACCEPTED" {
		t.Fatal("invalid committed receipt")
	}
	info, err := consumer.Info(ctx)
	if err != nil || info.NumAckPending != 1 || info.AckFloor.Stream != 0 {
		t.Fatalf("unexpected pre-crash ACK state: %v", err)
	}
	var state []byte
	var version, epoch int64
	if err = runtimePool.QueryRow(ctx, "SELECT o.state,o.version,c.writer_epoch FROM orders o JOIN synthetic_oms_commands c USING(tenant_id,order_id) WHERE c.idempotency_key=$1", request.Key).Scan(&state, &version, &epoch); err != nil || epoch != 1 || !bytes.Equal(state, accepted.State) || uint64(version) != accepted.OrderVersion {
		t.Fatalf("independent commit read failed: %v", err)
	}

	old, err := runtimePool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Rollback(ctx)
	var oldPID, switchPID int
	if err = old.QueryRow(ctx, "SELECT pg_backend_pid(),synthetic_oms_assert_writer()").Scan(&oldPID, &epoch); err != nil || epoch != 1 {
		t.Fatalf("first writer failed: %v", err)
	}
	control := syntheticPool(t, ctx, "SWITCH", request.Tenant)
	change, err := control.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer change.Rollback(ctx)
	if err = change.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&switchPID); err != nil {
		t.Fatal(err)
	}
	advanced := make(chan error, 1)
	go func() {
		var next int64
		e := change.QueryRow(ctx, "SELECT synthetic_test_handoff()").Scan(&next)
		if e == nil && next != 2 {
			e = errors.New("wrong successor generation")
		}
		advanced <- e
	}()
	waitForLock(t, ctx, control, "SELECT $1::integer=ANY(pg_blocking_pids($2::integer))", oldPID, switchPID)
	if err = old.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-advanced; err != nil {
		t.Fatal(err)
	}
	stale := make(chan syntheticvenue.Receipt, 1)
	go func() { r, _ := callSyntheticOwner(ctx, oldURL, request); stale <- r }()
	waitForLock(t, ctx, control, "SELECT EXISTS(SELECT FROM pg_stat_activity WHERE usename='kanz_synthetic_oms_runtime' AND $1::integer=ANY(pg_blocking_pids(pid)))", switchPID)
	if err = change.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if r := <-stale; r.OK || r.Code != "WRITER_FENCED" {
		t.Fatalf("stale surviving native process returned %q", r.Code)
	}

	if err = child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err = child.Wait(); err == nil {
		t.Fatal("first consumer exited successfully despite injected crash")
	}
	joined = true
	// The old SQL connection has its original login after failover. The
	// database trigger also rejects writes that did not pass through Rust.
	staleTx, err := runtimePool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = staleTx.Exec(ctx, "INSERT INTO synthetic_oms_commands SELECT * FROM synthetic_oms_commands WHERE idempotency_key=$1", request.Key)
	_ = staleTx.Rollback(ctx)
	var pgerr *pgconn.PgError
	if !errors.As(err, &pgerr) || pgerr.Code != "P1533" {
		t.Fatalf("direct stale write lacked database fence: %v", err)
	}
	if _, err = control.Exec(ctx, "SELECT synthetic_test_handoff()"); !errors.As(err, &pgerr) || pgerr.Code != "P1533" {
		t.Fatal("stale handoff CAS was accepted")
	}

	useWriterGeneration(t, "NEXT")
	replay, err := consumer.Next(jetstream.FetchMaxWait(4 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := replay.Metadata()
	if err != nil || metadata.NumDelivered != 2 || metadata.Sequence.Stream != info.Delivered.Stream {
		t.Fatalf("not the original redelivered command: %v", err)
	}
	recovered := requestFromMessage(t, replay, signedFrameDigest(t, s.frame))
	if !bytes.Equal(recovered.Command, request.Command) || recovered.Key != request.Key || recovered.Tenant != request.Tenant {
		t.Fatal("immutable command changed across redelivery")
	}
	r := requireNativeReceipt(t, ctx, recovered, "DUPLICATE")
	if r.OrderVersion != accepted.OrderVersion || !bytes.Equal(r.State, accepted.State) {
		t.Fatal("restart refolded the committed order")
	}
	if err = replay.DoubleAck(ctx); err != nil {
		t.Fatal(err)
	}
	info, err = consumer.Info(ctx)
	if err != nil || info.NumAckPending != 0 || info.AckFloor.Stream != metadata.Sequence.Stream {
		t.Fatal("redelivery ACK was not confirmed")
	}
	var commands, orders, facts, generations int
	if err = runtimePool.QueryRow(ctx, "SELECT (SELECT count(order_id) FROM synthetic_oms_commands),(SELECT count(order_id) FROM orders),(SELECT count(producer_service) FROM outbox)").Scan(&commands, &orders, &facts); err != nil || commands != 1 || orders != 1 || facts != 1 {
		t.Fatalf("post-crash state count %d/%d/%d: %v", commands, orders, facts, err)
	}
	if err = control.QueryRow(ctx, "SELECT count(*) FROM synthetic_oms_writer_history").Scan(&generations); err != nil || generations != 2 {
		t.Fatalf("handoff history: %d, %v", generations, err)
	}
	t.Log("real JetStream crash/restart, durable epoch 1 -> 2, surviving native process fenced, immutable replay acknowledged once")
	return accepted
}
