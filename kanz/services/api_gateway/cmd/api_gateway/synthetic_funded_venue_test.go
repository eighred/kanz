package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/testidentity"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/oms/tools/syntheticvenue"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func settlementHash(wire []byte) []byte { sum := sha256.Sum256(wire); return sum[:] }

func settlementProtoHash(t *testing.T, msg proto.Message) []byte {
	t.Helper()
	if msg == nil || !msg.ProtoReflect().IsValid() {
		return nil
	}
	wire, err := proto.MarshalOptions{Deterministic: true}.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	return settlementHash(wire)
}

type fundedVenueControl struct {
	order                  fundedOrder
	proof, issuer, adapter *pgxpool.Pool
	venue                  venuepb.VenueAdapterServiceClient
	accepted, cancellation *envelopepb.Envelope
}

func (control *fundedVenueControl) authorize(t *testing.T, ctx context.Context, c syntheticvenue.Command) {
	t.Helper()
	o := control.order
	if c.Tenant != o.request.Tenant || c.Portfolio != o.request.Portfolio || c.OrderID != o.command.OrderId || c.Principal != syntheticvenue.OMSIdentity {
		t.Fatal("foreign settlement observation")
	}
	parent := control.accepted
	if c.Op == "cancel_pending" {
		parent = control.cancellation
	}
	if c.Op == "dispatch" || c.Op == "cancel_pending" {
		at := time.Unix(c.EffectiveSeconds, int64(c.EffectiveNanos))
		if parent.GetEventId() != c.EventID || !parent.GetEventTime().AsTime().Equal(at) {
			t.Fatal("claim differs from delivered command or FACT")
		}
	}
	var recorded bool
	err := control.proof.QueryRow(ctx, "SELECT EXISTS(SELECT FROM synthetic_oms_observations WHERE order_id=$1 AND event_id=$2)", c.OrderID, c.EventID).Scan(&recorded)
	if err != nil {
		t.Fatal(err)
	}
	if recorded {
		return
	} // The native owner must resolve replay from the persisted observation.
	var generation int64
	// The venue writer may be a successor to the writer that admitted the order.
	if err = control.adapter.QueryRow(ctx, "SELECT public.synthetic_oms_assert_writer()").Scan(&generation); err != nil {
		t.Fatal(err)
	}
	var currencies []string
	err = control.proof.QueryRow(ctx, `SELECT ARRAY(SELECT currency FROM capital_members WHERE order_id=c.order_id ORDER BY currency)
		FROM synthetic_oms_commands c WHERE order_id=$1 AND principal_id=$2 AND portfolio_id=$3`, c.OrderID, o.request.Principal, c.Portfolio).Scan(&currencies)
	if err != nil {
		t.Fatal(err)
	}
	read := requireNativeReceipt(t, ctx, nativeSubmit{Version: 1, Op: "get", Tenant: c.Tenant, Portfolio: c.Portfolio, Principal: o.request.Principal, OrderID: c.OrderID}, "FOUND")
	var prior orderpb.OrderState
	if proto.Unmarshal(read.State, &prior) != nil {
		t.Fatal("unreadable scoped order")
	}
	var view *venuepb.QueryOrderResponse
	var executionHash []byte
	if c.Op == "ack" || c.Op == "fill" || c.Op == "cancelled" {
		view, err = control.venue.QueryOrder(ctx, &venuepb.QueryOrderRequest{TenantId: c.Tenant, State: &prior})
		if err != nil {
			t.Fatal(err)
		}
		if (c.Op == "fill" || c.Op == "cancelled") && len(view.GetFills()) == 1 {
			executionHash = settlementProtoHash(t, view.Fills[0])
		}
	}
	p, err := syntheticvenue.ProjectSettlement(&prior, c, view, currencies)
	if err != nil {
		t.Fatalf("project %s: %v", c.Op, err)
	}
	if err := control.recordProjection(ctx, c, generation, int64(read.OrderVersion), read.State, p, executionHash); err != nil {
		t.Fatalf("issue %s authority: %v", c.Op, err)
	}
}

func (control *fundedVenueControl) recordProjection(ctx context.Context, c syntheticvenue.Command, generation, version int64, state []byte, p syntheticvenue.SettlementProjection, executionHash []byte) error {
	digest := func(msg proto.Message) ([]byte, error) {
		if msg == nil || !msg.ProtoReflect().IsValid() {
			return nil, nil
		}
		wire, err := (proto.MarshalOptions{Deterministic: true}).Marshal(msg)
		return settlementHash(wire), err
	}
	next, err := digest(p.Next)
	if err != nil {
		return err
	}
	fact, err := digest(p.Fact)
	if err != nil {
		return err
	}
	_, err = control.issuer.Exec(ctx, `INSERT INTO synthetic_oms_settlement_authorizations(tenant_id,principal_id,portfolio_id,order_id,command_sha256,writer_epoch,
		operation,event_id,effective_seconds,effective_nanos,body_sha256,prior_version,prior_state_sha256,next_state_sha256,fact_payload_sha256,
		venue_terms_sha256,venue_execution_sha256,expires_at,currencies,executed_debits)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,clock_timestamp()+interval '30 seconds',$18,$19)`,
		c.Tenant, control.order.request.Principal, c.Portfolio, c.OrderID, settlementHash(control.order.request.Command), generation, c.Op, c.EventID, c.EffectiveSeconds, c.EffectiveNanos,
		settlementHash(p.Body), version, settlementHash(state), next, fact, settlementHash(p.Terms), executionHash, p.Currencies, p.ExecutedDebits)
	return err
}

// The portfolio cash remains known independently of the OMS status. A partial
// fill alone cannot release the remainder or claim that accounting has booked it.
func checkVenueCash(t *testing.T, ctx context.Context, pool *pgxpool.Pool, o fundedOrder, usdTotal, usdReserved, usdRequired, bnbRequired, executed, booked int64) {
	t.Helper()
	if err := compareVenueCash(ctx, pool, o, usdTotal, usdReserved, usdRequired, bnbRequired, executed, booked); err != nil {
		t.Fatal(err)
	}
}

func compareVenueCash(ctx context.Context, pool *pgxpool.Pool, o fundedOrder, usdTotal, usdReserved, usdRequired, bnbRequired, executed, booked int64) error {
	for _, leg := range []struct {
		currency                                    string
		total, reserved, required, executed, booked int64
	}{{"USD", usdTotal, usdReserved, usdRequired, executed, booked}, {"BNB", 8, bnbRequired, bnbRequired, 0, 0}} {
		var total, reserved, required, ownerExecuted, ownerBooked, memberExecuted, memberBooked string
		err := pool.QueryRow(ctx, `SELECT b.total,b.reserved,c.required_debit,c.executed_debit,c.booked_debit,m.executed_debit,m.booked_debit
			FROM capital_balances b JOIN capital_commitments c USING(tenant_id,portfolio_id,currency)
			JOIN capital_members m ON (m.tenant_id,m.currency,m.owner_order_id,m.order_id)=(c.tenant_id,c.currency,c.order_id,c.order_id)
			WHERE b.portfolio_id=$1 AND c.order_id=$2 AND b.currency=$3`, o.request.Portfolio, o.command.OrderId, leg.currency).
			Scan(&total, &reserved, &required, &ownerExecuted, &ownerBooked, &memberExecuted, &memberBooked)
		if err != nil {
			return err
		}
		for _, amount := range []struct {
			value string
			want  int64
		}{{total, leg.total}, {reserved, leg.reserved}, {required, leg.required}, {ownerExecuted, leg.executed}, {ownerBooked, leg.booked}, {memberExecuted, leg.executed}, {memberBooked, leg.booked}} {
			got, ok := new(big.Rat).SetString(amount.value)
			if !ok || got.Cmp(big.NewRat(amount.want, 1)) != 0 {
				return fmt.Errorf("capital %s value %q, want %d", leg.currency, amount.value, amount.want)
			}
		}
	}
	return nil
}

func verifySettlementReplay(t *testing.T, observed map[string][]string, filledOnCancel bool) {
	t.Helper()
	want := map[string][]string{
		"dispatch":       {"DISPATCH_ONCE", "RECONCILE_REQUIRED"},
		"timeout":        {"UNKNOWN"},
		"ack":            {"APPLIED"},
		"cancel_pending": {"CANCEL_ONCE", "RECONCILE_REQUIRED", "RECONCILE_REQUIRED"},
		"fill":           {"APPLIED", "DUPLICATE", "DUPLICATE", "DUPLICATE"},
		"cancelled":      {"APPLIED", "DUPLICATE"},
	}
	if filledOnCancel {
		want["fill"] = []string{"APPLIED", "DUPLICATE", "DUPLICATE"}
	}
	if len(observed) != len(want) {
		t.Fatalf("unexpected observation counts: %v", observed)
	}
	for op, codes := range want {
		if !slices.Equal(observed[op], codes) {
			t.Fatalf("native %s receipts %v, want %v", op, observed[op], codes)
		}
	}
}

func verifyVenueFill(t *testing.T, ctx context.Context, venue venuepb.VenueAdapterServiceClient, journal *pgxpool.Pool, tenant string, st *orderpb.OrderState, fillTime time.Time, want venuepb.OrderViewState) {
	t.Helper()
	view, err := venue.QueryOrder(ctx, &venuepb.QueryOrderRequest{TenantId: tenant, State: st})
	if err != nil || view.GetState() != want || len(view.GetFills()) != 1 {
		t.Fatalf("XSIM view=%s want=%s: %v", view.GetState(), want, err)
	}
	var records [][]byte
	err = journal.QueryRow(ctx, "SELECT array_agg(fill) FROM order_executions WHERE order_id=$1", st.OrderId).Scan(&records)
	if err != nil || len(records) != 1 {
		t.Fatalf("partial fill journal has %d records: %v", len(records), err)
	}
	var fill orderpb.Fill
	if proto.Unmarshal(records[0], &fill) != nil || !proto.Equal(&fill, view.Fills[0]) || !proto.Equal(fill.Quantity, view.ExecutedQuantity) ||
		fill.Venue != syntheticvenue.MIC || fill.VenueAccountId != syntheticvenue.Account || fill.FillId == "" || fill.FillId != fill.VenueExecutionId ||
		!fill.ExecutedAt.AsTime().Equal(fillTime) || dec.FromProto(fill.Price).RatString() != "200" || dec.FromProto(fill.Quantity).RatString() != "5/8" || fill.GetFee().GetCurrencyCode() != "USD" || dec.FromProto(fill.GetFee().GetAmount()).RatString() != "1" {
		t.Fatal("XSIM execution and OMS journal disagree on the partial fill or commission")
	}
}

func TestSyntheticFundedVenueLifecycle(t *testing.T) {
	if os.Getenv("KANZ_SYNTHETIC_OMS_BIN") == "" {
		t.Skip("requires isolated XSIM, OMS and accounting")
	}
	for _, scenario := range []struct {
		name, tenant string
		extra        time.Duration
		fillOnCancel bool
	}{
		{"partial_cancel_and_book", "synthetic-funded-venue", 0, false},
		{"submicrosecond_receipt", "synthetic-funded-venue-nanos", 321 * time.Nanosecond, false},
		{"first_fill_during_cancel", "synthetic-funded-cancel-fill", 0, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			tenant := scenario.tenant
			exerciseSyntheticSession(t, tenant, func(ctx context.Context, s syntheticSession) {
				prepareSyntheticAccounting(t, ctx, s, tenant)
				o := signedFundedOrder(t, ctx, s, fundedFirst, false)
				o.authorize(t, ctx, o.request.Principal, time.Now().Add(time.Minute))
				requireNativeReceipt(t, ctx, o.request, "ACCEPTED")
				terminal := exerciseFundedVenue(t, ctx, s, o, scenario.extra, scenario.fillOnCancel, nil, noGoWithdrawal, nil)
				if scenario.name == "partial_cancel_and_book" && os.Getenv("SYNTHETIC_BLOTTER_CAPTURE") == "1" {
					// The runner pipes this synthetic native receipt into the separate read-only probe.
					body, err := json.Marshal(map[string]any{"tenant": tenant, "portfolio": o.request.Portfolio, "state": terminal.State})
					if err != nil {
						t.Fatal("cannot encode funded terminal receipt")
					}
					t.Logf("FUNDED_BLOTTER_RECEIPT %s", body)
				}
			})
		})
	}
	t.Run("same_microsecond_pair", testSameMicrosecondPair)
	t.Run("claim_before_venue_send", testFundedDispatchBeforeSend)
}

type fundedHandoffEvidence struct {
	withdrawal *fundedWithdrawalApproval
	remainder  *fundedRemainderRecovery
}

type fundedGoSuccessor func(*testing.T, context.Context, syntheticSession, syntheticvenue.Receipt, string, *testidentity.Source, *envelopepb.Envelope, func(int) fundedHandoffEvidence)

func refuseFundedPartialHandback(t *testing.T, ctx context.Context, tenant string) {
	t.Helper()
	tx, err := syntheticPool(t, ctx, "SWITCH", tenant).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, "SELECT synthetic_test_recovery_handoff()")
	var refused *pgconn.PgError
	if !errors.As(err, &refused) || refused.Code != "P1533" || refused.Message != "funded order is not ready for Go recovery" {
		t.Fatalf("in-flight funded handback was not refused by its settlement guard: %v", err)
	}
}

func requireFundedPendingFacts(t *testing.T, ctx context.Context, proof *pgxpool.Pool, orderID string, wantTotal int, want []string) {
	t.Helper()
	var pending []string
	var total int
	err := proof.QueryRow(ctx, "SELECT count(*),array_agg(subject ORDER BY subject) FILTER (WHERE published_at IS NULL) FROM outbox WHERE partition_key=$1 AND producer_service='oms'", orderID).Scan(&total, &pending)
	if err != nil || total != wantTotal || !slices.Equal(pending, want) {
		t.Fatalf("retained OMS FACTs total=%d pending=%v want=%v: %v", total, pending, want, err)
	}
}

func exerciseFundedVenue(t *testing.T, ctx context.Context, s syntheticSession, o fundedOrder, extra time.Duration, fillOnCancel bool, successor fundedGoSuccessor, withdrawalMode goWithdrawalMode, project remainderProjector) syntheticvenue.Receipt {
	t.Helper()
	tenant, portfolio, orderID := o.request.Tenant, o.request.Portfolio, o.command.OrderId
	proof := syntheticPool(t, ctx, "PROOF", tenant)
	checkVenueCash(t, ctx, proof, o, 1000, 252, 252, 1, 0, 0)
	stream, relay := syntheticFactRelay(t, ctx, s, tenant)
	if n, err := relay.Flush(ctx, orderID); err != nil || n != 1 {
		t.Fatalf("publish accepted FACT: %d %v", n, err)
	}
	accepted, err := stream.CreateConsumer(ctx, jetstream.ConsumerConfig{Durable: "funded-dispatch", FilterSubject: syntheticvenue.AcceptedSubject, AckPolicy: jetstream.AckExplicitPolicy})
	if err != nil {
		t.Fatal(err)
	}
	// Select XSIM's clock value before execution so its retained fill stays reproducible.
	fillTime := time.Now().UTC().Truncate(time.Microsecond).Add(extra)
	if project != nil {
		fillTime = fillTime.Truncate(time.Microsecond).Add(321 * time.Nanosecond)
	}
	cfg := syntheticvenue.Config{Tenant: tenant, Portfolio: portfolio, FillTime: fillTime, LoseExecuteAck: true, FillOnExecute: !fillOnCancel, ExecutionFee: &commonpb.Money{CurrencyCode: "USD", Amount: &commonpb.Decimal{Coefficient: 1}}}
	var sends, queries, withdrawals atomic.Int32
	var withdrawal atomic.Pointer[fundedWithdrawalApproval]
	var lostWithdrawalAck, terminalApproved, sentContradiction atomic.Bool
	count := grpc.UnaryInterceptor(func(ctx context.Context, request any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		approval := withdrawal.Load()
		switch info.FullMethod {
		case venuepb.VenueAdapterService_Execute_FullMethodName:
			sends.Add(1)
		case venuepb.VenueAdapterService_QueryOrder_FullMethodName:
			queries.Add(1)
		case venuepb.VenueAdapterService_CancelOrder_FullMethodName:
			withdrawals.Add(1)
			if approval != nil {
				if err := approval.checkClaim(ctx); err != nil {
					return nil, status.Errorf(codes.FailedPrecondition, "missing durable claim before XSIM withdrawal: %v", err)
				}
			}
		}
		if approval != nil && approval.race != nil && approval.race.armed.Load() {
			if err := approval.race.beforeRPC(ctx, info.FullMethod); err != nil {
				return nil, err
			}
		}
		reply, err := handler(ctx, request)
		if err != nil || approval == nil {
			return reply, err
		}
		switch info.FullMethod {
		case venuepb.VenueAdapterService_CancelOrder_FullMethodName:
			if withdrawals.Load() == 1 {
				if err := approval.checkClaim(ctx); err != nil {
					return nil, status.Errorf(codes.FailedPrecondition, "withdrawal claim before acknowledgement: %v", err)
				}
				lostWithdrawalAck.Store(true)
				if approval.race != nil {
					close(approval.race.ackLost)
				}
				return nil, status.Error(codes.DeadlineExceeded, "lost XSIM withdrawal acknowledgement")
			}
		case venuepb.VenueAdapterService_QueryOrder_FullMethodName:
			view := reply.(*venuepb.QueryOrderResponse)
			if approval.race != nil && approval.race.armed.Load() {
				if err := approval.race.observeQuery(ctx, approval, view); err != nil {
					return nil, err
				}
			}
			if lostWithdrawalAck.Load() && view.GetState() == venuepb.OrderViewState_ORDER_VIEW_STATE_CANCELLED && !terminalApproved.Load() {
				if err := approval.recordTerminal(ctx, view); err != nil {
					return nil, status.Errorf(codes.FailedPrecondition, "terminal settlement approval: %v", err)
				}
				terminalApproved.Store(true)
			}
			if approval.mode == goWithdrawalContradiction && terminalApproved.Load() && view.GetState() == venuepb.OrderViewState_ORDER_VIEW_STATE_CANCELLED {
				// The independent approval already describes the real terminal
				// receipt. Send a different fee over mTLS for the same fill.
				if sentContradiction.CompareAndSwap(false, true) {
					changed := proto.Clone(view).(*venuepb.QueryOrderResponse)
					if len(changed.Fills) != 1 || changed.Fills[0].GetFee().GetAmount().GetCoefficient() != 1 {
						return nil, errors.New("unexpected XSIM fee fixture")
					}
					changed.Fills[0].Fee.Amount.Coefficient = 2
					return changed, nil
				}
				// A new query is the retry after the contradictory response.
				// Check the held cash and original state before releasing it.
				if err := approval.checkClaim(ctx); err != nil {
					return nil, err
				}
			}
		}
		return reply, nil
	})
	venue, addr, identity := startSyntheticVenue(t, ctx, cfg, count)
	control := fundedVenueControl{order: o, proof: proof, issuer: syntheticPool(t, ctx, "CONTROL", tenant), adapter: syntheticPool(t, ctx, "ADAPTER", tenant), venue: venue}
	interrupted := false
	observed := make(map[string][]string)
	owner := func(ctx context.Context, c syntheticvenue.Command) (syntheticvenue.Receipt, error) {
		// Simulate process loss after the fill commit and before the terminal
		// observation. A new delivery must query XSIM and replay its fill.
		if c.Op == "cancelled" && !interrupted {
			interrupted = true
			return syntheticvenue.Receipt{}, errors.New("interrupted after durable fill")
		}
		control.authorize(t, ctx, c)
		r, e := callSyntheticOwner(ctx, os.Getenv("SYNTHETIC_OMS_ADAPTER_URL"), struct {
			Version int `json:"version"`
			syntheticvenue.Command
		}{1, c})
		if e != nil || !r.OK {
			t.Fatalf("native %s: %q/%q: %v", c.Op, r.Code, r.Reason, e)
		}
		observed[c.Op] = append(observed[c.Op], r.Code)
		return r, e
	}
	ingress, err := syntheticvenue.NewWorkingPartialIngress(tenant, portfolio, venue, owner)
	if err != nil {
		t.Fatal(err)
	}
	get := func() (*orderpb.OrderState, syntheticvenue.Receipt) {
		t.Helper()
		r := requireNativeReceipt(t, ctx, nativeSubmit{Version: 1, Op: "get", Tenant: tenant, Portfolio: portfolio, OrderID: orderID, Principal: o.request.Principal}, "FOUND")
		var st orderpb.OrderState
		if err := proto.Unmarshal(r.State, &st); err != nil {
			t.Fatal(err)
		}
		return &st, r
	}
	for attempt := 1; attempt <= 2; attempt++ {
		msg, e := accepted.Next(jetstream.FetchMaxWait(3 * time.Second))
		if e != nil {
			t.Fatal(e)
		}
		meta, e := msg.Metadata()
		if e != nil || meta.NumDelivered != uint64(attempt) {
			t.Fatalf("dispatch delivery %d: %v", attempt, e)
		}
		var frame envelopepb.EventFrame
		if proto.Unmarshal(msg.Data(), &frame) != nil || bus.Validate(frame.Envelope) != nil {
			t.Fatal("invalid accepted FACT")
		}
		control.accepted = frame.Envelope
		if attempt == 1 {
			// The installed settlement path has all five admission approvals and
			// reserved cash. The independent dispatch approval is still absent.
			at := frame.Envelope.EventTime.AsTime()
			denied, e := callSyntheticOwner(ctx, os.Getenv("SYNTHETIC_OMS_ADAPTER_URL"), struct {
				Version int `json:"version"`
				syntheticvenue.Command
			}{1, syntheticvenue.Command{
				Op: "dispatch", Tenant: tenant, Portfolio: portfolio, Principal: syntheticvenue.OMSIdentity, OrderID: orderID, EventID: frame.Envelope.EventId, EffectiveSeconds: at.Unix(), EffectiveNanos: int32(at.Nanosecond())}})
			if e != nil || denied.OK || denied.Code != "CONTROL_DENIED" || denied.Reason != "FUNDED_LIFECYCLE_HELD" || denied.OrderVersion != 0 || len(denied.State) != 0 {
				t.Fatalf("dispatch without settlement authority: %s/%s %v", denied.Code, denied.Reason, e)
			}
			verifyFundedBook(t, ctx, proof, &o, 252)
		}
		e = ingress.HandleAccepted(ctx, frame.Envelope, frame.Payload)
		st, _ := get()
		if attempt == 1 {
			if e == nil || st.Quarantine == nil || st.VenueAckAt != nil || sends.Load() != 1 || queries.Load() != 0 {
				t.Fatal("lost XSIM acknowledgement did not persist uncertainty")
			}
			if e = msg.Nak(); e != nil {
				t.Fatal(e)
			}
			checkVenueCash(t, ctx, proof, o, 1000, 252, 252, 1, 0, 0)
			if successor != nil {
				refuseFundedPartialHandback(t, ctx, tenant)
			}
		} else {
			if e != nil || st.Quarantine != nil || st.VenueAckAt == nil || sends.Load() != 1 || queries.Load() == 0 {
				t.Fatalf("XSIM reconciliation: %v", e)
			}
			if e = msg.DoubleAck(ctx); e != nil {
				t.Fatal(e)
			}
			if fillOnCancel {
				if st.Status != orderpb.OrderStatus_ORDER_STATUS_ROUTED || dec.FromProto(st.FilledQuantity).Sign() != 0 {
					t.Fatal("working acknowledgement invented an execution")
				}
				checkVenueCash(t, ctx, proof, o, 1000, 252, 252, 1, 0, 0)
			} else {
				if st.Status != orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED || dec.FromProto(st.FilledQuantity).RatString() != "5/8" {
					t.Fatal("working fill absent after venue reconciliation")
				}
				checkVenueCash(t, ctx, proof, o, 1000, 252, 252, 1, 126, 0)
				verifyVenueFill(t, ctx, venue, control.adapter, tenant, st, fillTime, venuepb.OrderViewState_ORDER_VIEW_STATE_PARTIALLY_FILLED)
				checkFundedExecutionFactClaims(t, ctx, proof, tenant, orderID)
			}
		}
	}
	if successor != nil && !fillOnCancel {
		_, partial := get()
		requireFundedPendingFacts(t, ctx, proof, orderID, 3, []string{"order.order.partially_filled", "order.order.routed"})
		queriesBefore := queries.Load()
		var authorizeWithdrawal func(int) fundedHandoffEvidence
		if project != nil {
			var expected remainderEvidence
			firstBooked := os.Getenv("KANZ_SYNTHETIC_GO_REMAINDER_FIRST_BOOKED") == "1"
			expectedQueries := queriesBefore + 4
			restart := os.Getenv("KANZ_SYNTHETIC_GO_REMAINDER_RESTART") == "1"
			if restart {
				expectedQueries-- // startup relay replays the retained FACT without querying XSIM
			}
			prepare := func(generation int) fundedHandoffEvidence {
				if firstBooked {
					// The gate requires an unbooked G2 fill. Start and exhaust the ordinary
					// G3 working recovery before Accounting receives the partial FACT.
					startSyntheticGoSuccessor(t, ctx, s, tenant, addr, identity, partial, generation, os.Getenv("SYNTHETIC_OMS_RECOVERY_RUNTIME_URL"), "", nil, "", nil)
					requireFundedPendingFacts(t, ctx, proof, orderID, 3, nil)
					if booked := applyVenueAccounting(t, ctx, s, proof, o, 252, 1); booked != 126 {
						t.Fatal("G3 requires the first execution booked before the XSIM remainder")
					}
					checkVenueCash(t, ctx, proof, o, 874, 126, 252, 1, 126, 126)
					expectedQueries++
				}
				expected = prepareFundedRemainder(t, ctx, control, cfg, partial, generation, project)
				if restart {
					return fundedHandoffEvidence{remainder: &fundedRemainderRecovery{control, partial, expected}}
				}
				return fundedHandoffEvidence{}
			}
			successor(t, ctx, s, partial, addr, identity, control.accepted, prepare)
			if sends.Load() != 1 || withdrawals.Load() != 0 || queries.Load() != expectedQueries {
				t.Fatalf("remainder calls: Execute=%d Cancel=%d Query since handback=%d", sends.Load(), withdrawals.Load(), queries.Load()-queriesBefore)
			}
			return verifyFundedRemainder(t, ctx, s, control, partial, expected, firstBooked)
		}
		if withdrawalMode != noGoWithdrawal {
			authorizeWithdrawal = func(generation int) fundedHandoffEvidence {
				approval := control.prepareGoWithdrawal(t, ctx, s, partial, generation)
				approval.mode = withdrawalMode
				if withdrawalMode == goWithdrawalRace {
					approval.race = newFundedWithdrawalRace()
				}
				withdrawal.Store(approval)
				return fundedHandoffEvidence{withdrawal: approval}
			}
		}
		successor(t, ctx, s, partial, addr, identity, control.accepted, authorizeWithdrawal)
		if withdrawalMode != noGoWithdrawal {
			wantQueries := queriesBefore + 3
			if withdrawalMode == goWithdrawalContradiction {
				if !sentContradiction.Load() {
					t.Fatal("XSIM did not send the contradictory terminal fill")
				}
				wantQueries++
				t.Log("XSIM returned the recorded fill ID with a changed fee over mTLS; the next query observed the original OMS version, withdrawal claim, FACT count and cash holds")
			}
			if r := withdrawal.Load().race; r != nil {
				wantQueries = queriesBefore + 6
				if r.openViews.Load() != 1 || r.terminalViews.Load() != 2 {
					t.Fatalf("racing query views: open=%d terminal=%d", r.openViews.Load(), r.terminalViews.Load())
				}
			}
			if !lostWithdrawalAck.Load() || !terminalApproved.Load() || sends.Load() != 1 || withdrawals.Load() != 1 || queries.Load() != wantQueries {
				t.Fatalf("Go withdrawal after ACK loss: sends=%d cancels=%d queries=%d (previously %d)", sends.Load(), withdrawals.Load(), queries.Load(), queriesBefore)
			}
			if r := withdrawal.Load().race; r != nil {
				t.Logf("XSIM race: %d Execute, %d CancelOrder, %d post-handoff QueryOrder; %d in-flight working view and %d terminal views", sends.Load(), withdrawals.Load(), queries.Load()-queriesBefore, r.openViews.Load(), r.terminalViews.Load())
			}
			return verifyGoWithdrawalBooking(t, ctx, s, control, withdrawal.Load())
		}
		if sends.Load() != 1 || withdrawals.Load() != 0 || queries.Load() != queriesBefore+1 {
			t.Fatalf("Go working recovery: got %d sends, %d withdrawals, %d queries (was %d)", sends.Load(), withdrawals.Load(), queries.Load(), queriesBefore)
		}
		requireFundedPendingFacts(t, ctx, proof, orderID, 3, nil)
		checkVenueCash(t, ctx, proof, o, 1000, 252, 252, 1, 126, 0)
		applyVenueAccounting(t, ctx, s, proof, o, 252, 1)
		t.Log("XSIM open partial retained: one placement, one execution, no withdrawal; after booking 126 USD once, balance 874 USD, remaining holds 126 USD and 1 BNB")
		return partial
	}
	frame := s.cancelOrder(orderID, "cancel-remote", "synthetic-trader", portfolio)
	cancelFrom := orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED
	if fillOnCancel {
		cancelFrom = orderpb.OrderStatus_ORDER_STATUS_ROUTED
		// A fresh XSIM process reads the already working order. Its fill
		// on withdrawal occurs after the signed cancellation was published.
		fillTime = time.Now().UTC()
		if !fillTime.After(frame.Envelope.EventTime.AsTime()) {
			t.Fatal("XSIM clock precedes the signed cancellation")
		}
		cfg.FillTime = fillTime
		venue, addr, identity = startSyntheticVenue(t, ctx, cfg, count)
		control.venue = venue
		ingress, err = syntheticvenue.NewWorkingPartialIngress(tenant, portfolio, venue, owner)
		if err != nil {
			t.Fatal(err)
		}
	}
	cancels, err := s.js.CreateConsumer(ctx, "SYNTHETIC_ORDERS", jetstream.ConsumerConfig{Durable: "funded-remote-cancel", FilterSubject: bus.TenantRoutedSubject(tenant, "order.order.cancel"), AckPolicy: jetstream.AckExplicitPolicy})
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 2; attempt++ {
		msg, delivered := cancellationDelivery(t, cancels, frame, uint64(attempt))
		control.cancellation = delivered.Envelope
		if attempt == 1 {
			req := cancellationRequest(t, delivered, portfolio)
			st, _ := get()
			if st.Status != cancelFrom || st.OrderId != orderID || st.PortfolioId != req.Portfolio || req.Principal != o.request.Principal {
				t.Fatal("signed working cancellation does not match admitted order")
			}
			recordCancellationAuthority(t, ctx, req, delivered.Envelope, time.Now().Add(30*time.Second), nil, nil)
		}
		e := ingress.Cancel(ctx, orderID, delivered.Envelope.EventId, delivered.Envelope.EventTime.AsTime())
		st, _ := get()
		if dec.FromProto(st.FilledQuantity).RatString() != "5/8" || dec.FromProto(st.LeavesQuantity).RatString() != "5/8" {
			t.Fatal("partial fill changed the executed or remaining quantity")
		}
		if attempt == 1 {
			if e == nil || !interrupted || st.Status != orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED {
				t.Fatalf("expected interruption after actual fill: %v", e)
			}
			if successor != nil {
				refuseFundedPartialHandback(t, ctx, tenant)
			}
			checkVenueCash(t, ctx, proof, o, 1000, 252, 252, 1, 126, 0)
			if e = msg.Nak(); e != nil {
				t.Fatal(e)
			}
		} else {
			if e != nil || st.Status != orderpb.OrderStatus_ORDER_STATUS_CANCELLED || st.Quarantine != nil {
				t.Fatalf("terminal venue outcome unresolved: %v", e)
			}
			if e = msg.DoubleAck(ctx); e != nil {
				t.Fatal(e)
			}
		}
	}
	terminalState, terminal := get()
	verifyVenueFill(t, ctx, venue, control.adapter, tenant, terminalState, fillTime, venuepb.OrderViewState_ORDER_VIEW_STATE_CANCELLED)
	if e := ingress.Cancel(ctx, orderID, frame.Envelope.EventId, frame.Envelope.EventTime.AsTime()); e != nil {
		t.Fatal(e)
	}
	_, replayed := get()
	if !bytes.Equal(terminal.State, replayed.State) || terminal.OrderVersion != replayed.OrderVersion {
		t.Fatal("duplicate terminal receipt changed the order")
	}
	verifySettlementReplay(t, observed, fillOnCancel)
	if terminalState.GetAsOf().AsTime().Before(fillTime) {
		t.Fatal("terminal state predates the execution included in it")
	}
	var cause, trace, root string
	var confirmedAt time.Time
	var payload []byte
	err = proof.QueryRow(ctx, "SELECT causation_id,trace_context,correlation_id,event_time,payload FROM outbox WHERE partition_key=$1 AND subject='order.order.cancelled'", orderID).Scan(&cause, &trace, &root, &confirmedAt, &payload)
	var terminalFact orderpb.OrderCancelled
	if err != nil || proto.Unmarshal(payload, &terminalFact) != nil || terminalFact.OrderId != orderID || dec.FromProto(terminalFact.CancelledQuantity).RatString() != "5/8" ||
		cause != frame.Envelope.EventId || trace != frame.Envelope.TraceContext || root != control.accepted.CorrelationId || confirmedAt.Before(frame.Envelope.EventTime.AsTime().Truncate(time.Microsecond)) {
		t.Fatalf("terminal FACT lost signed cancellation, confirmation time or actual remainder: %v", err)
	}
	if !proto.Equal(terminalState.LeavesQuantity, terminalFact.CancelledQuantity) ||
		dec.FromProto(terminalState.FilledQuantity).RatString() != "5/8" {
		t.Fatal("terminal OMS quantities differ from the confirmed fill and cancellation")
	}
	checkVenueCash(t, ctx, proof, o, 1000, 126, 126, 0, 126, 0)
	if successor != nil {
		requireFundedPendingFacts(t, ctx, proof, orderID, 4, []string{"order.order.cancelled", "order.order.partially_filled", "order.order.routed"})
		cash, err := s.js.Stream(ctx, "SYNTHETIC_CASH")
		if err != nil {
			t.Fatal(err)
		}
		opening, err := cash.Info(ctx)
		if err != nil || opening.State.Msgs != 1 {
			t.Fatalf("expected only the accounting opening balance before Go startup: %v", err)
		}
		queriesBefore := queries.Load()
		successor(t, ctx, s, terminal, addr, identity, frame.Envelope, nil)
		if sends.Load() != 1 || withdrawals.Load() != 1 || queries.Load() != queriesBefore {
			t.Fatalf("Go terminal replay changed XSIM calls: %d sends, %d withdrawals, %d queries (was %d)", sends.Load(), withdrawals.Load(), queries.Load(), queriesBefore)
		}
		requireFundedPendingFacts(t, ctx, proof, orderID, 4, nil)
	} else if n, e := relay.Flush(ctx, orderID); e != nil || n != 3 {
		t.Fatalf("publish routed, partially filled, cancelled FACTs: %d %v", n, e)
	}
	booked := applyVenueAccounting(t, ctx, s, proof, o, 126, 0)
	if successor == nil {
		later := signedFundedOrder(t, ctx, s, fundedNext, false)
		later.authorize(t, ctx, later.request.Principal, time.Now().Add(time.Minute))
		requireFundedReason(t, ctx, later, "FUNDING_UNKNOWN")
	}
	checkVenueCash(t, ctx, proof, o, 874, 126-booked, 126, 0, 126, booked)
	var receipts, executions int
	err = syntheticPool(t, ctx, "VENUE", tenant).QueryRow(ctx, "SELECT count(*),count(execution) FROM synthetic_venue.receipts WHERE tenant_id=$1 AND portfolio_id=$2 AND order_id=$3", tenant, portfolio, orderID).Scan(&receipts, &executions)
	if err != nil || receipts != 1 || executions != 1 || sends.Load() != 1 {
		t.Fatalf("XSIM receipts=%d executions=%d Execute calls=%d: %v", receipts, executions, sends.Load(), err)
	}
	t.Logf("XSIM cash result tenant=%s total_USD=874 reserved_USD=%d booked_USD=%d reserved_BNB=0; one Execute, one retained execution", tenant, 126-booked, booked)
	return terminal
}

func applyVenueAccounting(t *testing.T, ctx context.Context, s syntheticSession, proof *pgxpool.Pool, o fundedOrder, usdRequired, bnbRequired int64) int64 {
	t.Helper()
	tenant, portfolio, orderID := o.request.Tenant, o.request.Portfolio, o.command.OrderId
	cashFrames := bookSyntheticFill(t, ctx, s, tenant, orderID)
	apply := syntheticvenue.CapitalCashHandler(syntheticPool(t, ctx, "CASH", tenant), tenant)
	if len(cashFrames) != 2 {
		t.Fatal("expected the original cash revision and fill replay")
	}
	var booked int64
	for i, frame := range cashFrames {
		// The baseline writer cannot attribute this trade. Only its successor
		// can discharge the USD obligation after replaying the immutable fill.
		if i == 1 || !upgradingBook(tenant) {
			booked = 126
		}
		for range 2 {
			if err := apply(bus.WithTenantID(ctx, tenant), frame.Envelope, frame.Payload); err != nil {
				t.Fatal(err)
			}
			checkVenueCash(t, ctx, proof, o, 874, usdRequired-booked, usdRequired, bnbRequired, 126, booked)
		}
	}
	var healthy bool
	if err := syntheticPool(t, ctx, "CASH", tenant).QueryRow(ctx, "SELECT fault_digest IS NULL FROM capital_source_health WHERE tenant_id=$1", tenant).Scan(&healthy); err != nil || !healthy {
		t.Fatal("cash FACT quarantined its source", err)
	}
	var incompleteFresh bool
	err := proof.QueryRow(ctx, `SELECT NOT coverage_complete AND NOT conflicted AND gap_revision=0
	AND observed_at_ns BETWEEN (extract(epoch FROM (clock_timestamp()-interval '60 seconds'))*1e9)::bigint
		AND (extract(epoch FROM clock_timestamp())*1e9)::bigint
	FROM capital_balances WHERE portfolio_id=$1 AND currency='USD'`, portfolio).Scan(&incompleteFresh)
	if err != nil || !incompleteFresh {
		t.Fatalf("expected a current, incomplete USD coverage report: %v", err)
	}
	return booked
}

// Two actual XSIM orders execute within one database microsecond. The ledger
// sees different execution IDs and economic instants; each books its own debit.
func testSameMicrosecondPair(t *testing.T) {
	const tenant, portfolio = "synthetic-funded-venue-pair", "synthetic-portfolio"
	exerciseSyntheticSession(t, tenant, func(ctx context.Context, s syntheticSession) {
		prepareSyntheticAccounting(t, ctx, s, tenant)
		stream, relay := syntheticFactRelay(t, ctx, s, tenant)
		proof := syntheticPool(t, ctx, "PROOF", tenant)
		issuer := syntheticPool(t, ctx, "CONTROL", tenant)
		journal := syntheticPool(t, ctx, "ADAPTER", tenant)
		var orders []fundedOrder
		for _, id := range []string{fundedFirst, fundedNext} {
			o := signedFundedOrder(t, ctx, s, id, false)
			o.authorize(t, ctx, o.request.Principal, time.Now().Add(time.Minute))
			requireNativeReceipt(t, ctx, o.request, "ACCEPTED")
			orders = append(orders, o)
		}
		base := time.Now().UTC().Truncate(time.Microsecond)
		var cashFrames []*envelopepb.EventFrame
		for i, o := range orders {
			id := o.command.OrderId
			at := base.Add([]time.Duration{321 * time.Nanosecond, 789 * time.Nanosecond}[i])
			venue := syntheticVenueClient(t, ctx, syntheticvenue.Config{Tenant: tenant, Portfolio: portfolio, FillTime: at, FillOnExecute: true, ExecutionFee: &commonpb.Money{CurrencyCode: "USD", Amount: &commonpb.Decimal{Coefficient: 1}}})
			control := fundedVenueControl{order: o, proof: proof, issuer: issuer, adapter: journal, venue: venue}
			ingress, err := syntheticvenue.NewWorkingPartialIngress(tenant, portfolio, venue, func(ctx context.Context, c syntheticvenue.Command) (syntheticvenue.Receipt, error) {
				control.authorize(t, ctx, c)
				r, err := callSyntheticOwner(ctx, os.Getenv("SYNTHETIC_OMS_ADAPTER_URL"), struct {
					Version int `json:"version"`
					syntheticvenue.Command
				}{1, c})
				if err != nil || !r.OK {
					t.Fatalf("native %s: %s/%s %v", c.Op, r.Code, r.Reason, err)
				}
				return r, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if n, err := relay.Flush(ctx, id); err != nil || n != 1 {
				t.Fatal("publish accepted FACT", n, err)
			}
			msg, err := stream.GetLastMsgForSubject(ctx, syntheticvenue.AcceptedSubject)
			if err != nil {
				t.Fatal(err)
			}
			var frame envelopepb.EventFrame
			if proto.Unmarshal(msg.Data, &frame) != nil || bus.Validate(frame.Envelope) != nil || frame.Envelope.PartitionKey != id {
				t.Fatal("wrong accepted order")
			}
			control.accepted = frame.Envelope
			if err := ingress.HandleAccepted(ctx, frame.Envelope, frame.Payload); err != nil {
				t.Fatal(err)
			}
			get := nativeSubmit{Version: 1, Op: "get", Tenant: tenant, Portfolio: portfolio, Principal: o.request.Principal, OrderID: id}
			var working orderpb.OrderState
			if proto.Unmarshal(requireNativeReceipt(t, ctx, get, "FOUND").State, &working) != nil || working.Status != orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED {
				t.Fatal("order did not partially execute")
			}
			verifyVenueFill(t, ctx, venue, journal, tenant, &working, at, venuepb.OrderViewState_ORDER_VIEW_STATE_PARTIALLY_FILLED)
			cancel := s.cancelOrder(id, "cancel-pair-"+id, "synthetic-trader", portfolio)
			control.cancellation = cancel.Envelope
			recordCancellationAuthority(t, ctx, cancellationRequest(t, cancel, portfolio), cancel.Envelope, time.Now().Add(30*time.Second), nil, nil)
			if err := ingress.Cancel(ctx, id, cancel.Envelope.EventId, cancel.Envelope.EventTime.AsTime()); err != nil {
				t.Fatal(err)
			}
			var terminal orderpb.OrderState
			if proto.Unmarshal(requireNativeReceipt(t, ctx, get, "FOUND").State, &terminal) != nil || terminal.Status != orderpb.OrderStatus_ORDER_STATUS_CANCELLED || dec.FromProto(terminal.LeavesQuantity).RatString() != "5/8" {
				t.Fatal("partial remainder was not cancelled")
			}
			if n, err := relay.Flush(ctx, id); err != nil || n != 3 {
				t.Fatal("publish routed, partial and cancelled FACTs", n, err)
			}
			cashFrames = append(cashFrames, bookSyntheticTrade(t, ctx, s, tenant, id, i+1)...)
		}
		for _, o := range orders {
			checkVenueCash(t, ctx, proof, o, 1000, 252, 126, 0, 126, 0)
		}
		cash := syntheticPool(t, ctx, "CASH", tenant)
		apply := syntheticvenue.CapitalCashHandler(cash, tenant)
		for i, frame := range cashFrames {
			bookedTradeCount := 1 + i/2
			for range 2 {
				if err := apply(bus.WithTenantID(ctx, tenant), frame.Envelope, frame.Payload); err != nil {
					t.Fatal(err)
				}
				for j, o := range orders {
					var booked int64
					if j < bookedTradeCount {
						booked = 126
					}
					checkVenueCash(t, ctx, proof, o, int64(1000-126*bookedTradeCount), int64(252-126*bookedTradeCount), 126, 0, 126, booked)
				}
			}
		}
		var healthy bool
		if err := cash.QueryRow(ctx, "SELECT fault_digest IS NULL FROM capital_source_health WHERE tenant_id=$1", tenant).Scan(&healthy); err != nil || !healthy {
			t.Fatal("pair cash source quarantined", err)
		}
		t.Logf("XSIM cash result tenant=%s total_USD=748 reserved_USD=0 booked_USD=252 reserved_BNB=0", tenant)
	})
}
