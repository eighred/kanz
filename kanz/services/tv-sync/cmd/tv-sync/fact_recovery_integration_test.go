package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/bustest"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/tv-sync/internal/brokerapi"
	"github.com/eighred/kanz/services/tv-sync/internal/projection"
	"github.com/eighred/kanz/services/tv-sync/internal/server"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// A real commit with an injected lost response, not a claimed network partition.
type responseFaultLog struct {
	projection.Log
	fail    atomic.Bool
	release chan struct{}
}

func (l *responseFaultLog) Append(ctx context.Context, f projection.Fact) (bool, int64, error) {
	if l.fail.Load() {
		select {
		case <-l.release:
		case <-ctx.Done():
			return false, 0, ctx.Err()
		}
	}
	fresh, seq, err := l.Log.Append(ctx, f)
	if err == nil && fresh && l.fail.CompareAndSwap(false, true) {
		return false, 0, errors.New("injected lost committed response")
	}
	return fresh, seq, err
}

func retryPool(t *testing.T, ctx context.Context, tenant string) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("TEST_POSTGRES_URL required")
	}
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	var unsafe bool
	if err := admin.QueryRow(ctx, "SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname=current_user").Scan(&unsafe); err != nil || unsafe {
		t.Fatalf("requires NOSUPERUSER NOBYPASSRLS: %v %v", unsafe, err)
	}
	if _, err := admin.Exec(ctx, `DROP SCHEMA IF EXISTS tv_retry_fixture CASCADE; CREATE SCHEMA tv_retry_fixture`); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = "tv_retry_fixture"
	cfg.ConnConfig.RuntimeParams["app.tenant_id"] = tenant
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	files, err := filepath.Glob("../../migrations/*.sql")
	if err != nil || len(files) == 0 {
		t.Fatal("migration fixtures missing", err)
	}
	sort.Strings(files)
	for _, file := range files {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(b)); err != nil {
			t.Fatal(err)
		}
	}
	return pool
}

func TestTVRetainedFactsRecoverWithNATSAndPostgres(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("TEST_NATS_URL required")
	}
	for _, mode := range []string{"committed-response", "database-timeout", "restart-unacked"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			tenant := fmt.Sprintf("tv-retry-%d", time.Now().UnixNano())
			pool := retryPool(t, ctx, tenant)
			nc, err := nats.Connect(url)
			if err != nil {
				t.Fatal(err)
			}
			defer nc.Close()
			js, err := jetstream.New(nc)
			if err != nil {
				t.Fatal(err)
			}
			const subject = "order.order.filled"
			bustest.EnsureSubjects(t, ctx, js, "TV_RETRY_FACT", []string{subject})
			bustest.EnsureSubjects(t, ctx, js, "TV_RETRY_DLQ", []string{"dlq." + subject})
			dlq, err := nc.SubscribeSync("dlq." + subject)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = dlq.Unsubscribe() }()
			if err := nc.Flush(); err != nil {
				t.Fatal(err)
			}
			tuning := bus.RetainedWorkTuning()
			client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: tenant, ConsumerTuning: &tuning})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = client.Close() }()
			producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "tv-retry-fixture", ProducerVersion: "test", Tenant: tenant})
			if err != nil {
				t.Fatal(err)
			}
			source := projection.NewPostgresLog(pool)
			var log projection.Log = source
			fault := &responseFaultLog{Log: source, release: make(chan struct{})}
			if mode != "database-timeout" {
				log = fault
			}
			var unlock func()
			if mode == "database-timeout" {
				tx, err := pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('tv-sync.fact-append'), hashtext(app_current_tenant()))`); err != nil {
					t.Fatal(err)
				}
				var once sync.Once
				unlock = func() { once.Do(func() { _ = tx.Rollback(ctx) }) }
				defer unlock()
			} else {
				var once sync.Once
				unlock = func() { once.Do(func() { close(fault.release) }) }
				defer unlock()
			}
			failed := make(chan struct{}, 4)
			launch := func() (*projection.Projection, *server.Readiness, func()) {
				p := projection.New(time.Now, nil, projection.WithLog(log, tenant))
				if err := p.Rehydrate(ctx); err != nil {
					t.Fatal(err)
				}
				subctx, stop := context.WithCancel(ctx)
				r := server.NewReadiness(subctx)
				consumer, err := newFactConsumer(client)
				if err != nil {
					t.Fatal(err)
				}
				h := guardedFold(r, tenant, func(c context.Context, e *envelopepb.Envelope, b []byte) error {
					if e.TenantId != tenant {
						return nil
					}
					attempt, end := context.WithTimeout(c, 200*time.Millisecond)
					defer end()
					err := p.Handle(attempt, e, b)
					if err != nil {
						select {
						case failed <- struct{}{}:
						default:
						}
					}
					return err
				})
				done := make(chan error, 1)
				go func() {
					done <- runConsumers(subctx, factConsumerRouter{facts: consumer, prices: consumer}, map[string]bus.EventHandler{subject: h}, tenant, r.Set)
				}()
				return p, r, func() {
					stop()
					select {
					case <-done:
					case <-ctx.Done():
						t.Error("consumer failed to drain")
					}
				}
			}
			p, r, stop := launch()
			defer func() { stop() }()
			waitTV(t, ctx, func() bool { return r.Ready() }, "initial empty durable did not catch up")
			now := timestamppb.Now()
			dec := func(n int64) *commonpb.Decimal { return &commonpb.Decimal{Coefficient: n} }
			payload := &orderpb.OrderFilled{OrderId: "order", State: &orderpb.OrderState{OrderId: "order", PortfolioId: "fund", InstrumentId: "BTC", Side: orderpb.Side_SIDE_BUY, Status: orderpb.OrderStatus_ORDER_STATUS_FILLED, OrderedQuantity: dec(2), FilledQuantity: dec(2), LeavesQuantity: dec(0), AverageFillPrice: dec(100), AsOf: now}, Fill: &orderpb.Fill{FillId: "fill", OrderId: "order", InstrumentId: "BTC", Side: orderpb.Side_SIDE_BUY, Quantity: dec(2), Price: dec(100), ExecutedAt: now}}
			if err := producer.Publish(ctx, bus.Event{Subject: subject, EventType: subject, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, Domain: "order", EventTime: time.Now(), PartitionKey: "order", PayloadSchemaRef: "order.v1.OrderFilled:1", Payload: payload}); err != nil {
				t.Fatal(err)
			}
			select {
			case <-failed:
			case <-ctx.Done():
				t.Fatal("real handler failure not reached")
			}
			waitTV(t, ctx, func() bool { return !r.Ready() }, "handler failure did not revoke serving")
			checkTVHTTP(t, r, p, tenant, http.StatusServiceUnavailable)
			// Keep the fault in place across an actual broker redelivery. An
			// application retry alone or a profile flag cannot satisfy this.
			select {
			case <-failed:
			case <-ctx.Done():
				t.Fatal("failed delivery was parked instead of redelivered while outage remained")
			}
			r.Set(true)
			r.FoldResult(tenant, "unrelated", false)
			if r.Ready() {
				t.Fatal("unrelated success reopened unresolved fold")
			}
			if _, err := dlq.NextMsg(100 * time.Millisecond); !errors.Is(err, nats.ErrTimeout) {
				t.Fatalf("transient failure parked: %v", err)
			}
			streamName, err := bustest.StreamFor(ctx, js, subject)
			if err != nil {
				t.Fatal(err)
			}
			st, err := js.Stream(ctx, streamName)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for info := range st.ListConsumers(ctx).Info() {
				if info.Config.FilterSubject == subject && len(info.Config.Durable) >= len(tenant) && info.Config.Durable[:len(tenant)] == tenant {
					found = true
					if info.Config.MaxDeliver != -1 || info.NumAckPending == 0 || info.NumRedelivered == 0 {
						t.Fatalf("broker did not retain failed delivery: %+v", info.Config)
					}
				}
			}
			if !found {
				t.Fatal("actual retained durable missing")
			}
			if mode == "restart-unacked" {
				stop()
				unlock()
				p, r, stop = launch()
			} else {
				unlock()
			}
			waitTV(t, ctx, func() bool { return r.Ready() }, "no later FACT was sent, but redelivery did not repair readiness")
			checkTVHTTP(t, r, p, tenant, http.StatusOK)
			positions, err := p.Positions(tenant, "fund", time.Time{})
			if err != nil || len(positions) != 1 || positions[0].Qty != "2" || positions[0].AvgPrice != "100" {
				t.Fatalf("lost/doubled recovered fact: %+v %v", positions, err)
			}
			var count int
			if err := pool.QueryRow(ctx, "SELECT count(*) FROM tv_facts").Scan(&count); err != nil || count != 1 {
				t.Fatalf("durable facts=%d err=%v", count, err)
			}
			// Positive DLQ control on the same actual transport: malformed bytes are terminal.
			if err := client.Publish(ctx, bus.Message{Subject: subject, Body: []byte("invalid-wire-fixture")}); err != nil {
				t.Fatal(err)
			}
			parked, err := dlq.NextMsg(5 * time.Second)
			if err != nil || string(parked.Data) != "invalid-wire-fixture" {
				t.Fatalf("terminal bytes not parked on actual DLQ: %v", err)
			}
		})
	}
}
func waitTV(t *testing.T, ctx context.Context, ok func() bool, message string) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for !ok() {
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal(message)
		}
	}
}
func checkTVHTTP(t *testing.T, r *server.Readiness, p *projection.Projection, tenant string, want int) {
	t.Helper()
	s := server.New(r, brokerapi.New(p))
	request := httptest.NewRequest("GET", "/broker/accounts", nil)
	auth.SetPrincipalHeaders(request.Header, "fixture", tenant, nil)
	response := httptest.NewRecorder()
	s.ServeHTTP(response, request)
	if response.Code != want {
		t.Fatalf("Broker status%d want%d", response.Code, want)
	}
}
