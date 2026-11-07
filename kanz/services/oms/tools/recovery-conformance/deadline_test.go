package recoveryconformance

import (
	"fmt"
	"github.com/eighred/kanz/internal/dec"
	"math/big"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/fillfact"
	"github.com/eighred/kanz/internal/outbox"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/oms/internal/order"
	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"
)

func deadlineFixture(t *testing.T) (*fixture, *orderpb.OrderState, *venue, *envelopepb.Envelope, []byte) {
	t.Helper()
	f := newFixture(t)
	st := initial()
	if order.NewPostgres(f.pool).Create(f.ctx, st, nil) != nil {
		t.Fatal("seed canonical order failed")
	}
	v := &venue{tenant: f.tenant, orderID: st.OrderId, description: &venuepb.DescribeResponse{Mic: "MOCK", Account: "fixture-account", AccountVerified: true, ExchangeAccountId: "fixture-exchange-account", SupportedOrderTypes: []orderpb.OrderType{orderpb.OrderType_ORDER_TYPE_LIMIT}}, history: history(st)}
	env := &envelopepb.Envelope{EventId: "observation", TenantId: f.tenant, EventType: execution.SubjectStateHealed, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, CorrelationId: "fixture-correlation"}
	return f, st, v, env, wire(t, &orderpb.StateHealed{OrderId: st.OrderId, Venue: st.Venue, State: st})
}
func deadlineConfig(f *fixture, tr *transport, budget int) map[string]any {
	return map[string]any{"endpoint": "https://" + tr.endpoint, "workload_socket": tr.socket, "expected_peer": serverID, "tenant": f.tenant, "mic": "MOCK", "account": "fixture-account", "portfolio": "fixture-portfolio", "schema": f.schema, "timeout_ms": budget}
}
func deadlineObservation(t *testing.T, w *worker, env *envelopepb.Envelope, payload []byte, budget int) response {
	t.Helper()
	return w.call(map[string]any{"method": "Observation", "envelope": wire(t, env), "payload": payload, "at": clock(), "timeout_ms": budget})
}
func assertDeadlineResponse(t *testing.T, r response, code string) {
	t.Helper()
	if r.Status != "error" || r.Error != code || !r.SessionsClosed {
		t.Fatalf("deadline outcome status=%s error=%s sessions_closed=%t", r.Status, r.Error, r.SessionsClosed)
	}
}
func assertNoOwnedSessions(t *testing.T, f *fixture, w *worker) {
	t.Helper()
	var n int
	if f.pool.QueryRow(f.ctx, "SELECT count(*) FROM pg_stat_activity WHERE application_name=$1", fmt.Sprintf("oms-recovery-%d", w.cmd.Process.Pid)).Scan(&n) != nil || n != 0 {
		t.Fatal("recovery SQL session survived response while process alive")
	}
	select {
	case <-w.done:
		t.Fatal("process exited before independent cleanup observation")
	default:
	}
}
func assertNoFinancialTail(t *testing.T, f *fixture, st *orderpb.OrderState) {
	t.Helper()
	got, version, err := order.NewPostgres(f.pool).Load(f.ctx, st.OrderId)
	if err != nil || version != 0 || !proto.Equal(got, st) {
		t.Fatal("precommit deadline changed order")
	}
	for _, table := range []string{"order_fills", "order_executions", "execution_fee_revisions", "execution_recovery_targets", "execution_recovery_acks", "outbox"} {
		var n int
		if f.pool.QueryRow(f.ctx, "SELECT count(*) FROM "+table).Scan(&n) != nil || n != 0 {
			t.Fatal("precommit deadline changed financial tail")
		}
	}
}
func assertCommittedDeadlineState(t *testing.T, f *fixture, st *orderpb.OrderState) order.RecoveryCase {
	t.Helper()
	got, version, err := order.NewPostgres(f.pool).Load(f.ctx, st.OrderId)
	filled, filledOK := dec.FromProtoChecked(got.GetFilledQuantity())
	leaves, leavesOK := dec.FromProtoChecked(got.GetLeavesQuantity())
	average, averageOK := dec.FromProtoChecked(got.GetAverageFillPrice())
	if err != nil || version != 1 || got.GetFilledQuantity() == nil || got.GetLeavesQuantity() == nil || got.GetAverageFillPrice() == nil || !filledOK || !leavesOK || !averageOK || filled.Cmp(big.NewRat(2, 1)) != 0 || leaves.Cmp(big.NewRat(2, 1)) != 0 || average.Cmp(big.NewRat(201, 2)) != 0 || got.GetStatus() != st.Status {
		t.Fatal("deadline fixture financial commit differs")
	}
	c, err := order.NewPostgres(f.pool).RecoveryCase(f.ctx, "observation")
	if err != nil || c.Checkpoint != 2 || c.Status != "investigating" {
		t.Fatal("deadline fixture recovery checkpoint differs")
	}
	for _, table := range []string{"order_fills", "order_executions", "execution_recovery_targets"} {
		var n int
		if f.pool.QueryRow(f.ctx, "SELECT count(*) FROM "+table).Scan(&n) != nil || n != 2 {
			t.Fatal("deadline fixture execution inventory differs")
		}
	}
	return c
}

// Both technical phases individually fit the shorter work budget. Resetting the
// budget between connection and Query incorrectly permits that negative case.
func TestRustRecoveryCumulativeCommandDeadline(t *testing.T) {
	for _, budget := range []int{1000, 3000} {
		name := "cumulative-refusal"
		if budget == 3000 {
			name = "same-phases-positive"
		}
		t.Run(name, func(t *testing.T) {
			f, st, v, env, payload := deadlineFixture(t)
			v.queryDelay = 600 * time.Millisecond
			tr := startTransport(t, v)
			config := deadlineConfig(f, tr, 10000)
			config["session_delay_ms"] = 600
			w, r := startWorkerWithin(t, config, 180*time.Second, f.postgresURL)
			if r.Status != "connected" {
				t.Fatal("deadline fixture failed startup")
			}
			receive := f.recovered(t, 2)
			r = deadlineObservation(t, w, env, payload, budget)
			if v.queries.Load() != 1 {
				t.Fatal("cumulative deadline did not reach real Query phase")
			}
			assertNoOwnedSessions(t, f, w)
			if budget == 1000 {
				assertDeadlineResponse(t, r, "Deadline")
				assertNoFinancialTail(t, f, st)
				// Same process retries the persisted observation with the longer work budget.
				r = deadlineObservation(t, w, env, payload, 3000)
			}
			if r.Status != "ok" || !r.SessionsClosed {
				t.Fatalf("positive work budget refused: status=%s error=%s", r.Status, r.Error)
			}
			assertCommittedDeadlineState(t, f, st)
			if len(receive()) != 2 {
				t.Fatal("complete FACT inventory missing")
			}
			w.close(tr)
		})
	}
}

func TestRustRecoveryCumulativeStartupDeadline(t *testing.T) {
	for _, budget := range []int{1000, 3000} {
		name := "cumulative-refusal"
		if budget == 3000 {
			name = "same-phases-positive"
		}
		t.Run(name, func(t *testing.T) {
			f, st, v, _, _ := deadlineFixture(t)
			v.describeDelay = 600 * time.Millisecond
			tr := startTransport(t, v)
			tr.source.SetInitialDelay(t, 600*time.Millisecond)
			w, r := startWorkerWithin(t, deadlineConfig(f, tr, budget), 180*time.Second, f.postgresURL)
			if tr.source.Started() < 1 || v.describes.Load() != 1 {
				t.Fatal("startup deadline did not reach both actual transport phases")
			}
			if budget == 3000 {
				if r.Status != "connected" {
					t.Fatalf("startup positive refused: status=%s error=%s", r.Status, r.Error)
				}
				w.close(tr)
			} else {
				assertDeadlineResponse(t, r, "Deadline")
				until := time.Now().Add(3 * time.Second)
				for (tr.source.Active() != 0 || v.active.Load() != 0) && time.Now().Before(until) {
					time.Sleep(10 * time.Millisecond)
				}
				if tr.source.Active() != 0 || v.active.Load() != 0 {
					t.Fatal("failed startup retained transport before process exit")
				}
				select {
				case <-w.done:
					t.Fatal("failed startup exited before cleanup observation")
				default:
				}
				if w.input.Close() != nil {
					t.Fatal("failed startup EOF failed")
				}
				select {
				case err := <-w.done:
					if err != nil {
						t.Fatal("failed startup process join failed")
					}
				case <-time.After(5 * time.Second):
					t.Fatal("failed startup process did not join")
				}
				w.closed = true
				w.cancel()
			}
			assertNoFinancialTail(t, f, st)
		})
	}
}

// JetStream independently accepts the first two frames before the second ACK
// return is held. SQL distinguishes the marked prefix from the uncertain suffix.
func TestRustRecoveryDeadlineAfterBrokerAcknowledgement(t *testing.T) {
	f, st, v, env, payload := deadlineFixture(t)
	tr := startTransport(t, v)
	receive := f.recovered(t, 3)
	nc, err := nats.Connect(brokerURL(t))
	if err != nil {
		t.Fatal("broker observer unavailable")
	}
	defer nc.Close()
	observed := make(chan *nats.Msg, 8)
	sub, err := nc.ChanSubscribe(fillfact.SubjectRecovered, observed)
	if err != nil {
		t.Fatal("broker observation subscribe failed")
	}
	defer func() { _ = sub.Unsubscribe() }()
	if nc.FlushTimeout(5*time.Second) != nil {
		t.Fatal("broker observer not armed")
	}
	config := deadlineConfig(f, tr, 10000)
	config["hold_after_ack"] = 2
	config["ack_hold_ms"] = 1500
	w, r := startWorkerWithin(t, config, 180*time.Second, f.postgresURL)
	if r.Status != "connected" {
		t.Fatal("held ACK fixture failed startup")
	}
	r = deadlineObservation(t, w, env, payload, 500)
	assertDeadlineResponse(t, r, "PublicationUnconfirmed")
	assertNoOwnedSessions(t, f, w)
	before := assertCommittedDeadlineState(t, f, st)
	var marked, pending int
	if f.pool.QueryRow(f.ctx, "SELECT count(*) FILTER(WHERE published_at IS NOT NULL),count(*) FILTER(WHERE published_at IS NULL) FROM outbox WHERE event_type=$1", fillfact.SubjectRecovered).Scan(&marked, &pending) != nil || marked != 1 || pending != 1 {
		t.Fatal("held second ACK did not preserve marked prefix and uncertain suffix")
	}
	var records []*orderpb.ExecutionRecovered
	var events []*envelopepb.Envelope
	for range 2 {
		select {
		case m := <-observed:
			e, p, eerr := bus.Unframe(m.Data)
			var fact orderpb.ExecutionRecovered
			if eerr != nil || bus.Validate(e) != nil || e.TenantId != f.tenant || proto.Unmarshal(p, &fact) != nil {
				t.Fatal("broker prefix frame differs")
			}
			events = append(events, e)
			records = append(records, &fact)
		case <-time.After(5 * time.Second):
			t.Fatal("actual broker prefix not observed")
		}
	}

	for i, marked := range []bool{true, false} {
		var raw []byte
		if f.pool.QueryRow(f.ctx, "SELECT payload FROM outbox WHERE event_type=$1 AND (published_at IS NOT NULL)=$2", fillfact.SubjectRecovered, marked).Scan(&raw) != nil {
			t.Fatal("durable prefix payload unavailable")
		}
		var persisted orderpb.ExecutionRecovered
		if proto.Unmarshal(raw, &persisted) != nil || !proto.Equal(&persisted, records[i]) {
			t.Fatal("SQL mark does not bind the observed broker prefix")
		}
	}
	if events[0].ProducerSequence >= events[1].ProducerSequence {
		t.Fatal("prefix producer sequence not increasing")
	}
	r = deadlineObservation(t, w, env, payload, 3000)
	if r.Status != "ok" || !r.SessionsClosed {
		t.Fatalf("uncertain suffix retry refused: status=%s error=%s", r.Status, r.Error)
	}
	after := assertCommittedDeadlineState(t, f, st)
	if after.Version != before.Version || v.queries.Load() != 1 || v.writes.Load() != 0 {
		t.Fatal("publication retry repeated financial transition or venue query")
	}
	if f.pool.QueryRow(f.ctx, "SELECT count(*) FROM outbox WHERE published_at IS NULL").Scan(&pending) != nil || pending != 0 {
		t.Fatal("retry left durable publication suffix")
	}
	// Both owners create a fresh envelope identity for each publication attempt.
	// The uncertain suffix preserves payload/lineage, not the earlier envelope ID.
	select {
	case m := <-observed:
		e, p, eerr := bus.Unframe(m.Data)
		var fact orderpb.ExecutionRecovered
		if eerr != nil || bus.Validate(e) != nil || e.EventId == "" || e.EventId == events[1].EventId || e.EventId == events[0].EventId || e.ProducerSequence <= events[1].ProducerSequence || e.TenantId != f.tenant || e.CausationId != "observation" || e.CorrelationId != "fixture-correlation" || proto.Unmarshal(p, &fact) != nil || !proto.Equal(&fact, records[1]) {
			t.Fatal("retry did not preserve only uncertain suffix identity")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("uncertain suffix publication not observed")
	}
	accepted := receive()
	if len(accepted) != 3 || !proto.Equal(accepted[0], records[0]) || !proto.Equal(accepted[1], records[1]) || !proto.Equal(accepted[2], records[1]) {
		t.Fatal("JetStream prefix/retry inventory differs")
	}
	if proto.Equal(records[0], records[1]) {
		t.Fatal("prefix fixture does not distinguish executions")
	}
	if nc.FlushTimeout(5*time.Second) != nil {
		t.Fatal("post-retry broker barrier failed")
	}
	select {
	case <-observed:
		t.Fatal("marked prefix or extra recovered FACT was republished")
	default:
	}
	assertNoOwnedSessions(t, f, w)
	w.close(tr)
}

func TestRustRecoveryRelayLockDeadline(t *testing.T) {
	f, st, v, env, payload := deadlineFixture(t)
	tr := startTransport(t, v)
	receive := f.recovered(t, 2)
	w := f.start(t, tr)
	unlock, won, err := outbox.NewPostgres(f.pool, outbox.ServiceOMS).LockKey(f.ctx, st.OrderId, true)
	if err != nil || !won {
		t.Fatal("canonical relay lock unavailable")
	}
	held := true
	defer func() {
		if held {
			unlock()
		}
	}()
	r := deadlineObservation(t, w, env, payload, 1000)
	assertDeadlineResponse(t, r, "PublicationUnconfirmed")
	assertNoOwnedSessions(t, f, w)
	before := assertCommittedDeadlineState(t, f, st)
	var published int
	if f.pool.QueryRow(f.ctx, "SELECT count(*) FROM outbox WHERE published_at IS NOT NULL").Scan(&published) != nil || published != 0 {
		t.Fatal("contended relay published through canonical lock")
	}
	unlock()
	held = false
	r = deadlineObservation(t, w, env, payload, 3000)
	if r.Status != "ok" || !r.SessionsClosed {
		t.Fatalf("relay lock release did not restore progress: status=%s error=%s", r.Status, r.Error)
	}
	after := assertCommittedDeadlineState(t, f, st)
	if after.Version != before.Version || v.queries.Load() != 1 {
		t.Fatal("relay retry repeated financial transition")
	}
	if len(receive()) != 2 {
		t.Fatal("released relay lost FACTs")
	}
	assertNoOwnedSessions(t, f, w)
	w.close(tr)
}
