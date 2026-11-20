//go:build synthetic_handoff

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/pg"
	"github.com/eighred/kanz/internal/platform/halt"
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
		State                            []byte
		Version                          int64
		Generation                       int
	}
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil || len(raw) >= 1<<20 || json.Unmarshal(raw, &in) != nil {
		t.Fatal("invalid synthetic startup input")
	}
	tenant := in.Tenant
	funded := tenant == "synthetic-funded-cancel-fill"
	switch {
	case in.CancelEventID == "" && tenant == "synthetic-go-handback" && in.Generation == 2:
	case in.CancelEventID == "" && tenant == "synthetic-go-roundtrip" && in.Generation == 3:
	case in.CancelEventID != "" && funded && in.Generation == 3:
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
	if funded {
		wantStatus, wantSweep = orderpb.OrderStatus_ORDER_STATUS_CANCELLED, 0
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
	client, e := bus.DialNATS(ctx, bus.NATSConfig{Name: "oms", URL: in.Broker})
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
	if funded {
		opts = append(opts, order.WithSyntheticHandoffCancellationRecovery(store))
	}
	svc, e := order.NewService(tenant, store, order.NewEmitter(publisher), nil, execution.NewRouter(venues), nil, logger, opts...)
	if e != nil {
		t.Fatal(e)
	}
	var claims, pending int
	if e = pool.QueryRow(ctx, "SELECT count(*), count(*) FILTER(WHERE published_at IS NULL) FROM outbox WHERE partition_key=$1", state.OrderId).Scan(&claims, &pending); e != nil || claims < 1 {
		t.Fatalf("retained outbox: %d / %d: %v", claims, pending, e)
	}
	if funded && (claims != 4 || pending != 3) {
		t.Fatalf("funded recovery requires a retained accepted fact and three pending facts, got %d / %d", claims, pending)
	}
	if n, err := svc.Outbox().DrainOnce(ctx); err != nil || n != pending {
		t.Fatalf("startup outbox drain: got %d, expected %d: %v", n, pending, err)
	}
	if n, err := svc.SweepInterrupted(ctx); err != nil || n != wantSweep {
		t.Fatalf("startup sweep: got %d: %v", n, err)
	}
	if funded {
		replaySyntheticFundedCancel(t, ctx, client, svc, in.Broker, in.CancelEventID)
	}
	var wire []byte
	var version int64
	var facts, unpublished int
	if e = pool.QueryRow(ctx, "SELECT state,version FROM orders WHERE order_id=$1", state.OrderId).Scan(&wire, &version); e != nil || !bytes.Equal(wire, in.State) || version != in.Version {
		t.Fatalf("startup changed acknowledged order bytes/version: %d vs %d: %v", version, in.Version, e)
	}
	if e = pool.QueryRow(ctx, "SELECT count(*), count(*) FILTER(WHERE published_at IS NULL) FROM outbox WHERE partition_key=$1", state.OrderId).Scan(&facts, &unpublished); e != nil || facts != claims || unpublished != 0 {
		t.Fatalf("unexpected order facts after sweep: %d / %d: %v", facts, unpublished, e)
	}
	t.Logf("GO_HANDBACK Go generation %d drained %d retained FACTs, swept %d working orders and preserved native version %d and %d outbox rows", in.Generation, pending, wantSweep, version, facts)
}
