package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/oms/tools/syntheticvenue"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
)

func submitSignedGoFundedOrder(t *testing.T, s syntheticSession) *envelopepb.EventFrame {
	t.Helper()
	return s.submitOrder("fund-"+fundedFirst, &orderpb.SubmitOrder{OrderId: fundedFirst, PortfolioId: "synthetic-portfolio",
		InstrumentId: "SYNTH-USD", Venue: "XSIM", Side: orderpb.Side_SIDE_BUY, Quantity: &commonpb.Decimal{Coefficient: 125, Exponent: -2},
		LimitPrice: &commonpb.Decimal{Coefficient: 200}, OrderType: orderpb.OrderType_ORDER_TYPE_LIMIT, TimeInForce: orderpb.TimeInForce_TIME_IN_FORCE_GTC})
}

func admitSignedFundedGoOrigin(t *testing.T, ctx context.Context, s syntheticSession, msg jetstream.Msg) (*envelopepb.EventFrame, nativeSubmit) {
	t.Helper()
	cmd, request := prepareSyntheticOrderFrame(t, ctx, s, s.frame.Envelope.TenantId, msg.Data(), time.Now().UTC())
	o := fundedOrder{cmd, request}
	o.authorize(t, ctx, request.Principal, time.Now().Add(time.Minute))
	goOriginResult(t, ctx, msg.Data(), "REFUSED")
	goOriginResult(t, ctx, msg.Data(), "ACCEPTED", "--funded")
	return s.frame, request
}

func TestSyntheticGoFundedForward(t *testing.T) {
	if os.Getenv("KANZ_SYNTHETIC_GO_FUNDED_FORWARD") != "1" {
		t.Skip("requires isolated funded Go-origin fixture")
	}
	for _, tenant := range []string{"synthetic-go-funded-mismatch", "synthetic-go-funded-forward"} {
		t.Run(tenant, func(t *testing.T) {
			exerciseSyntheticSession(t, tenant, func(ctx context.Context, s syntheticSession) {
				s.frame = submitSignedGoFundedOrder(t, s)
				c, err := s.js.CreateConsumer(ctx, "SYNTHETIC_ORDERS", jetstream.ConsumerConfig{Durable: "go-funded", FilterSubject: bus.TenantRoutedSubject(tenant, "order.order.submit"), DeliverPolicy: jetstream.DeliverLastPolicy, AckPolicy: jetstream.AckExplicitPolicy})
				if err != nil {
					t.Fatal(err)
				}
				msg, err := c.Next(jetstream.FetchMaxWait(3 * time.Second))
				if err != nil {
					t.Fatal(err)
				}
				proof := syntheticPool(t, ctx, "PROOF", tenant)
				if tenant == "synthetic-go-funded-mismatch" {
					cmd, req := prepareSyntheticOrderFrame(t, ctx, s, tenant, msg.Data(), time.Now().UTC())
					goOriginResult(t, ctx, msg.Data(), "REFUSED", "--funded")
					verifyFundedBook(t, ctx, proof, nil, 0)
					// This approval binds the correct signed bytes, but under-reserves
					// the USD leg by one; the canonical reserver would accept both legs.
					h := sha256.Sum256(req.Command)
					issuer := syntheticPool(t, ctx, "FUNDING", tenant)
					_, err := issuer.Exec(ctx, `INSERT INTO synthetic_oms_funding_authorizations
                        (principal_id,portfolio_id,order_id,command_sha256,venue,venue_account_id,currencies,amounts,expires_at)
                        VALUES($1,$2,$3,$4,'XSIM','synthetic-spot',ARRAY['BNB','USD'],ARRAY['1','251'],clock_timestamp()+interval '1 minute')`,
						req.Principal, req.Portfolio, cmd.OrderId, h[:])
					if err != nil {
						t.Fatal(err)
					}
					goOriginResult(t, ctx, msg.Data(), "REFUSED", "--funded")
					verifyFundedBook(t, ctx, proof, nil, 0)
					return
				}
				original, req := admitSignedFundedGoOrigin(t, ctx, s, msg)
				var cmd orderpb.SubmitOrder
				if err := proto.Unmarshal(original.Payload, &cmd); err != nil {
					t.Fatal(err)
				}
				o := fundedOrder{&cmd, req}
				verifyFundedBook(t, ctx, proof, &o, 252)
				goOriginResult(t, ctx, msg.Data(), "REFUSED")
				if err := msg.Nak(); err != nil {
					t.Fatal(err)
				}
				retry, err := c.Next(jetstream.FetchMaxWait(3 * time.Second))
				if err != nil {
					t.Fatal(err)
				}
				delivery, err := retry.Metadata()
				if err != nil || delivery.NumDelivered != 2 || !bytes.Equal(msg.Data(), retry.Data()) {
					t.Fatal("missing authenticated redelivery")
				}
				goOriginResult(t, ctx, retry.Data(), "DUPLICATE", "--funded")
				var state, wire, payload []byte
				var version int64
				err = proof.QueryRow(ctx, `SELECT o.state,c.go_gateway_frame,b.payload,o.version FROM orders o JOIN synthetic_oms_commands c USING(tenant_id,order_id)
                    JOIN outbox b ON (b.tenant_id,b.partition_key)=(o.tenant_id,o.order_id)
                    WHERE o.order_id=$1 AND c.writer_epoch=1 AND c.go_funding_amounts=ARRAY['1','252'] AND cardinality(c.evidence_ids)=5
                    AND b.subject='order.order.accepted' AND b.published_at IS NULL AND b.attempts=0 AND b.correlation_id=$2 AND b.causation_id=$2`,
					cmd.OrderId, original.Envelope.EventId).Scan(&state, &wire, &payload, &version)
				var accepted orderpb.OrderAccepted
				var pending orderpb.OrderState
				if err != nil || version != 0 || !bytes.Equal(wire, msg.Data()) || proto.Unmarshal(state, &pending) != nil || proto.Unmarshal(payload, &accepted) != nil || !proto.Equal(accepted.State, &pending) {
					t.Fatalf("incomplete Go accepted receipt: %v", err)
				}
				var epoch int64
				if err := syntheticPool(t, ctx, "SWITCH", tenant).QueryRow(ctx, "SELECT synthetic_test_handoff()").Scan(&epoch); err != nil || epoch != 2 {
					t.Fatalf("forward epoch %d: %v", epoch, err)
				}
				goOriginResult(t, ctx, retry.Data(), "REFUSED", "--funded")
				successor := os.Getenv("SYNTHETIC_OMS_NEXT_RUNTIME_URL")
				get := nativeSubmit{Version: 1, Op: "get", Tenant: req.Tenant, Portfolio: req.Portfolio, Principal: req.Principal, OrderID: cmd.OrderId}
				for _, tc := range []struct {
					req  nativeSubmit
					code string
				}{{get, "FOUND"}, {req, "DUPLICATE"}} {
					reopened, err := callSyntheticOwner(ctx, successor, tc.req)
					if err != nil || !reopened.OK || reopened.Code != tc.code || !bytes.Equal(reopened.State, state) || int64(reopened.OrderVersion) != version {
						t.Fatalf("funded %s at successor: %s %v", tc.req.Op, reopened.Code, err)
					}
				}
				verifyFundedBook(t, ctx, proof, &o, 252)
				checkVenueCash(t, ctx, proof, o, 1000, 252, 252, 1, 0, 0)
				if err := retry.DoubleAck(ctx); err != nil {
					t.Fatal(err)
				}
				facts, relay := syntheticFactRelay(t, ctx, s, tenant)
				for _, want := range []int{1, 0} {
					if n, err := relay.Flush(ctx, cmd.OrderId); err != nil || n != want {
						t.Fatalf("funded relay flushed %d, want %d: %v", n, want, err)
					}
				}
				raw, err := facts.GetLastMsgForSubject(ctx, syntheticvenue.AcceptedSubject)
				if err != nil {
					t.Fatal(err)
				}
				var fact envelopepb.EventFrame
				if proto.Unmarshal(raw.Data, &fact) != nil || bus.Validate(fact.Envelope) != nil || !bytes.Equal(fact.Payload, payload) ||
					fact.Envelope.TenantId != tenant || fact.Envelope.PartitionKey != cmd.OrderId || fact.Envelope.CausationId != original.Envelope.EventId ||
					fact.Envelope.CorrelationId != original.Envelope.EventId || fact.Envelope.TraceContext != original.Envelope.TraceContext {
					t.Fatal("relayed funded acceptance lost the signed command or Go state")
				}
			})
		})
	}
}
