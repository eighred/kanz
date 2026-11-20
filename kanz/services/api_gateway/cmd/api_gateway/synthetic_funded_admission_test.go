package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"math/big"
	"os"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/services/oms/tools/syntheticvenue"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

const (
	fundedFirst = "3d50f599a11447e088b19eb158b35971"
	fundedNext  = "90567d1829214110ae8a4943b23544cb"
)

type fundedOrder struct {
	command *orderpb.SubmitOrder
	request nativeSubmit
}

func signedFundedOrder(t *testing.T, ctx context.Context, s syntheticSession, id string, racing bool) fundedOrder {
	t.Helper()
	qty, price := &commonpb.Decimal{Coefficient: 125, Exponent: -2}, int64(200)
	if racing {
		qty, price = &commonpb.Decimal{Coefficient: 1}, 98
	}
	s.frame = s.submitOrder("fund-"+id, &orderpb.SubmitOrder{OrderId: id, PortfolioId: "synthetic-portfolio",
		InstrumentId: "SYNTH-USD", Venue: "XSIM", Side: orderpb.Side_SIDE_BUY, Quantity: qty,
		LimitPrice: &commonpb.Decimal{Coefficient: price}, OrderType: orderpb.OrderType_ORDER_TYPE_LIMIT,
		TimeInForce: orderpb.TimeInForce_TIME_IN_FORCE_GTC})
	cmd, request := prepareSyntheticOrder(t, ctx, s, s.frame.GetEnvelope().GetTenantId())
	return fundedOrder{cmd, request}
}

func (o fundedOrder) authorize(t *testing.T, ctx context.Context, principal string, deadline time.Time) {
	t.Helper()
	currencies, amounts, err := syntheticvenue.PhysicalFunding(o.command, []*commonpb.Money{
		{CurrencyCode: "USD", Amount: &commonpb.Decimal{Coefficient: 2}},
		{CurrencyCode: "BNB", Amount: &commonpb.Decimal{Coefficient: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(o.request.Command)
	issuer := syntheticPool(t, ctx, "FUNDING", o.request.Tenant)
	_, err = issuer.Exec(ctx, `INSERT INTO synthetic_oms_funding_authorizations
		(principal_id,portfolio_id,order_id,command_sha256,venue,venue_account_id,currencies,amounts,expires_at)
		VALUES($1,$2,$3,$4,'XSIM','synthetic-spot',$5,$6,$7)`,
		principal, o.request.Portfolio, o.command.OrderId, digest[:], currencies, amounts, deadline)
	if err != nil {
		t.Fatal(err)
	}
}

func requireFundedReason(t *testing.T, ctx context.Context, o fundedOrder, want string) {
	t.Helper()
	r := requireNativeReceipt(t, ctx, o.request, "CONTROL_DENIED")
	if r.Reason != want {
		t.Fatalf("control denied for %q, want %q", r.Reason, want)
	}
}

func requireFundedDispatchHeld(t *testing.T, ctx context.Context, o fundedOrder) {
	t.Helper()
	now := time.Now().UTC()
	r, err := callSyntheticOwner(ctx, os.Getenv("SYNTHETIC_OMS_ADAPTER_URL"), struct {
		Version int `json:"version"`
		syntheticvenue.Command
	}{1, syntheticvenue.Command{Op: "dispatch", OrderID: o.command.OrderId, Tenant: o.request.Tenant,
		Portfolio: o.request.Portfolio, Principal: syntheticvenue.OMSIdentity, EventID: "funded-dispatch",
		EffectiveSeconds: now.Unix(), EffectiveNanos: int32(now.Nanosecond())}})
	if err != nil || r.OK || r.Code != "CONTROL_DENIED" || r.Reason != "FUNDED_LIFECYCLE_HELD" {
		t.Fatalf("funded dispatch produced %q/%q: %v", r.Code, r.Reason, err)
	}
}

func exactCapital(t *testing.T, value string, expected int64) {
	t.Helper()
	r, ok := new(big.Rat).SetString(value)
	if !ok || r.Cmp(big.NewRat(expected, 1)) != 0 {
		t.Fatalf("capital value %q, want %d", value, expected)
	}
}

// Every count is evaluated under the observer's RLS tenant. Each test tenant
// has precisely one portfolio and starts with no orders or committed cash.
func verifyFundedBook(t *testing.T, ctx context.Context, proof *pgxpool.Pool, accepted *fundedOrder, usd int64) {
	t.Helper()
	want, bnb := 0, int64(0)
	if accepted != nil {
		want, bnb = 1, 1
	}
	var orders, claims, facts, commitments, members, observations int
	if err := proof.QueryRow(ctx, `SELECT (SELECT count(*) FROM orders),(SELECT count(*) FROM synthetic_oms_commands),
		(SELECT count(*) FROM outbox),(SELECT count(*) FROM capital_commitments),(SELECT count(*) FROM capital_members),
		(SELECT count(*) FROM synthetic_oms_observations)`).
		Scan(&orders, &claims, &facts, &commitments, &members, &observations); err != nil {
		t.Fatal(err)
	}
	if orders != want || claims != want || facts != want || commitments != 2*want || members != 2*want || observations != 0 {
		t.Fatalf("unexpected orders/claims/facts/commitments/members/observations: %d/%d/%d/%d/%d/%d", orders, claims, facts, commitments, members, observations)
	}
	for _, leg := range []struct {
		currency string
		debit    int64
	}{{"BNB", bnb}, {"USD", usd}} {
		var reserved string
		if err := proof.QueryRow(ctx, `SELECT reserved FROM capital_balances WHERE portfolio_id='synthetic-portfolio' AND currency=$1`, leg.currency).Scan(&reserved); err != nil {
			t.Fatal(err)
		}
		exactCapital(t, reserved, leg.debit)
		if accepted == nil {
			continue
		}
		var required, booked, executed, memberBooked, memberExecuted string
		if err := proof.QueryRow(ctx, `SELECT c.required_debit,c.booked_debit,c.executed_debit,m.booked_debit,m.executed_debit
			FROM capital_commitments c JOIN capital_members m USING(tenant_id,order_id,currency)
			WHERE c.order_id=$1 AND c.portfolio_id=$2 AND m.owner_order_id=c.order_id AND c.currency=$3`,
			accepted.command.OrderId, accepted.request.Portfolio, leg.currency).Scan(&required, &booked, &executed, &memberBooked, &memberExecuted); err != nil {
			t.Fatal(err)
		}
		exactCapital(t, required, leg.debit)
		for _, value := range []string{booked, executed, memberBooked, memberExecuted} {
			exactCapital(t, value, 0)
		}
	}
	if accepted != nil {
		var state, hash []byte
		var principal, key, subject string
		if err := proof.QueryRow(ctx, `SELECT o.state,c.command_sha256,c.principal_id,c.idempotency_key,f.subject FROM orders o
			JOIN synthetic_oms_commands c USING(tenant_id,order_id,portfolio_id) JOIN outbox f ON f.tenant_id=o.tenant_id AND f.partition_key=o.order_id
			WHERE o.order_id=$1 AND o.portfolio_id=$2`, accepted.command.OrderId, accepted.request.Portfolio).
			Scan(&state, &hash, &principal, &key, &subject); err != nil {
			t.Fatal(err)
		}
		var order orderpb.OrderState
		digest := sha256.Sum256(accepted.request.Command)
		if proto.Unmarshal(state, &order) != nil || order.OrderId != accepted.command.OrderId || order.PortfolioId != accepted.request.Portfolio ||
			order.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW || order.GetVenueAccountId() != syntheticvenue.Account || !proto.Equal(order.OrderedQuantity, accepted.command.Quantity) ||
			!proto.Equal(order.LimitPrice, accepted.command.LimitPrice) || !bytes.Equal(hash, digest[:]) ||
			principal != accepted.request.Principal || key != accepted.request.Key || subject != syntheticvenue.AcceptedSubject {
			t.Fatal("durable order, admission claim and accepted FACT do not match the signed submission")
		}
	}
}

// Keep the USD row locked until both native writers are waiting on it,
// directly or through the other writer's BNB lock. A sequential run cannot pass.
func observeCompetingWriters(t *testing.T, ctx context.Context, proof *pgxpool.Pool, launch func()) {
	t.Helper()
	holder, err := proof.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx)
	var pid int32
	if err := holder.QueryRow(ctx, "SELECT pg_backend_pid() FROM synthetic_hold_competing_cash()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	launch()
	wait, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	for {
		var blocked int
		err := proof.QueryRow(wait, `WITH RECURSIVE waiting(pid) AS (
			SELECT $1::integer UNION SELECT a.pid FROM waiting w CROSS JOIN pg_stat_activity a
			WHERE a.datname=current_database() AND a.usename='kanz_synthetic_oms_runtime' AND w.pid=ANY(pg_blocking_pids(a.pid))
		) SELECT count(*) FROM waiting WHERE pid<>$1`, pid).Scan(&blocked)
		if err != nil {
			t.Fatalf("waiting for overlapping native transactions: %v", err)
		}
		if blocked == 2 {
			break
		}
		select {
		case <-wait.Done():
			t.Fatalf("observed %d blocked native writers, want 2", blocked)
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func exerciseFundedCompetition(t *testing.T, ctx context.Context, proof *pgxpool.Pool, first, second fundedOrder) {
	t.Helper()
	type outcome struct {
		order   fundedOrder
		receipt syntheticvenue.Receipt
		err     error
	}
	answers := make(chan outcome, 2)
	observeCompetingWriters(t, ctx, proof, func() {
		for _, candidate := range []fundedOrder{first, second} {
			go func() {
				r, err := callSyntheticOwner(ctx, os.Getenv("SYNTHETIC_OMS_RUNTIME_URL"), candidate.request)
				answers <- outcome{candidate, r, err}
			}()
		}
	})
	var winner *fundedOrder
	for range 2 {
		answer := <-answers
		if answer.err != nil {
			t.Fatal(answer.err)
		}
		if answer.receipt.OK && answer.receipt.Code == "ACCEPTED" && winner == nil {
			winner = &answer.order
		} else if answer.receipt.OK || answer.receipt.Code != "CONTROL_DENIED" || answer.receipt.Reason != "FUNDING_INSUFFICIENT" {
			t.Fatalf("unexpected concurrent admission %q/%q", answer.receipt.Code, answer.receipt.Reason)
		}
	}
	if winner == nil {
		t.Fatal("no funded order acquired available cash")
	}
	verifyFundedBook(t, ctx, proof, winner, 100)
}

func TestSyntheticFundedOrderAdmission(t *testing.T) {
	if os.Getenv("KANZ_SYNTHETIC_OMS_BIN") == "" {
		t.Skip("requires the isolated funded synthetic order fixture")
	}
	for _, scenario := range []string{"ok", "short", "short-fee", "race", "rollback", "stale", "foreign"} {
		t.Run(scenario, func(t *testing.T) {
			tenant := "synthetic-funded-" + scenario
			exerciseSyntheticSession(t, tenant, func(ctx context.Context, s syntheticSession) {
				o := signedFundedOrder(t, ctx, s, fundedFirst, scenario == "race")
				proof := syntheticPool(t, ctx, "PROOF", tenant)
				// The normal five approvals were written above. Only the distinct
				// funding authority is missing, even if another tenant used this ID.
				requireFundedReason(t, ctx, o, "FUNDING_RESERVATION_REQUIRED")
				verifyFundedBook(t, ctx, proof, nil, 0)
				expires := time.Now().Add(time.Minute)
				if scenario == "stale" {
					expires = time.Now().Add(-time.Minute)
				}
				o.authorize(t, ctx, o.request.Principal, expires)
				switch scenario {
				case "short", "short-fee":
					requireFundedReason(t, ctx, o, "FUNDING_INSUFFICIENT")
					verifyFundedBook(t, ctx, proof, nil, 0)
					return
				case "stale":
					requireFundedReason(t, ctx, o, "FUNDING_AUTHORIZATION_INVALID")
					verifyFundedBook(t, ctx, proof, nil, 0)
					return
				case "race":
					other := signedFundedOrder(t, ctx, s, fundedNext, true)
					other.authorize(t, ctx, other.request.Principal, expires)
					exerciseFundedCompetition(t, ctx, proof, o, other)
					return
				case "rollback":
					requireNativeReceipt(t, ctx, o.request, "STORE_UNAVAILABLE")
					var fired bool
					if err := proof.QueryRow(ctx, "SELECT is_called FROM synthetic_funded_rollback_seen").Scan(&fired); err != nil || !fired {
						t.Fatal("the fixture did not observe all reservations and the accepted FACT before rollback", err)
					}
					verifyFundedBook(t, ctx, proof, nil, 0)
				case "foreign":
					var otherRows int
					if err := proof.QueryRow(ctx, "SELECT count(*) FROM capital_commitments WHERE tenant_id='synthetic-funded-ok' AND order_id=$1", fundedFirst).Scan(&otherRows); err != nil || otherRows != 0 {
						t.Fatal("tenant discovered a foreign reservation", err)
					}
					wrong := signedFundedOrder(t, ctx, s, fundedNext, false)
					wrong.authorize(t, ctx, "user:different-principal", expires)
					requireFundedReason(t, ctx, wrong, "FUNDING_AUTHORIZATION_INVALID")
					verifyFundedBook(t, ctx, proof, nil, 0)
				}
				first := requireNativeReceipt(t, ctx, o.request, "ACCEPTED")
				if scenario == "ok" {
					t.Run("funded_dispatch_held", func(t *testing.T) {
						requireFundedDispatchHeld(t, ctx, o)
						verifyFundedBook(t, ctx, proof, &o, 252)
					})
				}
				second := requireNativeReceipt(t, ctx, o.request, "DUPLICATE")
				if len(first.State) == 0 || !bytes.Equal(first.State, second.State) || first.OrderVersion != second.OrderVersion {
					t.Fatal("restarted owner changed the accepted receipt")
				}
				verifyFundedBook(t, ctx, proof, &o, 252)
			})
		})
	}
}
