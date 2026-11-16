package recoveryconformance

import (
	"errors"
	"github.com/eighred/kanz/internal/execution"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"github.com/eighred/kanz/services/oms/internal/order"
	"testing"
	"time"
)

func TestRustRecoveryConcurrentGoWriter(t *testing.T) {
	f := newFixture(t)
	st := initial()
	store := order.NewPostgres(f.pool)
	if store.Create(f.ctx, st, nil) != nil {
		t.Fatal("seed order failed")
	}
	release := make(chan struct{})
	v := &venue{tenant: f.tenant, orderID: st.OrderId, description: &venuepb.DescribeResponse{Mic: "MOCK", Account: "fixture-account", AccountVerified: true, ExchangeAccountId: "fixture-exchange-account", SupportedOrderTypes: []orderpb.OrderType{orderpb.OrderType_ORDER_TYPE_LIMIT}}, history: history(st), queryRelease: release}
	tr := startTransport(t, v)
	env := &envelopepb.Envelope{EventId: "observation", TenantId: f.tenant, EventType: execution.SubjectStateHealed, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, CorrelationId: "fixture-correlation"}
	payload := wire(t, &orderpb.StateHealed{OrderId: st.OrderId, Venue: st.Venue, State: st})
	receive := f.recovered(t, 2)
	svc := f.service(t, tr)
	w := f.start(t, tr)
	goDone := make(chan error, 1)
	go func() { goDone <- svc.HandleRecovery(lineage(f.ctx, env), env, payload) }()
	overlapped := make(chan bool, 1)
	// Both actual QueryOrder calls must be held at the independent venue before
	// either reply is released. A scheduling-only concurrent launch is not proof
	// that the two transaction coordinators actually overlapped.
	go func() {
		deadline := time.Now().Add(10 * time.Second)
		for v.queries.Load() < 2 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		observed := v.queries.Load() == 2
		close(release)
		overlapped <- observed
	}()
	r := w.call(map[string]any{"method": "Observation", "envelope": wire(t, env), "payload": payload, "at": clock(), "timeout_ms": 30000})
	if !<-overlapped {
		t.Fatal("cross-writer race did not reach both actual history calls")
	}
	if !r.SessionsClosed || (r.Status != "ok" && (r.Status != "error" || (r.Error != "Conflict" && r.Error != "PublicationUnconfirmed"))) {
		t.Fatal("concurrent Rust outcome outside retriable durable boundary")
	}
	select {
	case err := <-goDone:
		if err != nil && !errors.Is(err, order.ErrConflict) {
			t.Fatal("concurrent Go outcome outside canonical CAS refusal")
		}
	case <-time.After(35 * time.Second):
		t.Fatal("concurrent Go handler did not finish")
	}
	// A losing CAS or relay lock is retriable, never financial success. An exact
	// source retry resolves that ambiguity without changing frozen venue history.
	retried := w.call(map[string]any{"method": "Observation", "envelope": wire(t, env), "payload": payload, "at": clock(), "timeout_ms": 30000})
	if retried.Status != "ok" || !retried.SessionsClosed {
		t.Fatal("cross-writer retry failed")
	}
	tr.closeGo() // The independent Go writer must release its own transport first.
	w.close(tr)
	records := receive()
	if len(records) != 2 {
		t.Fatal("cross-writer FACT inventory differs")
	}
	_, version, err := store.Load(f.ctx, st.OrderId)
	if err != nil || version != 1 {
		t.Fatal("cross-writer order committed more than once")
	}
	c, err := store.RecoveryCase(f.ctx, env.EventId)
	if err != nil || c.Checkpoint != 2 || c.Status != "investigating" {
		t.Fatal("cross-writer checkpoint differs")
	}
	for _, query := range []string{"SELECT count(*) FROM order_fills", "SELECT count(*) FROM order_executions", "SELECT count(*) FROM execution_recovery_targets", "SELECT count(*) FROM outbox WHERE event_type='order.order.execution_recovered'"} {
		var n int
		if f.pool.QueryRow(f.ctx, query).Scan(&n) != nil || n != 2 {
			t.Fatal("cross-writer durable inventory duplicated or incomplete")
		}
	}
	if v.queries.Load() != 2 || v.writes.Load() != 0 {
		t.Fatal("cross-writer retry re-queried or traded")
	}
}
