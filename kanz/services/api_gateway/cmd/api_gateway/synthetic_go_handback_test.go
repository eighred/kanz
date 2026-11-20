package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/testidentity"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/oms/tools/syntheticvenue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

func TestSyntheticReverseTransferToGo(t *testing.T) {
	if os.Getenv("KANZ_SYNTHETIC_GO_HANDBACK") != "1" {
		t.Skip("requires the isolated Rust-to-Go handback fixture")
	}
	const tenant = "synthetic-go-handback"
	exerciseSyntheticSession(t, tenant, func(ctx context.Context, s syntheticSession) {
		cmd, request := prepareSyntheticOrder(t, ctx, s, tenant)
		requireNativeReceipt(t, ctx, request, "ACCEPTED")
		exerciseSyntheticGoHandback(t, ctx, s, cmd, request, 1)
	})
}

// Begin with an unpublished acceptance and a native writer that owns the
// retained authenticated command. The order may have been created by either
// writer, but the two venue outcomes and the Go startup path are identical.
func exerciseSyntheticGoHandback(t *testing.T, ctx context.Context, s syntheticSession, cmd *orderpb.SubmitOrder, request nativeSubmit, rustEpoch int) {
	t.Helper()
	tenant := request.Tenant
	handoff, goDSN := "SELECT synthetic_test_handoff()", os.Getenv("SYNTHETIC_OMS_NEXT_RUNTIME_URL")
	switch rustEpoch {
	case 1:
	case 2:
		handoff, goDSN = "SELECT synthetic_test_recovery_handoff()", os.Getenv("SYNTHETIC_OMS_RECOVERY_RUNTIME_URL")
	default:
		t.Fatalf("unsupported synthetic native epoch %d", rustEpoch)
	}
	operator := syntheticPool(t, ctx, "SWITCH", tenant)
	refuse := func(txOpts pgx.TxOptions, reason string) {
		t.Helper()
		tx, err := operator.BeginTx(ctx, txOpts)
		if err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(ctx, handoff)
		var refused *pgconn.PgError
		if !errors.As(err, &refused) || refused.Code != "P1533" || refused.Message != reason {
			t.Fatalf("unexpected handoff refusal: %v", err)
		}
		if err = tx.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		var generations int
		if err = operator.QueryRow(ctx, "SELECT count(*) FROM synthetic_oms_writer_history").Scan(&generations); err != nil || generations != rustEpoch {
			t.Fatalf("refused handoff changed history: %d %v", generations, err)
		}
	}
	read := nativeSubmit{Version: 1, Op: "get", Tenant: tenant, Principal: request.Principal, Portfolio: request.Portfolio, OrderID: cmd.OrderId}
	var sends, queries atomic.Int32
	count := func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		switch info.FullMethod {
		case venuepb.VenueAdapterService_Execute_FullMethodName:
			sends.Add(1)
		case venuepb.VenueAdapterService_QueryOrder_FullMethodName:
			queries.Add(1)
		}
		return handler(ctx, req)
	}
	venue, addr, identity := startSyntheticVenue(t, ctx, syntheticvenue.Config{Tenant: tenant, Portfolio: request.Portfolio, FillTime: time.Now().UTC(), LoseExecuteAck: true}, grpc.UnaryInterceptor(count))
	observe := func(ctx context.Context, c syntheticvenue.Command) (syntheticvenue.Receipt, error) {
		return callSyntheticOwner(ctx, os.Getenv("SYNTHETIC_OMS_ADAPTER_URL"), struct {
			Version int `json:"version"`
			syntheticvenue.Command
		}{1, c})
	}
	const unsafe = "retained order is not ready for Go recovery"
	var dispatch syntheticvenue.Command
	ingress, err := syntheticvenue.NewIngress(tenant, request.Portfolio, venue, func(ctx context.Context, c syntheticvenue.Command) (syntheticvenue.Receipt, error) {
		r, e := observe(ctx, c)
		if e == nil && r.OK && c.Op == "dispatch" && r.Code == "DISPATCH_ONCE" {
			dispatch = c
			if sends.Load() != 0 {
				t.Fatal("dispatch claim did not precede venue send")
			}
			refuse(pgx.TxOptions{}, unsafe)
		}
		return r, e
	})
	if err != nil {
		t.Fatal(err)
	}
	stream, relay := syntheticFactRelay(t, ctx, s, tenant)
	if n, e := relay.Flush(ctx, cmd.OrderId); e != nil || n != 1 {
		t.Fatalf("accepted relay: %d %v", n, e)
	}
	consumer, err := stream.CreateConsumer(ctx, jetstream.ConsumerConfig{Durable: "go-handback", FilterSubject: syntheticvenue.AcceptedSubject, AckPolicy: jetstream.AckExplicitPolicy})
	if err != nil {
		t.Fatal(err)
	}
	for delivery := uint64(1); delivery <= 2; delivery++ {
		msg, e := consumer.Next(jetstream.FetchMaxWait(4 * time.Second))
		if e != nil {
			t.Fatal(e)
		}
		meta, e := msg.Metadata()
		if e != nil || meta.NumDelivered != delivery {
			t.Fatalf("delivery count: %v %v", meta, e)
		}
		var frame envelopepb.EventFrame
		if proto.Unmarshal(msg.Data(), &frame) != nil || bus.Validate(frame.Envelope) != nil {
			t.Fatal("invalid committed acceptance")
		}
		source := s.frame.GetEnvelope()
		var accepted orderpb.OrderAccepted
		if frame.Envelope.GetCausationId() != source.GetEventId() || frame.Envelope.GetCorrelationId() != source.GetEventId() ||
			frame.Envelope.GetTraceContext() != source.GetTraceContext() || proto.Unmarshal(frame.Payload, &accepted) != nil ||
			accepted.GetOrderId() != cmd.GetOrderId() {
			t.Fatal("retained acceptance lost the signed command lineage")
		}
		e = ingress.HandleAccepted(ctx, frame.Envelope, frame.Payload)
		if delivery == 1 {
			var unknown orderpb.OrderState
			got := requireNativeReceipt(t, ctx, read, "FOUND")
			if e == nil || dispatch.OrderID != cmd.OrderId || sends.Load() != 1 || queries.Load() != 0 || proto.Unmarshal(got.State, &unknown) != nil || unknown.Quarantine == nil || unknown.VenueAckAt != nil {
				t.Fatal("lost venue ACK must remain uncertain after one send")
			}
			refuse(pgx.TxOptions{}, unsafe)
			if err = msg.Nak(); err != nil {
				t.Fatal(err)
			}
		} else if e != nil {
			t.Fatal(e)
		} else if err = msg.DoubleAck(ctx); err != nil {
			t.Fatal(err)
		}
	}
	working := requireNativeReceipt(t, ctx, read, "FOUND")
	var st orderpb.OrderState
	if proto.Unmarshal(working.State, &st) != nil || st.Status != orderpb.OrderStatus_ORDER_STATUS_ROUTED || st.Quarantine != nil || st.VenueAckAt == nil || sends.Load() != 1 || queries.Load() != 1 {
		t.Fatal("typed XSIM recovery did not establish a working order with one send")
	}
	refuse(pgx.TxOptions{IsoLevel: pgx.RepeatableRead}, "Go handback requires read committed")
	var generation int
	if err = operator.QueryRow(ctx, handoff).Scan(&generation); err != nil || generation != rustEpoch+1 {
		t.Fatalf("working handoff: %d %v", generation, err)
	}
	requireNativeReceipt(t, ctx, request, "WRITER_FENCED")
	if r, e := observe(ctx, dispatch); e != nil || r.OK || r.Code != "WRITER_FENCED" {
		t.Fatalf("retired adapter: %s %v", r.Code, e)
	}
	startSyntheticGoSuccessor(t, ctx, s, tenant, addr, identity, working, generation, goDSN, "", nil)
	if sends.Load() != 1 || queries.Load() != 2 {
		t.Fatalf("expected one original Execute and one Query per writer, got %d/%d", sends.Load(), queries.Load())
	}
	remote := syntheticPool(t, ctx, "VENUE", tenant)
	var rows, executions int
	if err = remote.QueryRow(ctx, "SELECT count(*),count(execution) FROM synthetic_venue.receipts WHERE order_id=$1", cmd.OrderId).Scan(&rows, &executions); err != nil || rows != 1 || executions != 0 {
		t.Fatalf("remote receipts/fills: %d/%d %v", rows, executions, err)
	}
	t.Logf("GO_HANDBACK Raw claim and UNKNOWN refused; one XSIM Execute, two typed venue queries, zero executions; acknowledged working order transferred to Go generation %d and both retired Rust logins fenced.", generation)
}

func startSyntheticGoSuccessor(t *testing.T, ctx context.Context, s syntheticSession, tenant, addr string, identity *testidentity.Source,
	working syntheticvenue.Receipt, generation int, goDSN, cancelEventID string, withdrawal *fundedWithdrawalApproval,
) {
	t.Helper()
	_, socket := testidentity.StartWorkload(t, identity)
	args := map[string]any{"tenant": tenant, "endpoint": addr, "socket": socket, "broker": s.broker,
		"state": working.State, "version": working.OrderVersion, "generation": generation, "cancelEventID": cancelEventID}
	if withdrawal != nil {
		args["withdrawalEventID"] = withdrawal.event.EventId
	}
	run := func(stage string, wantExit int) {
		t.Helper()
		args["withdrawalStage"] = stage
		input, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		child := exec.CommandContext(ctx, "/fixture/oms", "-test.run=^TestSyntheticRecoveredGoStartup$", "-test.count=1", "-test.v")
		child.Env = []string{"KANZ_SYNTHETIC_GO_HANDBACK=1", "SYNTHETIC_OMS_NEXT_RUNTIME_URL=" + goDSN}
		child.Stdin = bytes.NewReader(input)
		output, err := child.CombinedOutput()
		if wantExit != 0 {
			var exited *exec.ExitError
			if !errors.As(err, &exited) || exited.ExitCode() != wantExit {
				t.Fatalf("Go withdrawal %s expected process exit %d: %v: %s", stage, wantExit, err, output)
			}
			t.Logf("GO_HANDBACK withdrawal %s process exited %d", stage, wantExit)
		} else if err != nil || !strings.Contains(string(output), "--- PASS: TestSyntheticRecoveredGoStartup") {
			t.Fatalf("Go startup sweep failed: %v: %s", err, output)
		}
		for _, line := range strings.Split(string(output), "\n") {
			if _, message, ok := strings.Cut(line, "GO_HANDBACK "); ok {
				t.Log("GO_HANDBACK " + message)
			}
		}
	}
	if withdrawal == nil || !withdrawal.restart {
		run("", 0)
		return
	}
	run("send", 71)
	if err := withdrawal.checkClaim(ctx); err != nil {
		t.Fatal(err)
	}
	withdrawal.verifyRemoteAfterCrash(t, ctx)
	withdrawal.verifyBrokerBeforePublish(t, ctx, s, 1)
	run("reconcile", 72)
	withdrawal.verifyTerminal(t, ctx, []string{"order.order.cancelled"})
	withdrawal.verifyBrokerBeforePublish(t, ctx, s, 2)
	run("publish", 0)
}
