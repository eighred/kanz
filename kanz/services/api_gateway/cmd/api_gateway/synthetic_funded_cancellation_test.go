package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"testing"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/oms/tools/syntheticvenue"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
)

type fundedCancellation struct {
	nativeSubmit
	EventID string `json:"event_id"`
}

// Derive the request and independent control evidence from the same gateway
// delivery. The submit approval is keyed to different signed bytes.
func cancellationRequest(t *testing.T, frame *envelopepb.EventFrame, portfolio string) fundedCancellation {
	t.Helper()
	env := frame.GetEnvelope()
	var cmd orderpb.CancelOrder
	if proto.Unmarshal(frame.GetPayload(), &cmd) != nil || env.GetEventTime().CheckValid() != nil ||
		env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_COMMAND || env.GetEventType() != "order.order.cancel" ||
		env.GetSource() != "api-gateway" || env.GetTenantId() == "" || env.GetEventId() == "" || env.GetTraceContext() == "" ||
		cmd.GetOrderId() == "" || cmd.GetMetadata().GetTargetId() != cmd.OrderId || env.GetPartitionKey() != cmd.OrderId ||
		!auth.PortfolioEntitled(cmd.GetMetadata().GetPrincipalPortfolios(), portfolio) {
		t.Fatal("gateway cancellation lacks signed scope or source")
	}
	return fundedCancellation{nativeSubmit{Version: 1, Op: "cancel_funded", Tenant: env.TenantId, Principal: cmd.Metadata.Issuer,
		Portfolio: portfolio, Command: frame.Payload}, env.EventId}
}

func authorizeCancellation(t *testing.T, ctx context.Context, frame *envelopepb.EventFrame, portfolio string) fundedCancellation {
	t.Helper()
	return authorizeCancellationUntil(t, ctx, frame, portfolio, time.Now().Add(30*time.Second))
}

func authorizeCancellationUntil(t *testing.T, ctx context.Context, frame *envelopepb.EventFrame, portfolio string, expires time.Time) fundedCancellation {
	t.Helper()
	req := cancellationRequest(t, frame, portfolio)
	env := frame.GetEnvelope()
	lookup := nativeSubmit{Version: 1, Op: "get", Tenant: req.Tenant, Principal: req.Principal, Portfolio: portfolio, OrderID: env.PartitionKey}
	retained, err := callSyntheticOwner(ctx, os.Getenv("SYNTHETIC_OMS_RUNTIME_URL"), lookup)
	if err != nil {
		t.Fatal(err)
	}
	var stateHash, factHash []byte
	switch {
	case retained.OK && retained.Code == "FOUND":
		var state orderpb.OrderState
		if proto.Unmarshal(retained.State, &state) != nil || state.OrderId != lookup.OrderID || state.PortfolioId != portfolio ||
			state.Status != orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW || state.GetVenueAccountId() != syntheticvenue.Account {
			t.Fatal("unexpected order in scoped funding lookup")
		}
		next, fact, err := syntheticvenue.ProjectWithdrawal(&state, env.EventTime.AsTime())
		if err != nil {
			t.Fatal(err)
		}
		hash := func(message proto.Message) []byte {
			t.Helper()
			wire, err := proto.MarshalOptions{Deterministic: true}.Marshal(message)
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(wire)
			return sum[:]
		}
		stateHash, factHash = hash(next), hash(fact)
	case !retained.OK && retained.Code == "NOT_FOUND" && len(retained.State) == 0:
		// No scoped order means this principal can attest to no transition.
	default:
		t.Fatalf("scoped cancellation lookup returned %q/%q", retained.Code, retained.Reason)
	}
	recordCancellationAuthority(t, ctx, req, env, expires, stateHash, factHash)
	return req
}

func recordCancellationAuthority(t *testing.T, ctx context.Context, req fundedCancellation, env *envelopepb.Envelope, expires time.Time, stateHash, factHash []byte) {
	t.Helper()
	digest := sha256.Sum256(req.Command)
	issuer := syntheticPool(t, ctx, "CONTROL", req.Tenant)
	_, err := issuer.Exec(ctx, `INSERT INTO synthetic_oms_cancellation_authorizations
        (tenant_id,principal_id,portfolio_id,order_id,command_sha256,event_id,trace_context,effective_at,observed_at,expires_at,terminal_state_sha256,fact_payload_sha256)
        VALUES ($1,$2,$3,$4,$5,$6,$7,$8,clock_timestamp(),$9,$10,$11)`,
		req.Tenant, req.Principal, req.Portfolio, env.PartitionKey, digest[:], req.EventID, env.TraceContext, env.EventTime.AsTime(), expires, stateHash, factHash)
	if err != nil {
		t.Fatal(err)
	}
}

func cancelReceipt(t *testing.T, ctx context.Context, req fundedCancellation, code string) syntheticvenue.Receipt {
	t.Helper()
	r, err := callSyntheticOwner(ctx, os.Getenv("SYNTHETIC_OMS_RUNTIME_URL"), req)
	ok := code == "CANCELLED" || code == "DUPLICATE"
	if err != nil || r.Code != code || r.OK != ok || (!ok && (len(r.State) != 0 || r.OrderVersion != 0)) {
		t.Fatalf("native cancellation produced %q/%q, want %q: %v", r.Code, r.Reason, code, err)
	}
	return r
}

func checkWithdrawal(t *testing.T, ctx context.Context, proof *pgxpool.Pool, order fundedOrder, cancel *envelopepb.EventFrame, remaining int64) {
	t.Helper()
	id := order.command.OrderId
	status, version, phase, terminalFacts := orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW, int64(0), "accepted", 0
	if cancel != nil {
		status, version, phase, terminalFacts = orderpb.OrderStatus_ORDER_STATUS_CANCELLED, 1, "cancelled", 1
	}
	var wire []byte
	var dbStatus int32
	var dbVersion int64
	var dbPhase string
	var facts, receipts, observations, members, commitments int
	err := proof.QueryRow(ctx, `SELECT o.state,o.status,o.version,c.phase,
        (SELECT count(*) FROM outbox WHERE partition_key=o.order_id),
        (SELECT count(*) FROM synthetic_oms_funded_withdrawals WHERE order_id=o.order_id),
        (SELECT count(*) FROM synthetic_oms_observations WHERE order_id=o.order_id),
        (SELECT count(*) FROM capital_members WHERE order_id=o.order_id),
        (SELECT count(*) FROM capital_commitments WHERE order_id=o.order_id)
        FROM orders o JOIN synthetic_oms_commands c USING(tenant_id,order_id,portfolio_id)
        WHERE o.order_id=$1 AND o.portfolio_id=$2`, id, order.request.Portfolio).Scan(&wire, &dbStatus, &dbVersion, &dbPhase, &facts, &receipts, &observations, &members, &commitments)
	if err != nil {
		t.Fatal(err)
	}
	var state orderpb.OrderState
	if proto.Unmarshal(wire, &state) != nil || state.OrderId != id || state.PortfolioId != order.request.Portfolio ||
		state.Status != status || dbStatus != int32(status) || dbVersion != version || dbPhase != phase ||
		!proto.Equal(state.OrderedQuantity, order.command.Quantity) || state.GetFilledQuantity().GetCoefficient() != 0 ||
		facts != 1+terminalFacts || receipts != terminalFacts || observations != 0 || members != 2 || commitments != 2 {
		t.Fatalf("unexpected persisted %s, status=%d version=%d phase=%s facts=%d receipts=%d observations=%d members=%d commitments=%d", id, dbStatus, dbVersion, dbPhase, facts, receipts, observations, members, commitments)
	}
	for _, leg := range []struct {
		currency     string
		debit, total int64
	}{{"USD", 252, 1000}, {"BNB", 1, 8}} {
		var balance, reserved, admit, required, executed, booked, memberExecuted, memberBooked string
		var cv int64
		err := proof.QueryRow(ctx, `SELECT b.total,b.reserved,c.admission_debit,c.required_debit,c.executed_debit,c.booked_debit,
            m.executed_debit,m.booked_debit,c.version FROM capital_balances b JOIN capital_commitments c USING(tenant_id,portfolio_id,currency)
            JOIN capital_members m ON (m.tenant_id,m.currency,m.owner_order_id,m.order_id)=(c.tenant_id,c.currency,c.order_id,c.order_id)
            WHERE b.portfolio_id=$1 AND c.order_id=$2 AND b.currency=$3`, order.request.Portfolio, id, leg.currency).
			Scan(&balance, &reserved, &admit, &required, &executed, &booked, &memberExecuted, &memberBooked, &cv)
		if err != nil {
			t.Fatal(err)
		}
		exactCapital(t, balance, leg.total)
		exactCapital(t, reserved, remaining*leg.debit)
		exactCapital(t, admit, leg.debit)
		exactCapital(t, required, int64(1-terminalFacts)*leg.debit)
		for _, v := range []string{executed, booked, memberExecuted, memberBooked} {
			exactCapital(t, v, 0)
		}
		if cv != int64(1+terminalFacts) {
			t.Fatalf("commitment %s at version %d want %d", leg.currency, cv, 1+terminalFacts)
		}
	}
	if cancel == nil {
		return
	}
	env := cancel.GetEnvelope()
	var payload, hash []byte
	var event, cause, trace, root, acceptedRoot string
	var receiptVersion int64
	var factTime time.Time
	err = proof.QueryRow(ctx, `SELECT w.event_id,w.command_sha256,w.order_version,f.payload,f.causation_id,f.trace_context,f.correlation_id,a.correlation_id,f.event_time
        FROM synthetic_oms_funded_withdrawals w JOIN outbox f ON f.tenant_id=w.tenant_id AND f.partition_key=w.order_id AND f.subject='order.order.cancelled'
        JOIN outbox a ON a.tenant_id=w.tenant_id AND a.partition_key=w.order_id AND a.subject='order.order.accepted'
        WHERE w.order_id=$1 AND f.event_type=f.subject AND f.producer_service='oms' AND f.envelope_tenant_id=w.tenant_id
        AND f.payload_schema_ref='order.v1.OrderCancelled:1'`, id).
		Scan(&event, &hash, &receiptVersion, &payload, &cause, &trace, &root, &acceptedRoot, &factTime)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(cancel.Payload)
	var fact orderpb.OrderCancelled
	if proto.Unmarshal(payload, &fact) != nil || fact.OrderId != id || !proto.Equal(fact.CancelledQuantity, order.command.Quantity) ||
		!bytes.Equal(hash, digest[:]) || event != env.EventId || cause != env.EventId || trace != env.TraceContext ||
		receiptVersion != version || root == "" || root != acceptedRoot || !factTime.Equal(env.EventTime.AsTime().Truncate(time.Microsecond)) {
		t.Fatal("terminal receipt and FACT lost signed cancellation identity or quantity")
	}
}

func cancellationDelivery(t *testing.T, consumer jetstream.Consumer, expected *envelopepb.EventFrame, deliveries uint64) (jetstream.Msg, *envelopepb.EventFrame) {
	t.Helper()
	msg, err := consumer.Next(jetstream.FetchMaxWait(2 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	info, err := msg.Metadata()
	var frame envelopepb.EventFrame
	if err != nil || info.NumDelivered != deliveries || proto.Unmarshal(msg.Data(), &frame) != nil || !proto.Equal(expected, &frame) {
		t.Fatalf("broker did not deliver the signed cancel for attempt %d: %v", deliveries, err)
	}
	return msg, &frame
}

// The native writer also uses PostgreSQL's clock to decide authority freshness.
func waitCancellationExpiry(t *testing.T, ctx context.Context, proof *pgxpool.Pool, deadline time.Time) {
	t.Helper()
	wait, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := proof.Exec(wait, "SELECT pg_sleep(greatest(0,extract(epoch FROM ($1::timestamptz-clock_timestamp())))::double precision)", deadline); err != nil {
		t.Fatal(err)
	}
	var expired bool
	if err := proof.QueryRow(wait, "SELECT clock_timestamp() >= $1::timestamptz", deadline).Scan(&expired); err != nil || !expired {
		t.Fatal("cancellation authority is still current", err)
	}
}

func competeForWithdrawal(t *testing.T, ctx context.Context, proof *pgxpool.Pool, frames [2]*envelopepb.EventFrame, portfolio string) (int, syntheticvenue.Receipt) {
	t.Helper()
	requests := [2]fundedCancellation{authorizeCancellation(t, ctx, frames[0], portfolio), authorizeCancellation(t, ctx, frames[1], portfolio)}
	if requests[0].EventID == requests[1].EventID || !bytes.Equal(requests[0].Command, requests[1].Command) {
		t.Fatal("expected distinct signed gateway events with identical cancellation terms")
	}
	type result struct {
		index   int
		receipt syntheticvenue.Receipt
		err     error
	}
	results := make(chan result, 2)
	observeCompetingWriters(t, ctx, proof, func() {
		for i, req := range requests {
			go func() {
				r, err := callSyntheticOwner(ctx, os.Getenv("SYNTHETIC_OMS_RUNTIME_URL"), req)
				results <- result{i, r, err}
			}()
		}
	})
	winner := -1
	var terminal, already syntheticvenue.Receipt
	for range 2 {
		outcome := <-results
		if outcome.err != nil || !outcome.receipt.OK {
			t.Fatalf("concurrent cancellation %q/%q: %v", outcome.receipt.Code, outcome.receipt.Reason, outcome.err)
		}
		switch outcome.receipt.Code {
		case "CANCELLED":
			winner, terminal = outcome.index, outcome.receipt
		case "ALREADY_CANCELLED":
			already = outcome.receipt
		default:
			t.Fatalf("unexpected concurrent result %q", outcome.receipt.Code)
		}
	}
	if winner < 0 || terminal.OrderVersion != 1 || already.OrderVersion != terminal.OrderVersion || !bytes.Equal(already.State, terminal.State) {
		t.Fatal("independent requests did not return one cancellation and the same existing terminal order")
	}
	return winner, terminal
}

func TestSyntheticFundedOrderCancellation(t *testing.T) {
	if os.Getenv("KANZ_SYNTHETIC_OMS_BIN") == "" {
		t.Skip("requires the isolated funded cancellation fixture")
	}
	const portfolio = "synthetic-portfolio"
	const trader = "synthetic-trader"
	for _, scenario := range []string{"ok", "race", "rollback", "scope"} {
		t.Run(scenario, func(t *testing.T) {
			tenant := "synthetic-cancel-" + scenario
			exerciseSyntheticSession(t, tenant, func(ctx context.Context, s syntheticSession) {
				admit := func(id string) fundedOrder {
					t.Helper()
					o := signedFundedOrder(t, ctx, s, id, false)
					o.authorize(t, ctx, o.request.Principal, time.Now().Add(time.Minute))
					requireNativeReceipt(t, ctx, o.request, "ACCEPTED")
					return o
				}
				o := admit(fundedFirst)
				proof := syntheticPool(t, ctx, "PROOF", tenant)
				checkWithdrawal(t, ctx, proof, o, nil, 1)
				var sibling fundedOrder
				var remaining int64
				if scenario == "ok" {
					sibling = admit(fundedNext)
					remaining = 1
					checkWithdrawal(t, ctx, proof, sibling, nil, 2)
					requireFundedDispatchHeld(t, ctx, o)
					checkFundedTerminalFactClaim(t, ctx, proof, tenant, o.command.OrderId, false)
				}
				frame := s.cancelOrder(fundedFirst, "cancel-first", trader, portfolio)
				var consumer jetstream.Consumer
				var delivery jetstream.Msg
				if scenario == "ok" {
					var err error
					consumer, err = s.js.CreateOrUpdateConsumer(ctx, "SYNTHETIC_ORDERS", jetstream.ConsumerConfig{Durable: "funded-withdrawal",
						FilterSubject: bus.TenantRoutedSubject(tenant, "order.order.cancel"), AckPolicy: jetstream.AckExplicitPolicy})
					if err != nil {
						t.Fatal(err)
					}
					delivery, frame = cancellationDelivery(t, consumer, frame, 1)
				}
				var terminal syntheticvenue.Receipt
				var expires time.Time
				if scenario == "race" {
					frames := [2]*envelopepb.EventFrame{frame, s.cancelOrder(fundedFirst, "cancel-second", trader, portfolio)}
					winner, r := competeForWithdrawal(t, ctx, proof, frames, portfolio)
					frame, terminal = frames[winner], r
				} else {
					if scenario == "scope" {
						requireCancellationScope(t, ctx, s, o, frame, proof)
					}
					expires = time.Now().Add(30 * time.Second)
					if scenario == "ok" {
						expires = time.Now().Add(3 * time.Second)
					}
					req := authorizeCancellationUntil(t, ctx, frame, portfolio, expires)
					if scenario == "rollback" {
						cancelReceipt(t, ctx, req, "STORE_UNAVAILABLE")
						var fired bool
						if err := proof.QueryRow(ctx, "SELECT is_called FROM synthetic_cancel_rollback_seen").Scan(&fired); err != nil || !fired {
							t.Fatal("missing rollback witness after releasing cash and updating the order", err)
						}
						checkWithdrawal(t, ctx, proof, o, nil, 1)
					}
					terminal = cancelReceipt(t, ctx, req, "CANCELLED")
				}
				checkWithdrawal(t, ctx, proof, o, frame, remaining)
				if scenario == "ok" {
					waitCancellationExpiry(t, ctx, proof, expires)
					// A consumer crashed after committing the cancellation but before
					// its ACK. Negative acknowledgement schedules immediate redelivery.
					if err := delivery.Nak(); err != nil {
						t.Fatal(err)
					}
					delivery, frame = cancellationDelivery(t, consumer, frame, 2)
				}
				replay := cancelReceipt(t, ctx, cancellationRequest(t, frame, portfolio), "DUPLICATE")
				if terminal.OrderVersion != 1 || replay.OrderVersion != terminal.OrderVersion || len(terminal.State) == 0 || !bytes.Equal(terminal.State, replay.State) {
					t.Fatal("restarted native owner did not return its durable withdrawal")
				}
				checkWithdrawal(t, ctx, proof, o, frame, remaining)
				if scenario == "ok" {
					if err := delivery.DoubleAck(ctx); err != nil {
						t.Fatal(err)
					}
					info, err := consumer.Info(ctx)
					if err != nil || info.NumAckPending != 0 || info.NumPending != 0 {
						t.Fatal("durable withdrawal delivery is still outstanding", err)
					}
					checkWithdrawal(t, ctx, proof, sibling, nil, 1)
					checkFundedTerminalFactClaim(t, ctx, proof, tenant, o.command.OrderId, true)
					checkCancelledFactDelivery(t, ctx, s, tenant, o.command.OrderId, frame)
				}
			})
		})
	}
}

func requireCancellationScope(t *testing.T, ctx context.Context, s syntheticSession, o fundedOrder, frame *envelopepb.EventFrame, proof *pgxpool.Pool) {
	t.Helper()
	if got := cancelReceipt(t, ctx, cancellationRequest(t, frame, o.request.Portfolio), "CONTROL_DENIED"); got.Reason != "FUNDING_CANCELLATION_UNAUTHORIZED" {
		t.Fatalf("expected missing independent cancellation authority, got %q", got.Reason)
	}
	stale := s.cancelOrder(o.command.OrderId, "stale-cancel", "synthetic-trader", o.request.Portfolio)
	deadline := time.Now().Add(500 * time.Millisecond)
	expired := authorizeCancellationUntil(t, ctx, stale, o.request.Portfolio, deadline)
	waitCancellationExpiry(t, ctx, proof, deadline)
	if got := cancelReceipt(t, ctx, expired, "CONTROL_DENIED"); got.Reason != "FUNDING_CANCELLATION_UNAUTHORIZED" {
		t.Fatalf("expired cancellation evidence returned %q", got.Reason)
	}
	deny := func(ctx context.Context, req fundedCancellation) {
		t.Helper()
		r, err := callSyntheticOwner(ctx, os.Getenv("SYNTHETIC_OMS_RUNTIME_URL"), req)
		if err != nil || r.OK || r.Code != "NOT_FOUND" || len(r.State) != 0 || r.OrderVersion != 0 {
			t.Fatalf("outside scope received %q/%q: %v", r.Code, r.Reason, err)
		}
	}
	for _, identity := range []struct{ subject, portfolio string }{{"synthetic-colleague", o.request.Portfolio}, {"synthetic-trader", "other-portfolio"}} {
		outside := s.cancelOrder(o.command.OrderId, "outside-"+identity.subject, identity.subject, identity.portfolio)
		deny(ctx, authorizeCancellation(t, ctx, outside, identity.portfolio))
	}
	exerciseSyntheticSession(t, "synthetic-cancel-visitor", func(peerCtx context.Context, peer syntheticSession) {
		outside := peer.cancelOrder(o.command.OrderId, "outside-tenant", "synthetic-trader", o.request.Portfolio)
		deny(peerCtx, authorizeCancellation(t, peerCtx, outside, o.request.Portfolio))
	})
	checkWithdrawal(t, ctx, proof, o, nil, 1)
}
