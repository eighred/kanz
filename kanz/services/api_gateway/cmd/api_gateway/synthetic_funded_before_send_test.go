package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"slices"
	"testing"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"github.com/eighred/kanz/services/oms/tools/syntheticvenue"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

// Count calls on the actual mTLS connection, including requests that never
// produce a venue receipt. A missing row alone cannot rule out a second send.
type fundedRecoveryVenue struct {
	venuepb.VenueAdapterServiceClient
	executes, queries int
}

func (v *fundedRecoveryVenue) Execute(ctx context.Context, r *venuepb.ExecuteRequest, opts ...grpc.CallOption) (*venuepb.ExecuteResponse, error) {
	v.executes++
	return v.VenueAdapterServiceClient.Execute(ctx, r, opts...)
}

func (v *fundedRecoveryVenue) QueryOrder(ctx context.Context, r *venuepb.QueryOrderRequest, opts ...grpc.CallOption) (*venuepb.QueryOrderResponse, error) {
	v.queries++
	return v.VenueAdapterServiceClient.QueryOrder(ctx, r, opts...)
}

func testFundedDispatchBeforeSend(t *testing.T) {
	const tenant, portfolio = "synthetic-funded-before-send", "synthetic-portfolio"
	exerciseSyntheticSession(t, tenant, func(ctx context.Context, s syntheticSession) {
		o := signedFundedOrder(t, ctx, s, fundedFirst, false)
		o.authorize(t, ctx, o.request.Principal, time.Now().Add(time.Minute))
		admitted := requireNativeReceipt(t, ctx, o.request, "ACCEPTED")
		proof := syntheticPool(t, ctx, "PROOF", tenant)
		journal := syntheticPool(t, ctx, "ADAPTER", tenant)
		stream, relay := syntheticFactRelay(t, ctx, s, tenant)
		if n, err := relay.Flush(ctx, fundedFirst); err != nil || n != 1 {
			t.Fatalf("publish funded acceptance: %d %v", n, err)
		}
		consumer, err := stream.CreateConsumer(ctx, jetstream.ConsumerConfig{Durable: "funded-before-send", FilterSubject: syntheticvenue.AcceptedSubject, AckPolicy: jetstream.AckExplicitPolicy})
		if err != nil {
			t.Fatal(err)
		}
		cfg := syntheticvenue.Config{Tenant: tenant, Portfolio: portfolio, FillTime: time.Now().UTC(), FillOnExecute: true}
		control := fundedVenueControl{order: o, proof: proof, issuer: syntheticPool(t, ctx, "CONTROL", tenant), adapter: journal}
		get := nativeSubmit{Version: 1, Op: "get", Tenant: tenant, Portfolio: portfolio, OrderID: fundedFirst, Principal: o.request.Principal}
		var accepted []byte
		var quarantined syntheticvenue.Receipt
		for delivery := 1; delivery <= 3; delivery++ {
			msg, err := consumer.Next(jetstream.FetchMaxWait(3 * time.Second))
			if err != nil {
				t.Fatal(err)
			}
			meta, err := msg.Metadata()
			if err != nil || meta.NumDelivered != uint64(delivery) {
				t.Fatalf("delivery %d: %v", delivery, err)
			}
			var frame envelopepb.EventFrame
			if err := proto.Unmarshal(msg.Data(), &frame); err != nil {
				t.Fatal(err)
			}
			if delivery == 1 {
				accepted = bytes.Clone(msg.Data())
			} else if !bytes.Equal(msg.Data(), accepted) {
				t.Fatal("broker redelivery changed the accepted FACT")
			}
			control.accepted = frame.Envelope
			venue := &fundedRecoveryVenue{VenueAdapterServiceClient: syntheticVenueClient(t, ctx, cfg)}
			control.venue = venue
			var operations []string
			ingress, err := syntheticvenue.NewWorkingPartialIngress(tenant, portfolio, venue, func(ctx context.Context, c syntheticvenue.Command) (syntheticvenue.Receipt, error) {
				control.authorize(t, ctx, c)
				r, err := callSyntheticOwner(ctx, os.Getenv("SYNTHETIC_OMS_ADAPTER_URL"), struct {
					Version int `json:"version"`
					syntheticvenue.Command
				}{1, c})
				if err != nil || !r.OK {
					t.Fatalf("native %s: %s/%s %v", c.Op, r.Code, r.Reason, err)
				}
				operations = append(operations, c.Op+"/"+r.Code)
				if delivery == 1 && r.Code == "DISPATCH_ONCE" {
					// The native process has committed and exited. Its caller loses
					// the result before it can invoke the venue.
					return syntheticvenue.Receipt{}, errors.New("lost committed dispatch claim")
				}
				return r, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := ingress.HandleAccepted(ctx, frame.Envelope, frame.Payload); err == nil {
				t.Fatal("unconfirmed execution was acknowledged")
			}
			current := requireNativeReceipt(t, ctx, get, "FOUND")
			var st orderpb.OrderState
			if err := proto.Unmarshal(current.State, &st); err != nil {
				t.Fatal(err)
			}
			want := []string{"dispatch/DISPATCH_ONCE"}
			wantQueries := 0
			if delivery == 1 {
				if !bytes.Equal(current.State, admitted.State) || current.OrderVersion != admitted.OrderVersion {
					t.Fatal("dispatch claim alone changed visible order state")
				}
			} else {
				want = []string{"dispatch/RECONCILE_REQUIRED", "timeout/UNKNOWN"}
				wantQueries = 1
				var expected orderpb.OrderState
				if err := proto.Unmarshal(admitted.State, &expected); err != nil {
					t.Fatal(err)
				}
				expected.Quarantine = &orderpb.OrderQuarantine{At: frame.Envelope.EventTime, Reason: "synthetic venue outcome unknown"}
				if !proto.Equal(&st, &expected) || current.OrderVersion != admitted.OrderVersion+1 {
					t.Fatal("unknown venue outcome changed more than the quarantine")
				}
				if delivery == 2 {
					quarantined = current
				} else {
					want[1] = "timeout/DUPLICATE"
					if !bytes.Equal(current.State, quarantined.State) || current.OrderVersion != quarantined.OrderVersion {
						t.Fatal("repeated unknown outcome changed the durable order")
					}
				}
			}
			if !slices.Equal(operations, want) || venue.executes != 0 || venue.queries != wantQueries {
				t.Fatalf("delivery %d: receipts %v; execute %d query %d", delivery, operations, venue.executes, venue.queries)
			}
			checkVenueCash(t, ctx, proof, o, 1000, 252, 252, 1, 0, 0)
			var executions, facts int
			if err := journal.QueryRow(ctx, "SELECT count(*) FROM order_executions WHERE order_id=$1", fundedFirst).Scan(&executions); err != nil || executions != 0 {
				t.Fatalf("execution records %d: %v", executions, err)
			}
			if err := proof.QueryRow(ctx, "SELECT count(*) FROM outbox WHERE partition_key=$1", fundedFirst).Scan(&facts); err != nil || facts != 1 {
				t.Fatalf("outbox records %d: %v", facts, err)
			}
			if err := msg.Nak(); err != nil {
				t.Fatal(err)
			}
		}
		info, err := consumer.Info(ctx)
		if err != nil || info.AckFloor.Stream != 0 {
			t.Fatalf("unresolved delivery advanced the durable ACK floor: %v", err)
		}
		t.Log("XSIM pre-send recovery retained 252 USD / 1 BNB; two redeliveries, two queries, zero sends, executions or additional facts")
	})
}
