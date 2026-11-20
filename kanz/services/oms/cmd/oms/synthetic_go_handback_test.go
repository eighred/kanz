//go:build synthetic_handoff

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/pg"
	"github.com/eighred/kanz/internal/platform/halt"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/oms/internal/config"
	"github.com/eighred/kanz/services/oms/internal/order"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/proto"
)

// This fresh process uses the same adapter discovery, durable outbox drain and
// startup sweep as cmd/oms, with only the successor generation's database login.
func TestSyntheticRecoveredGoStartup(t *testing.T) {
	if os.Getenv("KANZ_SYNTHETIC_GO_HANDBACK") != "1" {
		t.Skip("requires disposable Go handback with a retained native order")
	}
	var in struct {
		Tenant, Endpoint, Socket, Broker string
		CancelEventID                    string
		WithdrawalEventID                string
		WithdrawalStage                  string
		State                            []byte
		Version                          int64
		Generation                       int
	}
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil || len(raw) >= 1<<20 || json.Unmarshal(raw, &in) != nil {
		t.Fatal("invalid synthetic startup input")
	}
	tenant := in.Tenant
	terminal := tenant == "synthetic-funded-cancel-fill"
	working := tenant == "synthetic-go-funded-working"
	if in.WithdrawalEventID != "" && !working {
		t.Fatal("a new withdrawal requires the isolated funded working profile")
	}
	switch in.WithdrawalStage {
	case "":
	case "send", "reconcile", "publish":
		if in.WithdrawalEventID == "" {
			t.Fatal("restart stages require the original withdrawal event")
		}
	default:
		t.Fatal("invalid withdrawal restart stage")
	}
	switch {
	case in.CancelEventID == "" && tenant == "synthetic-go-handback" && in.Generation == 2:
	case in.CancelEventID == "" && tenant == "synthetic-go-roundtrip" && in.Generation == 3:
	case in.CancelEventID != "" && terminal && in.Generation == 3:
	case in.CancelEventID == "" && working && in.Generation == 3:
	default:
		t.Fatal("expected an isolated handback tenant and generation")
	}
	loopback := func(addr string) bool {
		ip, port, e := net.SplitHostPort(addr)
		return e == nil && ip == "127.0.0.1" && port != ""
	}
	broker, e := url.Parse(in.Broker)
	if !loopback(in.Endpoint) || e != nil || broker.Scheme != "nats" || !loopback(broker.Host) || broker.User != nil || broker.Path != "" || broker.RawQuery != "" || broker.Fragment != "" || !strings.HasPrefix(in.Socket, "unix:///tmp/") {
		t.Fatal("synthetic loopback endpoints required")
	}
	var state orderpb.OrderState
	if proto.Unmarshal(in.State, &state) != nil || state.OrderId == "" || state.Quarantine != nil || state.VenueAckAt == nil || state.Venue != "XSIM" || state.VenueAccountId != "synthetic-spot" {
		t.Fatal("a verified XSIM order is required")
	}
	wantStatus, wantSweep := orderpb.OrderStatus_ORDER_STATUS_ROUTED, 1
	if terminal {
		wantStatus, wantSweep = orderpb.OrderStatus_ORDER_STATUS_CANCELLED, 0
	} else if working {
		wantStatus = orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED
	}
	if in.WithdrawalStage == "publish" {
		wantSweep = 0
	}
	if state.Status != wantStatus {
		t.Fatalf("expected %s retained state, got %s", wantStatus, state.Status)
	}
	ctx := bus.WithTenantID(t.Context(), tenant)
	dsn := os.Getenv("SYNTHETIC_OMS_NEXT_RUNTIME_URL")
	cfg, e := pgxpool.ParseConfig(dsn)
	role := fmt.Sprintf("kanz_synthetic_oms_runtime_g%d", in.Generation)
	if e != nil || cfg.ConnConfig.Host != "127.0.0.1" || cfg.ConnConfig.Port != 5432 || cfg.ConnConfig.Database != "kanz_synthetic_oms" || cfg.ConnConfig.User != role || len(cfg.ConnConfig.Fallbacks) > 0 {
		t.Fatal("dedicated loopback successor role required")
	}
	pool, e := pg.NewTenantPool(ctx, dsn, tenant)
	if e != nil {
		t.Fatal("could not open scoped successor pool")
	}
	defer pool.Close()
	var active bool
	if e = pool.QueryRow(ctx, "SELECT synthetic_oms_assert_writer()=$1 AND current_user=$2 AND pg_has_role(current_user,'kanz_synthetic_oms_go_runtime','MEMBER') AND NOT(rolsuper OR rolbypassrls) FROM pg_roles WHERE rolname=current_user", in.Generation, role).Scan(&active); e != nil || !active {
		t.Fatalf("successor is not authorized: %v", e)
	}
	logger := slog.Default()
	adapterCfg := config.Config{Tenant: tenant, VenueEndpoints: fmt.Sprintf("%s/%s=%s", state.Venue, state.VenueAccountId, in.Endpoint), SPIFFESocket: in.Socket, RequireVerifiedAccount: true}
	venues, _, closeVenues, e := dialVenues(ctx, adapterCfg, newVenueCapabilityCounters(prometheus.NewRegistry()), logger)
	if e != nil {
		t.Fatal(e)
	}
	defer closeVenues()
	if len(venues) != 1 {
		t.Fatalf("discovered %d venues", len(venues))
	}
	venueRouter := execution.NewRouter(venues)
	if working {
		var rawFill []byte
		if err := pool.QueryRow(ctx, "SELECT fill FROM order_executions WHERE order_id=$1", state.OrderId).Scan(&rawFill); err != nil {
			t.Fatalf("load retained working execution: %v", err)
		}
		var fill orderpb.Fill
		venue, err := venueRouter.Route(&state)
		if err != nil {
			t.Fatalf("route retained working order: %v", err)
		}
		querier, ok := venue.(execution.Querier)
		if proto.Unmarshal(rawFill, &fill) != nil || !ok {
			t.Fatal("working recovery needs one retained execution and typed venue query")
		}
		venues = []execution.Venue{fundedWorkingRecoveryVenue{Venue: venue, query: querier, fill: &fill}}
	}
	brokerConfig := bus.NATSConfig{Name: "oms", URL: in.Broker}
	if in.WithdrawalEventID != "" {
		// Keep the unresolved withdrawal on its dedicated test durable.
		// The retained-retry guard requires unlimited broker delivery.
		brokerConfig.ConsumerTuning = &bus.ConsumerTuning{AckWait: bus.WorkAckWait, MaxDeliver: -1, MaxAckPending: 1}
	}
	if in.WithdrawalStage != "" {
		brokerConfig.ConsumerTuning.AckWait = 3 * time.Second
	}
	client, e := bus.DialNATS(ctx, brokerConfig)
	if e != nil {
		t.Fatal(e)
	}
	defer client.Close()
	publisher, e := bus.NewProducer(client, bus.ProducerConfig{Source: "oms", ProducerVersion: "synthetic-go-successor"})
	if e != nil {
		t.Fatal(e)
	}
	store := order.NewPostgres(pool)
	opts := []order.ServiceOption{order.WithHaltGate(halt.NewGate(nil))}
	if terminal {
		opts = append(opts, order.WithSyntheticHandoffCancellationRecovery(store))
	}
	if in.WithdrawalStage != "" {
		opts = append(opts, order.WithSyntheticHandoffWithdrawal(store))
	}
	svc, e := order.NewService(tenant, store, order.NewEmitter(publisher), nil, execution.NewRouter(venues), nil, logger, opts...)
	if e != nil {
		t.Fatal(e)
	}
	var claims, pending int
	if e = pool.QueryRow(ctx, "SELECT count(*), count(*) FILTER(WHERE published_at IS NULL) FROM outbox WHERE partition_key=$1", state.OrderId).Scan(&claims, &pending); e != nil || claims < 1 {
		t.Fatalf("retained outbox: %d / %d: %v", claims, pending, e)
	}
	if terminal && (claims != 4 || pending != 3) {
		t.Fatalf("funded recovery requires a retained accepted fact and three pending facts, got %d / %d", claims, pending)
	}
	if working {
		wantClaims, wantPending := 3, 2
		switch in.WithdrawalStage {
		case "reconcile":
			wantPending = 0
		case "publish":
			wantClaims, wantPending = 4, 1
		}
		if claims != wantClaims || pending != wantPending {
			t.Fatalf("retained startup facts: wanted %d/%d, got %d/%d", wantClaims, wantPending, claims, pending)
		}
	}
	if n, err := svc.Outbox().DrainOnce(ctx); err != nil || n != pending {
		t.Fatalf("startup outbox drain: got %d, expected %d: %v", n, pending, err)
	}
	if n, err := svc.SweepInterrupted(ctx); err != nil || n != wantSweep {
		t.Fatalf("startup sweep: got %d: %v", n, err)
	}
	if terminal {
		replaySyntheticFundedCancel(t, ctx, client, svc, in.Broker, in.CancelEventID)
	}
	var wire []byte
	var version int64
	var facts, unpublished int
	if e = pool.QueryRow(ctx, "SELECT state,version FROM orders WHERE order_id=$1", state.OrderId).Scan(&wire, &version); e != nil {
		t.Fatalf("read startup state: %v", e)
	}
	if in.WithdrawalStage == "publish" {
		expected, _, err := order.Cancel(&state, state.GetAsOf().AsTime())
		var actual orderpb.OrderState
		if err != nil || proto.Unmarshal(wire, &actual) != nil || actual.GetAsOf().CheckValid() != nil ||
			actual.GetAsOf().AsTime().Before(state.GetAsOf().AsTime()) || version != in.Version+1 {
			t.Fatalf("invalid committed withdrawal state at restart: %d", version)
		}
		expected.AsOf = actual.AsOf
		if !proto.Equal(expected, &actual) {
			t.Fatal("committed withdrawal changed other order fields")
		}
	} else if !bytes.Equal(wire, in.State) || version != in.Version {
		t.Fatalf("startup changed acknowledged order bytes/version: %d vs %d: %v", version, in.Version, e)
	}
	if e = pool.QueryRow(ctx, "SELECT count(*), count(*) FILTER(WHERE published_at IS NULL) FROM outbox WHERE partition_key=$1", state.OrderId).Scan(&facts, &unpublished); e != nil || facts != claims || unpublished != 0 {
		t.Fatalf("unexpected order facts after sweep: %d / %d: %v", facts, unpublished, e)
	}
	t.Logf("GO_HANDBACK Go generation %d drained %d retained FACTs, swept %d working orders and preserved native version %d and %d outbox rows", in.Generation, pending, wantSweep, version, facts)
	if in.WithdrawalEventID != "" {
		var events order.Bus = publisher
		if in.WithdrawalStage == "reconcile" {
			events = stopBeforeWithdrawalFact{Bus: publisher, eventID: in.WithdrawalEventID, orderID: state.OrderId}
		}
		withdrawal, err := order.NewService(tenant, store, order.NewEmitter(events), nil, venueRouter, nil, logger,
			order.WithHaltGate(halt.NewGate(nil)), order.WithSyntheticHandoffWithdrawal(store))
		if err != nil {
			t.Fatal(err)
		}
		if in.WithdrawalStage == "" {
			consumeSyntheticFundedWithdrawal(t, ctx, client, withdrawal, in.Broker, in.WithdrawalEventID)
		} else {
			restartSyntheticFundedWithdrawal(t, ctx, client, withdrawal, in.Broker, in.WithdrawalEventID, in.WithdrawalStage)
		}
	}
}

// This profile has qualified recovery of one retained execution. It does not
// enroll a writer for new funded executions or live venue cancellations.
type fundedWorkingRecoveryVenue struct {
	execution.Venue
	query execution.Querier
	fill  *orderpb.Fill
}

func (v fundedWorkingRecoveryVenue) Execute(context.Context, *orderpb.OrderState) ([]*orderpb.Fill, error) {
	return nil, errors.New("working recovery profile does not send orders")
}

func (v fundedWorkingRecoveryVenue) CancelOrder(context.Context, *orderpb.OrderState) error {
	return errors.New("working recovery profile does not dispatch cancellations")
}

func (v fundedWorkingRecoveryVenue) QueryOrder(ctx context.Context, st *orderpb.OrderState) (execution.OrderView, error) {
	view, err := v.query.QueryOrder(ctx, st)
	if err != nil {
		return execution.OrderView{}, err
	}
	// Ordinary adoption skips an already-applied raw fill ID. Establish
	// content equality and a complete cumulative total before it can do so.
	if view.State != execution.OrderViewPartiallyFilled || len(view.Fills) != 1 || !proto.Equal(view.Fills[0], v.fill) ||
		!dec.InDomain(view.ExecutedQuantity) || !dec.InDomain(st.GetFilledQuantity()) ||
		dec.FromProto(view.ExecutedQuantity).Cmp(dec.FromProto(st.GetFilledQuantity())) != 0 {
		return execution.OrderView{}, errors.New("working venue view contradicts the retained execution")
	}
	return view, nil
}

type workingQuery struct{ view execution.OrderView }

func (q workingQuery) QueryOrder(context.Context, *orderpb.OrderState) (execution.OrderView, error) {
	return q.view, nil
}

func TestFundedWorkingQueryRejectsConflictingExecution(t *testing.T) {
	qty := &commonpb.Decimal{Coefficient: 625, Exponent: -3}
	fill := &orderpb.Fill{FillId: "retained", Quantity: qty, Fee: &commonpb.Money{
		CurrencyCode: "USD", Amount: &commonpb.Decimal{Coefficient: 1},
	}}
	conflict := proto.CloneOf(fill)
	conflict.Fee.Amount.Coefficient = 2
	for _, tc := range []struct {
		name  string
		state execution.OrderViewState
		total *commonpb.Decimal
		fill  *orderpb.Fill
		valid bool
	}{
		{"exact", execution.OrderViewPartiallyFilled, qty, fill, true},
		{"same_id_changed_fee", execution.OrderViewPartiallyFilled, qty, conflict, false},
		{"unreconciled_total", execution.OrderViewPartiallyFilled, &commonpb.Decimal{Coefficient: 1}, fill, false},
		{"missing_total", execution.OrderViewPartiallyFilled, nil, fill, false},
		{"unexpected_terminal", execution.OrderViewFilled, qty, fill, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := fundedWorkingRecoveryVenue{fill: fill, query: workingQuery{execution.OrderView{
				State: tc.state, ExecutedQuantity: tc.total, Fills: []*orderpb.Fill{tc.fill},
			}}}
			_, err := v.QueryOrder(t.Context(), &orderpb.OrderState{FilledQuantity: qty})
			if (err == nil) != tc.valid {
				t.Fatalf("working query acceptance: %v", err)
			}
		})
	}
}
