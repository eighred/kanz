package main

import (
	"bytes"
	"context"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/testidentity"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/oms/tools/syntheticvenue"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
)

func TestSyntheticGoRustGoRoundTrip(t *testing.T) {
	if os.Getenv("KANZ_SYNTHETIC_GO_ROUNDTRIP") != "1" {
		t.Skip("requires a disposable Go-to-Rust-to-Go fixture")
	}
	exerciseGoRustGo(t, "synthetic-go-roundtrip", noGoWithdrawal)
}

func TestSyntheticFundedGoRustGoRoundTrip(t *testing.T) {
	if os.Getenv("KANZ_SYNTHETIC_GO_FUNDED_ROUNDTRIP") != "1" {
		t.Skip("requires a disposable funded Go-to-Rust-to-Go fixture")
	}
	exerciseGoRustGo(t, "synthetic-funded-cancel-fill", noGoWithdrawal)
}

const fundedWorkingTenant = "synthetic-go-funded-working"

func TestSyntheticFundedWorkingGoRustGo(t *testing.T) {
	if os.Getenv("KANZ_SYNTHETIC_GO_FUNDED_WORKING") != "1" {
		t.Skip("requires a disposable funded working-order handback")
	}
	exerciseGoRustGo(t, fundedWorkingTenant, noGoWithdrawal)
}

func TestSyntheticFundedWithdrawalGoRustGo(t *testing.T) {
	if os.Getenv("KANZ_SYNTHETIC_GO_FUNDED_WITHDRAWAL") != "1" {
		t.Skip("requires the disposable funded Go withdrawal fixture")
	}
	exerciseGoRustGo(t, fundedWorkingTenant, goWithdrawal)
}

func TestSyntheticFundedWithdrawalRestartGoRustGo(t *testing.T) {
	if os.Getenv("KANZ_SYNTHETIC_GO_FUNDED_WITHDRAWAL_RESTART") != "1" {
		t.Skip("requires the disposable funded withdrawal restart fixture")
	}
	exerciseGoRustGo(t, fundedWorkingTenant, goWithdrawalRestart)
}

type goWithdrawalMode int

const (
	noGoWithdrawal goWithdrawalMode = iota
	goWithdrawal
	goWithdrawalRestart
)

func exerciseGoRustGo(t *testing.T, tenant string, withdrawalMode goWithdrawalMode) {
	funded := tenant != "synthetic-go-roundtrip"
	t.Helper()
	exerciseSyntheticSession(t, tenant, func(ctx context.Context, s syntheticSession) {
		admit, policy := admitSignedGoOrigin, jetstream.DeliverAllPolicy
		var flags []string
		if funded {
			prepareSyntheticAccounting(t, ctx, s, tenant)
			s.frame = submitSignedGoFundedOrder(t, s)
			admit, policy = admitSignedFundedGoOrigin, jetstream.DeliverLastPolicy
			flags = []string{"--funded"}
		}
		consumer, err := s.js.CreateConsumer(ctx, "SYNTHETIC_ORDERS", jetstream.ConsumerConfig{
			Durable: "roundtrip-origin", FilterSubject: bus.TenantRoutedSubject(tenant, "order.order.submit"),
			AckPolicy: jetstream.AckExplicitPolicy, DeliverPolicy: policy,
		})
		if err != nil {
			t.Fatal(err)
		}
		msg, err := consumer.Next(jetstream.FetchMaxWait(3 * time.Second))
		if err != nil {
			t.Fatal(err)
		}
		wire := msg.Data()
		source, request := admit(t, ctx, s, msg)
		var cmd orderpb.SubmitOrder
		if err = proto.Unmarshal(source.Payload, &cmd); err != nil {
			t.Fatal(err)
		}
		reader := syntheticPool(t, ctx, "RUNTIME", tenant)
		var state, retained []byte
		var admitted, version int64
		err = reader.QueryRow(ctx, `SELECT o.state,o.version,c.writer_epoch,c.go_gateway_frame
			FROM orders o JOIN synthetic_oms_commands c USING(tenant_id,order_id) WHERE o.order_id=$1`,
			cmd.OrderId).Scan(&state, &version, &admitted, &retained)
		if err != nil || version != 0 || admitted != 1 || !bytes.Equal(retained, wire) {
			t.Fatalf("same-transaction Go admission: version %d, epoch %d: %v", version, admitted, err)
		}

		// Interrupt the command before ACK. The next owner must recover the
		// persisted result from the same broker delivery rather than re-admit.
		if err = msg.Nak(); err != nil {
			t.Fatal(err)
		}
		operator := syntheticPool(t, ctx, "SWITCH", tenant)
		var epoch int64
		if err = operator.QueryRow(ctx, "SELECT synthetic_test_handoff()").Scan(&epoch); err != nil || epoch != 2 {
			t.Fatalf("Go-to-Rust handoff: epoch %d: %v", epoch, err)
		}
		retry, err := consumer.Next(jetstream.FetchMaxWait(3 * time.Second))
		if err != nil {
			t.Fatal(err)
		}
		meta, err := retry.Metadata()
		if err != nil || meta.NumDelivered != 2 || !bytes.Equal(retry.Data(), wire) {
			t.Fatal("successor did not receive the original signed delivery")
		}
		goOriginResult(t, ctx, retry.Data(), "REFUSED", flags...)
		useWriterGeneration(t, "NEXT")
		reopened := requireNativeReceipt(t, ctx, request, "DUPLICATE")
		if reopened.OrderVersion != 0 || !bytes.Equal(reopened.State, state) {
			t.Fatal("Rust replay changed the accepted Go order")
		}
		if err = retry.DoubleAck(ctx); err != nil {
			t.Fatal(err)
		}

		if funded {
			exerciseFundedVenue(t, ctx, s, fundedOrder{&cmd, request}, 0, tenant != fundedWorkingTenant, exerciseFundedGoSuccessor, withdrawalMode)
			requireNativeReceipt(t, ctx, request, "WRITER_FENCED")
		} else {
			exerciseSyntheticGoHandback(t, ctx, s, &cmd, request, 2)
		}

		var observations []int64
		err = reader.QueryRow(ctx, `SELECT c.writer_epoch,c.go_gateway_frame,
			ARRAY(SELECT DISTINCT writer_epoch FROM synthetic_oms_observations WHERE order_id=c.order_id ORDER BY writer_epoch)
			FROM synthetic_oms_commands c WHERE c.order_id=$1`, cmd.OrderId).Scan(&admitted, &retained, &observations)
		wantEpochs := []int64{2}
		if withdrawalMode != noGoWithdrawal {
			wantEpochs = []int64{2, 3}
			var operations []string
			if e := reader.QueryRow(ctx, "SELECT array_agg(operation ORDER BY operation) FROM synthetic_oms_observations WHERE order_id=$1 AND writer_epoch=3", cmd.OrderId).Scan(&operations); e != nil || !slices.Equal(operations, []string{"cancel_pending", "cancelled"}) {
				t.Fatalf("Go withdrawal journal: %v %v", operations, e)
			}
		}
		if err != nil || admitted != 1 || !bytes.Equal(retained, wire) || !slices.Equal(observations, wantEpochs) {
			t.Fatalf("retained first admission or venue lineage changed: epoch %d, observations %v: %v", admitted, observations, err)
		}
		t.Log("Go generation 1 admitted the original signed command, Rust generation 2 recovered its redelivery and venue history, Go generation 3 recovered the same order")
	})
}

func exerciseFundedGoSuccessor(t *testing.T, ctx context.Context, s syntheticSession, receipt syntheticvenue.Receipt,
	addr string, identity *testidentity.Source, event *envelopepb.Envelope, authorizeWithdrawal func(int) *fundedWithdrawalApproval,
) {
	t.Helper()
	tenant := event.TenantId
	operator := syntheticPool(t, ctx, "SWITCH", tenant)
	var generation int
	if err := operator.QueryRow(ctx, "SELECT synthetic_test_recovery_handoff()").Scan(&generation); err != nil || generation != 3 {
		t.Fatalf("funded Rust-to-Go handback: epoch %d: %v", generation, err)
	}
	var state orderpb.OrderState
	if err := proto.Unmarshal(receipt.State, &state); err != nil {
		t.Fatal(err)
	}
	at := event.EventTime
	op, cancelEventID := "dispatch", ""
	if state.Status == orderpb.OrderStatus_ORDER_STATUS_CANCELLED {
		op, cancelEventID = "cancel_pending", event.EventId
	}
	replayed, err := callSyntheticOwner(ctx, os.Getenv("SYNTHETIC_OMS_ADAPTER_URL"), struct {
		Version int `json:"version"`
		syntheticvenue.Command
	}{1, syntheticvenue.Command{
		Op: op, Tenant: tenant, Portfolio: state.PortfolioId, OrderID: state.OrderId,
		Principal: syntheticvenue.OMSIdentity, EventID: event.EventId,
		EffectiveSeconds: at.Seconds, EffectiveNanos: at.Nanos,
	}})
	if err != nil || replayed.OK || replayed.Code != "WRITER_FENCED" {
		t.Fatalf("retired Rust funded adapter: %s %v", replayed.Code, err)
	}
	var withdrawal *fundedWithdrawalApproval
	if authorizeWithdrawal != nil {
		withdrawal = authorizeWithdrawal(generation)
	}
	startSyntheticGoSuccessor(t, ctx, s, tenant, addr, identity, receipt, generation,
		os.Getenv("SYNTHETIC_OMS_RECOVERY_RUNTIME_URL"), cancelEventID, withdrawal)
}
