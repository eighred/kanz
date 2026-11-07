package recoveryconformance

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/fillfact"
	"github.com/eighred/kanz/internal/platform/halt"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/oms/internal/order"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var instant = time.Date(2026, 1, 1, 0, 0, 0, 123000, time.UTC)

func wire(t *testing.T, m proto.Message) []byte {
	t.Helper()
	b, e := proto.MarshalOptions{Deterministic: true}.Marshal(m)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func clock() map[string]any {
	return map[string]any{"seconds": instant.Unix(), "nanos": instant.Nanosecond()}
}
func lineage(ctx context.Context, env *envelopepb.Envelope) context.Context {
	ctx = bus.WithTenantID(ctx, env.TenantId)
	ctx = bus.WithCorrelationID(ctx, env.CorrelationId)
	ctx = bus.WithCausationID(ctx, env.EventId)
	return bus.WithTraceContext(ctx, env.TraceContext)
}
func (f *fixture) service(t *testing.T, tr *transport) *order.Service {
	t.Helper()
	url := brokerURL(t)
	if url == "" {
		t.Skip("real JetStream unavailable")
	}
	client, err := bus.DialNATS(f.ctx, bus.NATSConfig{URL: url, Name: "recovery-conformance"})
	if err != nil {
		t.Fatal("fixture broker connection failed")
	}
	t.Cleanup(func() {
		if client.Close() != nil {
			t.Error("broker cleanup failed")
		}
	})
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "oms", ProducerVersion: "go-oracle", Tenant: f.tenant})
	if err != nil {
		t.Fatal(err)
	}
	adapter := execution.NewGRPCVenue("MOCK", "fixture-account", tr.goConn, f.tenant)
	bindings, err := execution.ParseBindings(f.tenant + "/fixture-portfolio@MOCK=fixture-account")
	if err != nil {
		t.Fatal(err)
	}
	svc, err := order.NewService(f.tenant, order.NewPostgres(f.pool), order.NewEmitter(producer), nil, execution.NewRouter([]execution.Venue{adapter}), nil, nil, order.WithAccountBindings(bindings, true, nil), order.WithHaltGate(halt.OpenGate(nil)))
	if err != nil {
		t.Fatal(err)
	}
	return svc
}
func initial() *orderpb.OrderState {
	st := &orderpb.OrderState{OrderId: "recovery-order", PortfolioId: "fixture-portfolio", Venue: "MOCK", VenueAccountId: "fixture-account", InstrumentId: "fixture-instrument", Side: orderpb.Side_SIDE_BUY, Status: orderpb.OrderStatus_ORDER_STATUS_CANCELLED, OrderedQuantity: &commonpb.Decimal{Coefficient: 4}}
	st.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 33, protowire.VarintType), 77))
	st.OrderedQuantity.ProtoReflect().SetUnknown(protowire.AppendBytes(protowire.AppendTag(nil, 100, protowire.BytesType), []byte("synthetic-quantity-sidecar")))
	return st
}
func history(st *orderpb.OrderState) *venuepb.QueryOrderResponse {
	fills := make([]*orderpb.Fill, 2)
	for i := range fills {
		fills[i] = &orderpb.Fill{OrderId: st.OrderId, FillId: []string{"execution-a", "execution-b"}[i], VenueExecutionId: []string{"remote-a", "remote-b"}[i], Venue: st.Venue, VenueAccountId: st.VenueAccountId, InstrumentId: st.InstrumentId, Side: st.Side, Quantity: &commonpb.Decimal{Coefficient: 1}, Price: &commonpb.Decimal{Coefficient: int64(100 + i)}, Fee: &commonpb.Money{Amount: &commonpb.Decimal{Coefficient: 1, Exponent: -1}, CurrencyCode: "USD"}, ExecutedAt: timestamppb.New(instant)}
	}
	fills[0].ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 1000, protowire.VarintType), 19))
	fills[0].Fee.Amount.ProtoReflect().SetUnknown(protowire.AppendBytes(protowire.AppendTag(nil, 100, protowire.BytesType), []byte("synthetic-fee-sidecar")))
	return &venuepb.QueryOrderResponse{State: venuepb.OrderViewState_ORDER_VIEW_STATE_CANCELLED, Fills: fills, ExecutedQuantity: &commonpb.Decimal{Coefficient: 2}}
}
func TestRustRecoveryObservation(t *testing.T) {
	for _, name := range []string{"complete-terminal-history", "complete-raw-observation-unknown", "complete-large-history-small-facts", "complete-single-round-weighted-average", "complete-identical-redelivery-history", "missing-order-blocked", "conflicting-observation-blocked", "incomplete-total-blocked", "missing-total-blocked", "unknown-view-blocked", "duplicate-execution-conflict-blocked"} {
		t.Run(name, func(t *testing.T) {
			count, filled, leaves := 2, int64(2), int64(2)
			average := &commonpb.Decimal{Coefficient: 1005, Exponent: -1}
			if name == "complete-single-round-weighted-average" {
				count, filled, leaves = 3, 6, 4
				average = &commonpb.Decimal{Coefficient: 133333333, Exponent: -8}
			}
			var states []*orderpb.OrderState
			var cases []order.RecoveryCase
			for _, owner := range []string{"go", "rust"} {
				f := newFixture(t)
				st := initial()
				if name == "complete-single-round-weighted-average" {
					st.OrderedQuantity.Coefficient = 10
				}
				store := order.NewPostgres(f.pool)
				if name != "missing-order-blocked" {
					if err := store.Create(f.ctx, st, nil); err != nil {
						t.Fatal("seed canonical order failed")
					}
				}
				v := &venue{tenant: f.tenant, orderID: st.OrderId, description: &venuepb.DescribeResponse{Mic: "MOCK", Account: "fixture-account", AccountVerified: true, ExchangeAccountId: "fixture-exchange-account", SupportedOrderTypes: []orderpb.OrderType{orderpb.OrderType_ORDER_TYPE_LIMIT}}, history: history(st)}
				if name == "incomplete-total-blocked" {
					v.history.ExecutedQuantity = &commonpb.Decimal{Coefficient: 3}
				}
				switch name {
				case "complete-large-history-small-facts":
					for _, fill := range v.history.Fills {
						unknown := append([]byte(nil), fill.ProtoReflect().GetUnknown()...)
						unknown = protowire.AppendBytes(protowire.AppendTag(unknown, 1001, protowire.BytesType), []byte(strings.Repeat("x", 600<<10)))
						fill.ProtoReflect().SetUnknown(unknown)
						if proto.Size(fill) >= 1<<20 {
							t.Fatal("large-history fixture exceeds individual record domain")
						}
					}
					if n := proto.Size(v.history); n <= 1<<20 || n >= 4<<20 {
						t.Fatal("large-history fixture does not discriminate recovery receive budget")
					}
				case "complete-single-round-weighted-average":
					v.history.Fills[0].Price.Coefficient = 1
					v.history.Fills[1].Price.Coefficient = 2
					v.history.Fills[1].Quantity.Coefficient = 2
					third := proto.Clone(v.history.Fills[0]).(*orderpb.Fill)
					third.FillId = "execution-c"
					third.VenueExecutionId = "remote-c"
					third.Quantity.Coefficient = 3
					v.history.Fills = append(v.history.Fills, third)
					v.history.ExecutedQuantity = &commonpb.Decimal{Coefficient: 6}
				case "complete-identical-redelivery-history":
					v.history.Fills = append(v.history.Fills, proto.Clone(v.history.Fills[0]).(*orderpb.Fill))
				case "missing-total-blocked":
					v.history.ExecutedQuantity = nil
				case "unknown-view-blocked":
					v.history.State = venuepb.OrderViewState(999)
				case "duplicate-execution-conflict-blocked":
					v.history.Fills[1].VenueExecutionId = v.history.Fills[0].VenueExecutionId
				}
				tr := startTransport(t, v)
				env := &envelopepb.Envelope{EventId: "observation", TenantId: f.tenant, EventType: execution.SubjectStateHealed, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, CorrelationId: "fixture-correlation"}
				observation := &orderpb.StateHealed{OrderId: st.OrderId, Venue: st.Venue, State: proto.Clone(st).(*orderpb.OrderState), Reason: "synthetic complete-history investigation", DetectedAt: timestamppb.New(instant)}
				if name == "complete-raw-observation-unknown" {
					// Same-number wrong-wire OrderId plus an unknown group;
					// raw evidence hashing must not silently discard either.
					unknown := protowire.AppendVarint(protowire.AppendTag(nil, 1, protowire.VarintType), 17)
					unknown = protowire.AppendTag(unknown, 1001, protowire.StartGroupType)
					unknown = protowire.AppendVarint(protowire.AppendTag(unknown, 1, protowire.VarintType), 19)
					unknown = protowire.AppendTag(unknown, 1001, protowire.EndGroupType)
					observation.ProtoReflect().SetUnknown(unknown)
					env.ProtoReflect().SetUnknown(protowire.AppendBytes(protowire.AppendTag(nil, 2001, protowire.BytesType), []byte("synthetic-envelope-sidecar")))
				}
				if name == "conflicting-observation-blocked" {
					observation.Venue = "OTHER"
				}
				var receive func() []*orderpb.ExecutionRecovered
				if strings.HasPrefix(name, "complete-") {
					receive = f.recovered(t, count)
				}
				payload := wire(t, observation)
				calledAt := time.Now()
				if owner == "go" {
					if err := f.service(t, tr).HandleRecovery(lineage(f.ctx, env), env, payload); err != nil {
						t.Fatal("canonical Go recovery failed")
					}
				} else {
					w := f.start(t, tr)
					r := w.call(map[string]any{"method": "Observation", "envelope": wire(t, env), "payload": payload, "at": clock(), "timeout_ms": 30000})
					if r.Status != "ok" || !r.SessionsClosed {
						t.Fatalf("Rust recovery handler: status=%s error=%s sessions_closed=%t", r.Status, r.Error, r.SessionsClosed)
					}
					w.close(tr)
				}
				c, err := store.RecoveryCase(f.ctx, env.EventId)
				if err != nil {
					t.Fatal("durable recovery case missing")
				}
				expected := "blocked"
				if strings.HasPrefix(name, "complete-") {
					expected = "investigating"
				}
				if c.Status != expected {
					t.Fatal("handler success disguised incorrect durable lifecycle")
				}
				cases = append(cases, c)
				if !f.invoke(t, owner, tr, "Observation", env, payload) {
					t.Fatal("exact observation redelivery refused")
				}
				changed := proto.Clone(observation).(*orderpb.StateHealed)
				changed.Reason = "changed evidence"
				if f.invoke(t, owner, tr, "Observation", env, wire(t, changed)) {
					t.Fatal("changed observation reused existing evidence identity")
				}
				stable, err := store.RecoveryCase(f.ctx, c.ID)
				if err != nil || stable.Version != c.Version || stable.Digest != c.Digest {
					t.Fatal("observation replay mutated frozen evidence")
				}

				if v.writes.Load() != 0 {
					t.Fatal("recovery attempted a trading command")
				}
				var targets, claims int
				if f.pool.QueryRow(f.ctx, "SELECT count(*) FROM execution_recovery_targets").Scan(&targets) != nil || f.pool.QueryRow(f.ctx, "SELECT count(*) FROM order_fills").Scan(&claims) != nil {
					t.Fatal("durable evidence read failed")
				}
				if strings.HasPrefix(name, "complete-") {
					if targets != count || claims != count || c.Checkpoint != count {
						t.Fatal("complete history was not atomically projected")
					}
					got, _, err := store.Load(f.ctx, st.OrderId)
					if err != nil {
						t.Fatal("recovered order missing")
					}
					// Independent arithmetic: ordinary(100+101)/2=100.5. Weighted
					// (1*1+2*2+3*1)/6=1.33333333 at1e-8; rounded incremental
					// averaging yields1.33333334, which must not be accepted.
					if got.Status != orderpb.OrderStatus_ORDER_STATUS_CANCELLED || dec.Cmp(got.FilledQuantity, &commonpb.Decimal{Coefficient: filled}) != 0 || dec.Cmp(got.AverageFillPrice, average) != 0 || dec.Cmp(got.LeavesQuantity, &commonpb.Decimal{Coefficient: leaves}) != 0 {
						t.Fatal("terminal recovery arithmetic or status differs")
					}
					if got.AsOf == nil || got.AsOf.CheckValid() != nil {
						t.Fatal("recovery timestamp missing")
					}
					if owner == "rust" {
						if !got.AsOf.AsTime().Equal(instant) {
							t.Fatal("Rust recovery timestamp differs")
						}
					} else if got.AsOf.AsTime().Before(calledAt) || got.AsOf.AsTime().After(time.Now()) {
						t.Fatal("Go recovery timestamp outside handler interval")
					}
					records := receive()
					seen := map[string]bool{}
					for _, record := range records {
						if seen[record.Fill.FillId] || !proto.Equal(record.State, got) || record.Fill.Recovery.CaseId != c.ID {
							t.Fatal("recovery FACT state/provenance differs")
						}
						candidate := proto.Clone(record.Fill).(*orderpb.Fill)
						candidate.Recovery = nil
						var original *orderpb.Fill
						for _, fill := range v.history.Fills {
							if fill.FillId == candidate.FillId {
								original = fill
							}
						}
						if original == nil || !proto.Equal(candidate, original) {
							t.Fatal("recovered FACT lost execution or unknown fields")
						}
						seen[record.Fill.FillId] = true
					}
					if count == 3 && !seen["execution-c"] {
						t.Fatal("third execution missing")
					}
					if !seen["execution-a"] || !seen["execution-b"] {
						t.Fatal("recovery FACT execution inventory differs")
					}
					f.acknowledge(t, owner, tr, records)
					got.AsOf = nil
					states = append(states, got)
				} else if targets != 0 || claims != 0 || c.Checkpoint != 0 {
					t.Fatal("blocked history wrote financial claims")
				}
			}
			if len(cases) != 2 || cases[0].Status != cases[1].Status || cases[0].Checkpoint != cases[1].Checkpoint || cases[0].Version != cases[1].Version || cases[0].Reason != cases[1].Reason || cases[0].Digest != cases[1].Digest || cases[0].MappingVersion != cases[1].MappingVersion {
				t.Fatal("Go/Rust durable recovery lifecycle differs")
			}
			if len(states) == 2 && !proto.Equal(states[0], states[1]) {
				t.Fatal("Go/Rust complete order states differ")
			}
		})
	}
}

func (f *fixture) invoke(t *testing.T, owner string, tr *transport, method string, env *envelopepb.Envelope, payload []byte) bool {
	t.Helper()
	if owner == "rust" {
		w := f.start(t, tr)
		result := w.call(map[string]any{"method": method, "envelope": wire(t, env), "payload": payload, "at": clock(), "timeout_ms": 30000})
		if !result.SessionsClosed {
			t.Fatal("recovery SQL owners not closed")
		}
		w.close(tr)
		if result.Status != "ok" {
			t.Logf("recovery owner=rust status=%s error=%s sessions_closed=%t", result.Status, result.Error, result.SessionsClosed)
		}
		return result.Status == "ok"
	}
	svc := f.service(t, tr)
	var err error
	switch method {
	case "Observation":
		err = svc.HandleRecovery(lineage(f.ctx, env), env, payload)
	case "Ack":
		err = svc.HandleRecoveryAck(lineage(f.ctx, env), env, payload)
	case "Parked":
		err = svc.HandleRecoveryParked(lineage(f.ctx, env), env, payload)
	default:
		t.Fatal("unsupported oracle handler")
	}
	if err != nil {
		t.Logf("recovery owner=go category=%s", goErrorCategory(err))
	}
	return err == nil
}
func (f *fixture) acknowledge(t *testing.T, owner string, tr *transport, records []*orderpb.ExecutionRecovered) {
	t.Helper()
	store := order.NewPostgres(f.pool)
	parked := &envelopepb.Envelope{EventId: "parked", TenantId: f.tenant, EventType: fillfact.SubjectRecovered, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT}
	for range 2 {
		if !f.invoke(t, owner, tr, "Parked", parked, wire(t, records[0])) {
			t.Fatal("bound parked delivery refused")
		}
	}
	c, err := store.RecoveryCase(f.ctx, "observation")
	if err != nil || c.Status != "blocked" {
		t.Fatal("parked delivery did not persist unresolved lifecycle")
	}
	for index, record := range records {
		for _, subject := range []string{fillfact.RecoveryPositionApplied, fillfact.RecoveryLedgerApplied} {
			ack, err := fillfact.RecoveryAck(bus.WithTenantID(f.ctx, f.tenant), subject, record.Fill, instant)
			if err != nil {
				t.Fatal(err)
			}
			env := &envelopepb.Envelope{EventId: "ack", TenantId: f.tenant, EventType: subject, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT}
			if index == 0 && subject == fillfact.RecoveryPositionApplied {
				var wrong orderpb.ExecutionRecoveryApplied
				if proto.Unmarshal(ack.Payload, &wrong) != nil {
					t.Fatal("canonical ACK invalid")
				}
				wrong.ExecutionDigest = "incorrect"
				if f.invoke(t, owner, tr, "Ack", env, wire(t, &wrong)) {
					t.Fatal("unbound ACK accepted")
				}
				unchanged, err := store.RecoveryCase(f.ctx, c.ID)
				if err != nil || unchanged.Version != c.Version {
					t.Fatal("invalid ACK mutated lifecycle")
				}
			}
			for range 2 {
				if !f.invoke(t, owner, tr, "Ack", env, ack.Payload) {
					t.Fatal("bound ACK or duplicate refused")
				}
			}
			current, err := store.RecoveryCase(f.ctx, c.ID)
			if err != nil {
				t.Fatal(err)
			}
			final := index == len(records)-1 && subject == fillfact.RecoveryLedgerApplied
			if final && current.Status != "corrected" || !final && current.Status != "blocked" {
				t.Fatal("ACK closure occurred before every book/execution")
			}
		}
	}
	var count int
	if f.pool.QueryRow(f.ctx, "SELECT count(*) FROM execution_recovery_acks").Scan(&count) != nil || count != len(records)*2 {
		t.Fatal("ACK dedup/cardinality differs")
	}
	corrected, err := store.RecoveryCase(f.ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !f.invoke(t, owner, tr, "Parked", parked, wire(t, records[0])) {
		t.Fatal("late bound parked event refused")
	}
	after, err := store.RecoveryCase(f.ctx, c.ID)
	if err != nil || after.Status != "corrected" || after.Version != corrected.Version {
		t.Fatal("late parked event reopened closed recovery")
	}
	if tr.venue.queries.Load() != 1 || tr.venue.writes.Load() != 0 {
		t.Fatal("restart/ACK/parked re-queried or traded")
	}
}
