package recoveryconformance

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/execution"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"github.com/eighred/kanz/services/oms/internal/order"
	"google.golang.org/protobuf/proto"
)

// The crash point is established by an actual server-observed QueryOrder, not
// a production fault flag or an elapsed sleep. Mapping must already be durable;
// no history reply has been supplied and no financial write is permissible yet.
func TestRustRecoveryCrashDuringHistoryQuery(t *testing.T) {
	f := newFixture(t)
	st := initial()
	store := order.NewPostgres(f.pool)
	if store.Create(f.ctx, st, nil) != nil {
		t.Fatal("seed canonical order failed")
	}
	v := &venue{tenant: f.tenant, orderID: st.OrderId, description: &venuepb.DescribeResponse{Mic: "MOCK", Account: "fixture-account", AccountVerified: true, ExchangeAccountId: "fixture-exchange-account", SupportedOrderTypes: []orderpb.OrderType{orderpb.OrderType_ORDER_TYPE_LIMIT}}, history: history(st), queryWait: true}
	tr := startTransport(t, v)
	env := &envelopepb.Envelope{EventId: "observation", TenantId: f.tenant, EventType: execution.SubjectStateHealed, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, CorrelationId: "fixture-correlation"}
	payload := wire(t, &orderpb.StateHealed{OrderId: st.OrderId, Venue: st.Venue, State: st})
	w := f.start(t, tr)
	raw, err := json.Marshal(map[string]any{"method": "Observation", "envelope": wire(t, env), "payload": payload, "at": clock(), "timeout_ms": 30000})
	if err != nil {
		t.Fatal(err)
	}
	written := make(chan error, 1)
	go func() { _, err := w.input.Write(append(raw, '\n')); written <- err }()
	select {
	case err := <-written:
		if err != nil {
			t.Fatal("crash fixture request failed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("crash fixture write timed out")
	}
	deadline := time.Now().Add(10 * time.Second)
	for v.queries.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if v.queries.Load() != 1 {
		t.Fatal("crash point did not reach actual history server")
	}
	if w.cmd.Process.Kill() != nil {
		t.Fatal("owned fixture crash failed")
	}
	_ = w.input.Close()
	select {
	case err := <-w.done:
		if err == nil {
			t.Fatal("crashed worker reported ordinary success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("crashed worker not joined")
	}
	w.closed = true
	w.cancel()
	c, err := store.RecoveryCase(f.ctx, "observation")
	if err != nil || c.Status != "observed" || c.Checkpoint != 0 {
		t.Fatal("pre-history crash lost observation or advanced recovery")
	}
	var mappings int
	if f.pool.QueryRow(f.ctx, "SELECT count(*) FROM execution_account_mappings").Scan(&mappings) != nil || mappings != 1 {
		t.Fatal("verified account mapping was not durable before query")
	}
	for _, query := range []string{"SELECT count(*) FROM order_fills", "SELECT count(*) FROM order_executions", "SELECT count(*) FROM execution_recovery_targets", "SELECT count(*) FROM execution_recovery_acks", "SELECT count(*) FROM outbox"} {
		var n int
		if f.pool.QueryRow(f.ctx, query).Scan(&n) != nil || n != 0 {
			t.Fatal("pre-history crash committed financial evidence")
		}
	}
	got, version, err := store.Load(f.ctx, st.OrderId)
	if err != nil || version != 0 || !proto.Equal(got, st) {
		t.Fatal("pre-history crash changed order")
	}
	v.mu.Lock()
	v.queryWait = false
	v.mu.Unlock()
	receive := f.recovered(t, 2)
	if !f.invoke(t, "rust", tr, "Observation", env, payload) {
		t.Fatal("restart did not resume observed recovery")
	}
	records := receive()
	if len(records) != 2 || v.queries.Load() != 2 || v.writes.Load() != 0 {
		t.Fatal("restart query/FACT inventory differs")
	}
	c, err = store.RecoveryCase(f.ctx, "observation")
	if err != nil || c.Checkpoint != 2 || c.Status != "investigating" {
		t.Fatal("restart failed durable recovery")
	}
	if !f.invoke(t, "rust", tr, "Observation", env, payload) || v.queries.Load() != 2 {
		t.Fatal("committed restart re-queried venue history")
	}
}
