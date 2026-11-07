package main

import (
	"bytes"
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
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// A real commit with an injected lost response, not a claimed network partition.
type responseFaultLog struct {
	projection.Log
	simulateAmbiguous                             bool
	ambiguous                                     atomic.Bool
	fail                                          atomic.Bool
	release                                       chan struct{}
	fresh, duplicate, appendError, held, injected atomic.Uint64
}

func (l *responseFaultLog) Append(ctx context.Context, f projection.Fact) (bool, int64, error) {
	if l.fail.Load() {
		l.held.Add(1)
		select {
		case <-l.release:
		case <-ctx.Done():
			return false, 0, ctx.Err()
		}
	}
	fresh, seq, err := l.Log.Append(ctx, f)
	if err != nil {
		l.appendError.Add(1)
	} else if fresh {
		l.fresh.Add(1)
	} else {
		l.duplicate.Add(1)
	}
	// A real committed first insert can lose its response. The next confirmed
	// duplicate still proves durability and must arm the injected outage.
	if err == nil && fresh && l.simulateAmbiguous && l.ambiguous.CompareAndSwap(false, true) {
		return false, 0, errors.New("injected ambiguous first commit response")
	}
	if err == nil && l.fail.CompareAndSwap(false, true) {
		l.injected.Add(1)
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
	url := os.Getenv("TEST_TV_NATS_URL")
	if url == "" {
		t.Skip("TEST_TV_NATS_URL required (dedicated broker without other service consumers)")
	}
	for _, mode := range []string{"committed-response", "ambiguous-committed-response", "database-timeout", "restart-unacked"} {
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
			fault := &responseFaultLog{Log: source, release: make(chan struct{}), simulateAmbiguous: mode == "ambiguous-committed-response"}
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
			failed := make(chan bool, 4)
			var targetID atomic.Value
			var handlerSuccess, handlerError atomic.Uint64
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
					if id, ok := targetID.Load().(string); ok && e.EventId == id {
						if err != nil {
							handlerError.Add(1)
						} else {
							handlerSuccess.Add(1)
						}
					}
					if err != nil && (mode == "database-timeout" || fault.fail.Load()) {
						select {
						case failed <- (fault.injected.Load() == 1 && fault.held.Load() > 0):
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
					drain, endDrain := context.WithTimeout(context.Background(), 3*time.Second)
					defer endDrain()
					select {
					case <-done:
					case <-drain.Done():
						t.Error("consumer failed to drain within independent cleanup budget")
					}
				}
			}
			p, r, stop := launch()
			defer func() { stop() }()
			waitTV(t, ctx, func() bool { return r.Ready() }, "initial empty durable did not catch up")
			eventID, err := uuid.NewV7()
			if err != nil {
				t.Fatal(err)
			}
			target := targetDLQ{tenant: tenant, eventID: eventID.String()}
			targetID.Store(target.eventID)
			defer func() {
				if !t.Failed() {
					return
				}
				diagnostic, endDiagnostic := context.WithTimeout(context.Background(), 3*time.Second)
				defer endDiagnostic()
				t.Logf("retained diagnostic: ambiguous=%t injected=%d fresh=%d duplicate=%d append_error=%d held=%d target_handler_success=%d target_handler_error=%d ready=%t", fault.ambiguous.Load(), fault.injected.Load(), fault.fresh.Load(), fault.duplicate.Load(), fault.appendError.Load(), fault.held.Load(), handlerSuccess.Load(), handlerError.Load(), r.Ready())
				err := target.observe(dlq.NextMsg, 100*time.Millisecond, nil)
				result := "no_target_observed"
				if errors.Is(err, errTargetParked) {
					result = "target_parked"
				} else if err != nil {
					result = "observation_failed"
				}
				t.Logf("retained diagnostic: dlq=%s", result)
				streamName, err := bustest.StreamFor(diagnostic, js, subject)
				if err != nil {
					t.Log("retained diagnostic: stream_lookup_failed")
					return
				}
				st, err := js.Stream(diagnostic, streamName)
				if err != nil {
					t.Log("retained diagnostic: stream_open_failed")
					return
				}
				found := 0
				listing := st.ListConsumers(diagnostic)
				for info := range listing.Info() {
					if info.Config.FilterSubject == subject && info.Config.Durable == tenant+"-order_order_filled" {
						found++
						t.Logf("retained diagnostic: max_deliver=%d ack_pending=%d redelivered=%d pending=%d delivered=%d ack_floor=%d", info.Config.MaxDeliver, info.NumAckPending, info.NumRedelivered, info.NumPending, info.Delivered.Consumer, info.AckFloor.Consumer)
					}
				}
				t.Logf("retained diagnostic: matching_durables=%d listing_failed=%t", found, listing.Err() != nil)
			}()
			now := timestamppb.Now()
			dec := func(n int64) *commonpb.Decimal { return &commonpb.Decimal{Coefficient: n} }
			payload := &orderpb.OrderFilled{OrderId: "order", State: &orderpb.OrderState{OrderId: "order", PortfolioId: "fund", InstrumentId: "BTC", Side: orderpb.Side_SIDE_BUY, Status: orderpb.OrderStatus_ORDER_STATUS_FILLED, OrderedQuantity: dec(2), FilledQuantity: dec(2), LeavesQuantity: dec(0), AverageFillPrice: dec(100), AsOf: now}, Fill: &orderpb.Fill{FillId: "fill", OrderId: "order", InstrumentId: "BTC", Side: orderpb.Side_SIDE_BUY, Quantity: dec(2), Price: dec(100), ExecutedAt: now}}
			if err := producer.Publish(ctx, bus.Event{EventID: eventID.String(), Subject: subject, EventType: subject, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, Domain: "order", EventTime: time.Now(), PartitionKey: "order", PayloadSchemaRef: "order.v1.OrderFilled:1", Payload: payload}); err != nil {
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
			var postInjection bool
			select {
			case postInjection = <-failed:
			case <-ctx.Done():
				t.Fatal("second handler failure not observed before test deadline; see bounded diagnostics")
			}
			if mode != "database-timeout" && !postInjection {
				t.Fatal("required post-injection failed broker retry not established; see bounded diagnostics")
			}
			if mode == "ambiguous-committed-response" && (!fault.ambiguous.Load() || fault.fresh.Load() != 1 || fault.duplicate.Load() < 1) {
				t.Fatal("real committed insert then confirmed duplicate not established")
			}
			r.Set(true)
			r.FoldResult(tenant, "unrelated", false)
			if r.Ready() {
				t.Fatal("unrelated success reopened unresolved fold")
			}
			if err := target.observe(dlq.NextMsg, 100*time.Millisecond, nil); err != nil {
				t.Fatal(err)
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
			storedPayload, err := proto.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			var recorded []byte
			if err := pool.QueryRow(ctx, "SELECT payload FROM tv_facts WHERE event_id=$1", target.eventID).Scan(&recorded); err != nil || !bytes.Equal(recorded, storedPayload) {
				t.Fatal("durable original payload changed")
			}
			// Same subscription spans the outage and recovery. New durables can
			// re-park old malformed history before tenant filtering.
			if err := target.observe(dlq.NextMsg, 100*time.Millisecond, nil); err != nil {
				t.Fatal(err)
			}
			terminal := []byte("invalid-wire-fixture-" + tenant)
			if err := client.Publish(ctx, bus.Message{Subject: subject, Body: terminal}); err != nil {
				t.Fatal(err)
			}
			if err := target.observe(dlq.NextMsg, 5*time.Second, terminal); err != nil {
				t.Fatal(err)
			}
			if err := target.observe(dlq.NextMsg, 100*time.Millisecond, nil); err != nil {
				t.Fatal(err)
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

// targetDLQ scopes a shared subject to this fixture's canonical identity. No raw
// payload is logged. A flooded subscription fails closed at a bounded count.
type targetDLQ struct{ tenant, eventID string }

var errTargetParked = errors.New("target FACT was parked on DLQ")

func (target targetDLQ) observe(next func(time.Duration) (*nats.Msg, error), window time.Duration, terminal []byte) error {
	deadline := time.Now().Add(window)
	for n := 0; n < 10000; n++ {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		msg, err := next(remaining)
		if errors.Is(err, nats.ErrTimeout) {
			break
		}
		if err != nil {
			return fmt.Errorf("DLQ observation failed: %w", err)
		}
		env, _, decodeErr := bus.Unframe(msg.Data)
		if decodeErr == nil && env.TenantId == target.tenant && env.EventId == target.eventID {
			return errTargetParked
		}
		if terminal != nil && bytes.Equal(msg.Data, terminal) {
			return nil
		}
		if n == 9999 {
			return errors.New("DLQ observation exceeded bounded message budget")
		}
	}
	if terminal != nil {
		return errors.New("unique terminal control not observed on DLQ")
	}
	return nil
}

func TestTargetDLQObservationIsIdentityScoped(t *testing.T) {
	frame := func(tenant, event string) []byte {
		b, err := proto.Marshal(&envelopepb.EventFrame{Envelope: &envelopepb.Envelope{TenantId: tenant, EventId: event}})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	target := targetDLQ{tenant: "tenant", eventID: "event"}
	for _, tc := range []struct {
		name     string
		messages [][]byte
		terminal []byte
		wantErr  bool
	}{
		{"old malformed history", [][]byte{[]byte("invalid-wire-fixture")}, nil, false},
		{"other tenant", [][]byte{frame("other", "event")}, nil, false},
		{"other event", [][]byte{frame("tenant", "other")}, nil, false},
		{"target parked", [][]byte{frame("tenant", "event")}, nil, true},
		{"unique terminal after noise", [][]byte{[]byte("old"), []byte("unique")}, []byte("unique"), false},
		{"unrelated cannot satisfy control", [][]byte{[]byte("old")}, []byte("unique"), true},
		{"target before control", [][]byte{frame("tenant", "event"), []byte("unique")}, []byte("unique"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at := 0
			next := func(time.Duration) (*nats.Msg, error) {
				if at == len(tc.messages) {
					return nil, nats.ErrTimeout
				}
				data := tc.messages[at]
				at++
				return &nats.Msg{Data: data}, nil
			}
			if err := target.observe(next, time.Second, tc.terminal); (err != nil) != tc.wantErr {
				t.Fatalf("error=%v wantError=%v", err, tc.wantErr)
			}
		})
	}
	flood := func(time.Duration) (*nats.Msg, error) { return &nats.Msg{Data: []byte("unrelated")}, nil }
	if err := target.observe(flood, time.Second, nil); err == nil {
		t.Fatal("unbounded unrelated traffic accepted")
	}
}

// These outcome counters distinguish an injected lost response from an actual
// Append error followed by a confirmed duplicate that also arms injection.
// Neither case implies DLQ parking.
type diagnosticAppendLog struct {
	projection.Log
	fresh bool
	err   error
}

func (l *diagnosticAppendLog) Append(context.Context, projection.Fact) (bool, int64, error) {
	return l.fresh, 1, l.err
}

func TestResponseFaultDiagnosticsDistinguishInjection(t *testing.T) {
	for _, injected := range []bool{false, true} {
		t.Run(fmt.Sprint(injected), func(t *testing.T) {
			base := &diagnosticAppendLog{fresh: injected}
			if !injected {
				base.err = context.DeadlineExceeded
			}
			fault := &responseFaultLog{Log: base, release: make(chan struct{})}
			if _, _, err := fault.Append(context.Background(), projection.Fact{}); err == nil {
				t.Fatal("first error missing")
			}
			if injected {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				if _, _, err := fault.Append(ctx, projection.Fact{}); !errors.Is(err, context.Canceled) {
					t.Fatal("held retry not observed")
				}
				if fault.injected.Load() != 1 || fault.fresh.Load() != 1 || fault.held.Load() != 1 || fault.appendError.Load() != 0 {
					t.Fatal("injection counters differ")
				}
			} else {
				base.err = nil
				if fresh, _, err := fault.Append(context.Background(), projection.Fact{}); err == nil || fresh {
					t.Fatal("confirmed duplicate did not inject response loss")
				}
				if fault.injected.Load() != 1 || fault.appendError.Load() != 1 || fault.duplicate.Load() != 1 || !fault.fail.Load() {
					t.Fatal("confirmed duplicate injection misclassified")
				}
			}
		})
	}
}

func TestAmbiguousResponseRequiresConfirmedDuplicateBeforeFault(t *testing.T) {
	base := &diagnosticAppendLog{fresh: true}
	fault := &responseFaultLog{Log: base, release: make(chan struct{}), simulateAmbiguous: true}
	if _, _, err := fault.Append(context.Background(), projection.Fact{}); err == nil || !fault.ambiguous.Load() || fault.fail.Load() || fault.injected.Load() != 0 {
		t.Fatal("ambiguous first response counted as confirmed injection")
	}
	base.fresh = false
	if _, _, err := fault.Append(context.Background(), projection.Fact{}); err == nil || fault.injected.Load() != 1 || fault.duplicate.Load() != 1 {
		t.Fatal("confirmed duplicate did not arm loss")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := fault.Append(ctx, projection.Fact{}); !errors.Is(err, context.Canceled) || fault.held.Load() != 1 {
		t.Fatal("post-injection retry not held")
	}
	close(fault.release)
	if _, _, err := fault.Append(context.Background(), projection.Fact{}); err != nil || fault.injected.Load() != 1 {
		t.Fatal("release reinjected or refused")
	}
}
