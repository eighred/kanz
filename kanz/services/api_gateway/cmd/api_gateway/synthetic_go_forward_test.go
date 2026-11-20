package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/oms/tools/syntheticvenue"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
)

func goOriginResult(t *testing.T, ctx context.Context, wire []byte, want string, options ...string) {
	t.Helper()
	child := exec.CommandContext(ctx, "/fixture/go-admit", options...)
	child.Env = []string{"SYNTHETIC_OMS_DATABASE_URL=" + os.Getenv("SYNTHETIC_OMS_RUNTIME_URL")}
	child.Stdin = bytes.NewReader(wire)
	output, err := child.Output()
	if err != nil || strings.TrimSpace(string(output)) != want {
		t.Fatalf("Go origin response %q want %s: %v", output, want, err)
	}
}

// The caller owns ACK and redelivery. Both the entitlement decision and Go
// admission use the exact bytes delivered by the authenticated gateway broker.
func admitSignedGoOrigin(t *testing.T, ctx context.Context, s syntheticSession, msg jetstream.Msg) (*envelopepb.EventFrame, nativeSubmit) {
	t.Helper()
	var original envelopepb.EventFrame
	if proto.Unmarshal(msg.Data(), &original) != nil || !proto.Equal(&original, s.frame) {
		t.Fatal("source differs from authenticated gateway session")
	}
	_, request := prepareSyntheticOrderFrame(t, ctx, s, original.Envelope.TenantId, msg.Data(), time.Now().UTC())
	mutated := proto.Clone(&original).(*envelopepb.EventFrame)
	mutated.Envelope.IdempotencyKey += "-other"
	mutatedWire, err := proto.Marshal(mutated)
	if err != nil {
		t.Fatal(err)
	}
	goOriginResult(t, ctx, mutatedWire, "REFUSED")
	goOriginResult(t, ctx, msg.Data(), "ACCEPTED")
	return &original, request
}

func TestSyntheticGoOriginForward(t *testing.T) {
	if os.Getenv("KANZ_SYNTHETIC_GO_FORWARD") != "1" {
		t.Skip("requires dedicated funded and unfunded first-generation fixture")
	}
	for _, tc := range []struct {
		tenant          string
		forward, legacy bool
	}{
		{"synthetic-go-armed", false, false}, {"synthetic-go-legacy-evidence", false, true},
		{"synthetic-go-missing-posture", false, false}, {"synthetic-go-expired", false, false}, {"synthetic-go-forward", true, false},
	} {
		t.Run(tc.tenant, func(t *testing.T) {
			exerciseSyntheticSession(t, tc.tenant, func(ctx context.Context, s syntheticSession) {
				submit := bus.TenantRoutedSubject(tc.tenant, "order.order.submit")
				consumer, err := s.js.CreateConsumer(ctx, "SYNTHETIC_ORDERS", jetstream.ConsumerConfig{Durable: "go-origin", FilterSubject: submit, AckPolicy: jetstream.AckExplicitPolicy})
				if err != nil {
					t.Fatal(err)
				}
				msg, err := consumer.Next(jetstream.FetchMaxWait(3 * time.Second))
				if err != nil {
					t.Fatal(err)
				}
				wire := msg.Data()
				var original envelopepb.EventFrame
				if proto.Unmarshal(wire, &original) != nil || !proto.Equal(&original, s.frame) {
					t.Fatal("source bytes differ from the authenticated broker delivery")
				}
				goOriginResult(t, ctx, wire, "REFUSED")
				runtime := syntheticPool(t, ctx, "RUNTIME", tc.tenant)
				relayDB := syntheticPool(t, ctx, "RELAY", tc.tenant)
				empty := func() {
					t.Helper()
					var n int
					if err := runtime.QueryRow(ctx, "SELECT (SELECT count(*) FROM orders)+(SELECT count(*) FROM synthetic_oms_commands)").Scan(&n); err != nil || n != 0 {
						t.Fatalf("refusal retained %d order/command records: %v", n, err)
					}
					if err := relayDB.QueryRow(ctx, "SELECT count(*) FROM outbox").Scan(&n); err != nil || n != 0 {
						t.Fatalf("refusal retained %d facts: %v", n, err)
					}
				}
				if !tc.forward {
					input := wire
					if tc.legacy {
						input = nil
					}
					clock := time.Now().UTC()
					if tc.tenant == "synthetic-go-expired" {
						clock = clock.Add(-time.Minute)
					}
					prepareSyntheticOrderFrame(t, ctx, s, tc.tenant, input, clock)
					goOriginResult(t, ctx, wire, "REFUSED")
					empty()
					return
				}
				delivered, request := admitSignedGoOrigin(t, ctx, s, msg)
				var cmd orderpb.SubmitOrder
				if proto.Unmarshal(delivered.Payload, &cmd) != nil {
					t.Fatal("invalid retained submit")
				}
				if err := msg.Nak(); err != nil {
					t.Fatal(err)
				}
				retry, err := consumer.Next(jetstream.FetchMaxWait(3 * time.Second))
				if err != nil {
					t.Fatal(err)
				}
				meta, err := retry.Metadata()
				if err != nil || meta.NumDelivered != 2 || !bytes.Equal(wire, retry.Data()) {
					t.Fatal("missing exact accepted command redelivery")
				}
				goOriginResult(t, ctx, retry.Data(), "DUPLICATE")

				var state, source, command, payload []byte
				var epoch, version int64
				var evidence int
				var principal, key string
				err = runtime.QueryRow(ctx, `SELECT o.state,o.version,c.writer_epoch,c.command,c.go_gateway_frame,
					c.principal_id,c.idempotency_key,cardinality(c.evidence_ids) FROM orders o JOIN synthetic_oms_commands c USING(tenant_id,order_id)
					WHERE o.order_id=$1`, cmd.OrderId).Scan(&state, &version, &epoch, &command, &source, &principal, &key, &evidence)
				if err != nil || version != 0 || epoch != 1 || evidence != 5 || principal != request.Principal || key != request.Key ||
					!bytes.Equal(command, original.Payload) || !bytes.Equal(source, wire) {
					t.Fatalf("Go retained source, version or controls differ: %v", err)
				}
				var st orderpb.OrderState
				var ev orderpb.OrderAccepted
				err = relayDB.QueryRow(ctx, `SELECT payload FROM outbox WHERE partition_key=$1 AND subject='order.order.accepted'
					AND causation_id=$2 AND correlation_id=$2 AND trace_context=$3 AND published_at IS NULL AND attempts=0 AND (SELECT count(*) FROM outbox)=1`,
					cmd.OrderId, original.Envelope.EventId, original.Envelope.TraceContext).Scan(&payload)
				if err != nil || proto.Unmarshal(state, &st) != nil || proto.Unmarshal(payload, &ev) != nil || !proto.Equal(ev.State, &st) ||
					ev.OrderId != cmd.OrderId || st.VenueAccountId != syntheticvenue.Account || st.Status != orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW ||
					st.AcceptedAnnouncedAt == nil || st.VenueAckAt != nil || st.Quarantine != nil {
					t.Fatalf("Go state and pending acceptance differ: %v", err)
				}

				var next int64
				if err := syntheticPool(t, ctx, "SWITCH", tc.tenant).QueryRow(ctx, "SELECT synthetic_test_handoff()").Scan(&next); err != nil || next != 2 {
					t.Fatalf("forward handoff epoch %d: %v", next, err)
				}
				goOriginResult(t, ctx, wire, "REFUSED")
				former, err := callSyntheticOwner(ctx, os.Getenv("SYNTHETIC_OMS_RUNTIME_URL"), request)
				if err != nil || former.OK || former.Code != "WRITER_FENCED" {
					t.Fatalf("retired writer: %s %v", former.Code, err)
				}
				child := os.Getenv("SYNTHETIC_OMS_NEXT_RUNTIME_URL")
				get := nativeSubmit{Version: 1, Op: "get", Tenant: request.Tenant, Portfolio: request.Portfolio, Principal: request.Principal, OrderID: cmd.OrderId}
				for _, op := range []struct {
					r    nativeSubmit
					code string
				}{{get, "FOUND"}, {request, "DUPLICATE"}} {
					reopened, err := callSyntheticOwner(ctx, child, op.r)
					if err != nil || !reopened.OK || reopened.Code != op.code || int64(reopened.OrderVersion) != version || !bytes.Equal(reopened.State, state) {
						t.Fatalf("reopened Rust %s: %s %v", op.r.Op, reopened.Code, err)
					}
				}
				foreign := get
				foreign.Principal = "user:other-trader"
				miss, err := callSyntheticOwner(ctx, child, foreign)
				if err != nil || miss.OK || miss.Code != "NOT_FOUND" || len(miss.State) != 0 {
					t.Fatal("foreign principal discovered Go-origin state")
				}
				if err := retry.DoubleAck(ctx); err != nil {
					t.Fatal(err)
				}
				facts, relay := syntheticFactRelay(t, ctx, s, tc.tenant)
				for _, want := range []int{1, 0} {
					if n, err := relay.Flush(ctx, cmd.OrderId); err != nil || n != want {
						t.Fatalf("Go acceptance relay %d, want %d: %v", n, want, err)
					}
				}
				raw, err := facts.GetLastMsgForSubject(ctx, syntheticvenue.AcceptedSubject)
				if err != nil {
					t.Fatal(err)
				}
				var frame envelopepb.EventFrame
				if proto.Unmarshal(raw.Data, &frame) != nil || bus.Validate(frame.Envelope) != nil || !bytes.Equal(frame.Payload, payload) ||
					frame.Envelope.TenantId != tc.tenant || frame.Envelope.PartitionKey != cmd.OrderId ||
					frame.Envelope.CausationId != original.Envelope.EventId || frame.Envelope.TraceContext != original.Envelope.TraceContext {
					t.Fatal("recovered FACT lost original order or source")
				}
				at := frame.Envelope.EventTime.AsTime()
				claim := struct {
					Version int `json:"version"`
					syntheticvenue.Command
				}{1, syntheticvenue.Command{Op: "dispatch", Tenant: tc.tenant, Portfolio: cmd.PortfolioId,
					OrderID: cmd.OrderId, Principal: syntheticvenue.OMSIdentity, EventID: frame.Envelope.EventId, EffectiveSeconds: at.Unix(), EffectiveNanos: int32(at.Nanosecond())}}
				for _, want := range []string{"DISPATCH_ONCE", "RECONCILE_REQUIRED"} {
					r, err := callSyntheticOwner(ctx, os.Getenv("SYNTHETIC_OMS_NEXT_ADAPTER_URL"), claim)
					if err != nil || !r.OK || r.Code != want {
						t.Fatalf("successor dispatch receipt %s, want %s: %v", r.Code, want, err)
					}
				}
				err = runtime.QueryRow(ctx, `SELECT writer_epoch FROM synthetic_oms_observations WHERE order_id=$1 AND event_id=$2 AND operation='dispatch'
					AND (SELECT count(*) FROM synthetic_oms_observations)=1`, cmd.OrderId, frame.Envelope.EventId).Scan(&epoch)
				if err != nil || epoch != 2 {
					t.Fatalf("successor claim epoch %d: %v", epoch, err)
				}
				t.Log("Go generation 1 acceptance recovered by Rust generation 2; one broker fact and one durable dispatch claim, no venue call")
			})
		})
	}
}
