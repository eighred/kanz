package storeconformance

import (
	"bytes"
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/bustest"
	"github.com/eighred/kanz/internal/fillfact"
	"github.com/eighred/kanz/internal/outbox"
	"github.com/eighred/kanz/internal/platform/subject"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/oms/internal/position"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// The oracle calls the actual Go Projector and PostgreSQL store. This publisher
// observes its events; actual Rust publication is separately read from JetStream.
type projectorCapture struct {
	records []outbox.Record
	fail    bool
}

func (p *projectorCapture) Publish(ctx context.Context, event bus.Event) error {
	if p.fail {
		return errors.New("fixture publication refusal")
	}
	record, err := outbox.From(ctx, event)
	if err != nil {
		return err
	}
	p.records = append(p.records, record)
	return nil
}
func projectorContext(ctx context.Context, e *envelopepb.Envelope) context.Context {
	tenant := e.TenantId
	if tenant == "" {
		tenant = bus.SystemTenant
	}
	ctx = bus.WithTenantID(ctx, tenant)
	ctx = bus.WithCorrelationID(ctx, e.CorrelationId)
	ctx = bus.WithCausationID(ctx, e.EventId)
	return bus.WithTraceContext(ctx, e.TraceContext)
}
func projectorEnvelope(tenant, eventType string) *envelopepb.Envelope {
	return &envelopepb.Envelope{TenantId: tenant, EventType: eventType, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, EventId: uuid.Must(uuid.NewV7()).String(), CorrelationId: uuid.Must(uuid.NewV7()).String(), CausationId: uuid.Must(uuid.NewV7()).String(), TraceContext: "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"}
}
func projectorPayload(t *testing.T, kind, portfolio string, f *orderpb.Fill) []byte {
	t.Helper()
	state := &orderpb.OrderState{OrderId: f.GetOrderId(), PortfolioId: portfolio, Venue: f.GetVenue(), VenueAccountId: f.GetVenueAccountId(), InstrumentId: f.GetInstrumentId(), Side: f.GetSide()}
	switch kind {
	case fillfact.SubjectFilled:
		return wire(t, &orderpb.OrderFilled{State: state, Fill: f})
	case fillfact.SubjectPartiallyFilled:
		return wire(t, &orderpb.OrderPartiallyFilled{State: state, Fill: f})
	case fillfact.SubjectRecovered:
		return wire(t, &orderpb.ExecutionRecovered{State: state, Fill: f})
	default:
		return nil
	}
}
func projectorRequest(t *testing.T, f *fixture, e *envelopepb.Envelope, payload []byte) map[string]any {
	t.Helper()
	if payload == nil {
		payload = []byte{}
	} // JSON raw bytes: empty base64 string, never null.
	return map[string]any{"mode": "position-projector", "schema": f.schema, "tenant": f.tenant, "state": wire(t, e), "fill": payload, "at": clock(at), "limit": 128}
}
func projectorOracle(t *testing.T, f *fixture, capture *projectorCapture) *position.Projector {
	t.Helper()
	p, err := position.NewProjector(position.NewPostgres(f.pool, "USD"), capture, f.tenant)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func projectorConsumers(t *testing.T, f *fixture, subjects []string) map[string]relayBroker {
	t.Helper()
	address := os.Getenv("TEST_NATS_URL")
	if address == "" {
		t.Skip("TEST_NATS_URL unavailable: actual projector broker required")
	}
	nc, err := nats.Connect(address, nats.Timeout(5*time.Second))
	if err != nil {
		t.Fatal("broker unavailable")
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	// Require the canonical topology already installed by the workflow.
	result := map[string]relayBroker{}
	for _, subj := range subjects {
		stream, err := bustest.StreamFor(f.ctx, js, subj)
		if err != nil {
			t.Fatal("canonical projector stream absent")
		}
		name := "projector-" + uuid.NewString()
		c, err := js.CreateConsumer(f.ctx, stream, jetstream.ConsumerConfig{Name: name, FilterSubject: subj, DeliverPolicy: jetstream.DeliverNewPolicy, AckPolicy: jetstream.AckExplicitPolicy})
		if err != nil {
			t.Fatal(err)
		}
		result[subj] = relayBroker{consumer: c}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if js.DeleteConsumer(ctx, stream, name) != nil {
				t.Error("projector consumer cleanup failed")
			}
		})
	}
	return result
}
func projectorSubjects(f *fixture, portfolio string, fill *orderpb.Fill) []string {
	return []string{subject.PositionFor(f.tenant, portfolio, fill.InstrumentId), subject.VenuePositionFor(f.tenant, portfolio, fill.Venue, fill.InstrumentId), fillfact.RecoveryPositionApplied}
}
func verifyProjectorRecords(t *testing.T, records []outbox.Record, consumers map[string]relayBroker, since time.Time) {
	t.Helper()
	sequences := map[string]uint64{}
	for _, record := range records {
		key := record.EventType + "/" + record.PartitionKey
		sequences[key]++
		b, ok := consumers[record.Subject]
		if !ok {
			t.Fatal("unobserved projector subject")
		}
		verifyRelayFrame(t, record, b.next(t, record.PartitionKey), sequences[key], since)
	}
}
func TestRustPositionProjectorPostgresNATS(t *testing.T) {
	t.Run("filled-partial-lineage-and-restart", func(t *testing.T) {
		f, g := newFixture(t), newFixture(t)
		portfolio := f.schema
		v := fill()
		consumers := projectorConsumers(t, f, projectorSubjects(f, portfolio, v))
		capture := &projectorCapture{}
		goProjector := projectorOracle(t, g, capture)
		for i, kind := range []string{fillfact.SubjectPartiallyFilled, fillfact.SubjectFilled} {
			v = proto.Clone(v).(*orderpb.Fill)
			v.FillId = uuid.NewString()
			v.VenueExecutionId = v.FillId
			env := projectorEnvelope(f.tenant, kind)
			payload := projectorPayload(t, kind, portfolio, v)
			if err := goProjector.Handle(projectorContext(g.ctx, env), env, payload); err != nil {
				t.Fatal(err)
			}
			since := time.Now()
			result := runResult(t, projectorRequest(t, f, env, payload))
			expect(t, result.Status, "handled")
			if result.Sent != 2 || !result.SessionsClosed {
				t.Fatal("projector did not finish both announcements and its SQL session")
			}
			verifyProjectorRecords(t, capture.records[i*2:], consumers, since)
			want, err := position.NewPostgres(g.pool, "USD").Snapshot(g.ctx, portfolio, at)
			if err != nil {
				t.Fatal(err)
			}
			got, err := position.NewPostgres(f.open(f.tenant), "USD").Snapshot(f.ctx, portfolio, at)
			if err != nil || !proto.Equal(got, want) {
				t.Fatal("Go/Rust durable projector state differs")
			}
		}
	})
	t.Run("unknown-and-nil-fill-ignore-but-foreign-refuses", func(t *testing.T) {
		f, g := newFixture(t), newFixture(t)
		capture := &projectorCapture{}
		goProjector := projectorOracle(t, g, capture)
		for _, tc := range []struct {
			kind, tenant, want string
			payload            []byte
		}{{"other.event", f.tenant, "ignored", []byte{255}}, {fillfact.SubjectFilled, f.tenant, "ignored", wire(t, &orderpb.OrderFilled{})}, {"other.event", "foreign", "error", nil}} {
			env := projectorEnvelope(tc.tenant, tc.kind)
			before := positionDurable(t, f)
			err := goProjector.Handle(projectorContext(g.ctx, env), env, tc.payload)
			result := runResult(t, projectorRequest(t, f, env, tc.payload))
			if tc.want == "error" {
				if err == nil || result.Status != "InvalidInput" {
					t.Fatal("foreign scope accepted")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				expect(t, result.Status, tc.want)
			}
			if !reflect.DeepEqual(before, positionDurable(t, f)) || len(capture.records) != 0 {
				t.Fatal("ignored/refused input changed position owners")
			}
		}
	})
	t.Run("unknown-and-duplicate-embedded-wire-journal", func(t *testing.T) {
		f, g := newFixture(t), newFixture(t)
		portfolio := f.schema
		v := fill()
		env := projectorEnvelope(f.tenant, fillfact.SubjectFilled)
		base := &orderpb.OrderFilled{State: &orderpb.OrderState{PortfolioId: portfolio}, Fill: v}
		base.ProtoReflect().SetUnknown(varField(199, 7))
		v.Price.ProtoReflect().SetUnknown(varField(198, 8))
		payload := wire(t, base)
		fragment := &orderpb.Fill{Price: &commonpb.Decimal{Coefficient: 130}}
		fragment.ProtoReflect().SetUnknown(varField(197, 9))
		number := base.ProtoReflect().Descriptor().Fields().ByName("fill").Number()
		payload = protowire.AppendBytes(protowire.AppendTag(payload, protowire.Number(number), protowire.BytesType), wire(t, fragment))
		capture := &projectorCapture{}
		goProjector := projectorOracle(t, g, capture)
		consumers := projectorConsumers(t, f, projectorSubjects(f, portfolio, v))
		if err := goProjector.Handle(projectorContext(g.ctx, env), env, payload); err != nil {
			t.Fatal(err)
		}
		since := time.Now()
		result := runResult(t, projectorRequest(t, f, env, payload))
		expect(t, result.Status, "handled")
		if result.Sent != 2 || !result.SessionsClosed {
			t.Fatal("wire fixture publication incomplete")
		}
		verifyProjectorRecords(t, capture.records, consumers, since)
		var want, got []byte
		if g.pool.QueryRow(g.ctx, "SELECT fill FROM position_execution_history").Scan(&want) != nil || f.pool.QueryRow(f.ctx, "SELECT fill FROM position_execution_history").Scan(&got) != nil || !bytes.Equal(want, got) {
			t.Fatal("merged unknown wire journal differs from Go")
		}
	})
	t.Run("recovery-ack-binding-and-invalid-provenance-rollback", func(t *testing.T) {
		f, g := newFixture(t), newFixture(t)
		portfolio := f.schema
		v := fill()
		digest, err := fillfact.ExecutionDigest(v)
		if err != nil {
			t.Fatal(err)
		}
		v.Recovery = &orderpb.ExecutionRecoveryProvenance{CaseId: "case", MappingVersion: "fixture-v1", SourceCursor: "cursor", PayloadDigest: strings.Repeat("a", 64), ExecutionDigest: digest}
		env := projectorEnvelope(f.tenant, fillfact.SubjectRecovered)
		payload := projectorPayload(t, env.EventType, portfolio, v)
		capture := &projectorCapture{}
		goProjector := projectorOracle(t, g, capture)
		consumers := projectorConsumers(t, f, projectorSubjects(f, portfolio, v))
		before := time.Now()
		if err := goProjector.Handle(projectorContext(g.ctx, env), env, payload); err != nil {
			t.Fatal(err)
		}
		after := time.Now()
		if len(capture.records) != 3 {
			t.Fatal("Go recovery must announce both positions plus ACK")
		}
		ackRecord := &capture.records[2]
		var ack orderpb.ExecutionRecoveryApplied
		if proto.Unmarshal(ackRecord.Payload, &ack) != nil || ack.RecordedAt.AsTime().Before(before) || ack.RecordedAt.AsTime().After(after) || ackRecord.EventTime.Before(before) || ackRecord.EventTime.After(after) {
			t.Fatal("Go ACK clock outside actual call")
		}
		ack.RecordedAt = timestamppb.New(at)
		ackRecord.EventTime = at
		ackRecord.Payload = wire(t, &ack)
		since := time.Now()
		result := runResult(t, projectorRequest(t, f, env, payload))
		expect(t, result.Status, "handled")
		if result.Sent != 3 || !result.SessionsClosed {
			t.Fatal("recovery publication/session incomplete")
		}
		verifyProjectorRecords(t, capture.records, consumers, since)
		invalid := proto.Clone(v).(*orderpb.Fill)
		invalid.FillId = "incomplete"
		invalid.VenueExecutionId = "incomplete"
		invalid.Recovery.CaseId = ""
		invalid.Recovery.ExecutionDigest, err = fillfact.ExecutionDigest(invalid)
		if err != nil {
			t.Fatal(err)
		}
		bad := projectorPayload(t, env.EventType, portfolio, invalid)
		unchanged := positionDurable(t, f)
		if goProjector.Handle(projectorContext(g.ctx, env), env, bad) == nil {
			t.Fatal("Go accepted incomplete ACK provenance")
		}
		expect(t, run(t, projectorRequest(t, f, env, bad)), "InvalidInput")
		if !reflect.DeepEqual(unchanged, positionDurable(t, f)) {
			t.Fatal("failed recovery ACK leaked fold/history/claim/outbox")
		}
	})
	t.Run("flush-failure-keeps-committed-book-and-restart-retries", func(t *testing.T) {
		f, g := newFixture(t), newFixture(t)
		portfolio := f.schema
		v := fill()
		env := projectorEnvelope(f.tenant, fillfact.SubjectFilled)
		payload := projectorPayload(t, env.EventType, portfolio, v)
		consumers := projectorConsumers(t, f, projectorSubjects(f, portfolio, v))
		capture := &projectorCapture{fail: true}
		goProjector := projectorOracle(t, g, capture)
		if goProjector.Handle(projectorContext(g.ctx, env), env, payload) == nil {
			t.Fatal("Go flush failure was acknowledged")
		}
		request := projectorRequest(t, f, env, payload)
		request["other"] = "fail-first"
		result := runResult(t, request)
		expect(t, result.Status, "PublicationUnconfirmed")
		if !result.SessionsClosed {
			t.Fatal("failed flush left SQL session")
		}
		key := subject.PositionFor(f.tenant, portfolio, v.InstrumentId)
		pending := pendingRelay(t, f, key)
		if len(pending) != 2 || pending[0].Attempts != 1 || pending[1].Attempts != 0 {
			t.Fatal("failed head did not preserve ordered pending records")
		}
		var claims int
		if f.pool.QueryRow(f.ctx, "SELECT count(*) FROM position_fills").Scan(&claims) != nil || claims != 1 {
			t.Fatal("publication failure lost committed claim")
		}
		capture.fail = false
		if err := goProjector.Handle(projectorContext(g.ctx, env), env, payload); err != nil {
			t.Fatal(err)
		}
		delete(request, "other")
		// POSITION retains only the latest message per subject. Arm a bounded
		// two-message pull before publication so both acknowledged frames remain
		// observable even when the next state compacts the first stored record.
		batches := map[string]jetstream.MessageBatch{}
		for _, subj := range projectorSubjects(f, portfolio, v)[:2] {
			consumer := consumers[subj].consumer
			batch, err := consumer.Fetch(2, jetstream.FetchMaxWait(30*time.Second))
			if err != nil {
				t.Fatal(err)
			}
			batches[subj] = batch
			ready, cancel := context.WithTimeout(f.ctx, 5*time.Second)
			tick := time.NewTicker(10 * time.Millisecond)
			armed := false
			for !armed {
				info, err := consumer.Info(ready)
				if err != nil {
					cancel()
					tick.Stop()
					t.Fatal(err)
				}
				armed = info.NumWaiting > 0
				if !armed {
					select {
					case <-ready.Done():
						cancel()
						tick.Stop()
						t.Fatal("broker pull was not armed")
					case <-tick.C:
					}
				}
			}
			cancel()
			tick.Stop()
		}
		since := time.Now()
		result = runResult(t, request)
		expect(t, result.Status, "handled")
		if result.Sent != 4 || !result.SessionsClosed {
			t.Fatal("restart did not drain old and duplicate announcements")
		}
		sequences := map[string]uint64{}
		for _, record := range capture.records {
			batch := batches[record.Subject]
			select {
			case msg, ok := <-batch.Messages():
				if !ok {
					t.Fatal("prefetched broker frame absent", batch.Error())
				}
				sequences[record.Subject]++
				frame := verifyRelayFrame(t, record, msg.Data(), sequences[record.Subject], since)
				if msg.Headers().Get("Nats-Msg-Id") != frame.Envelope.IdempotencyKey || msg.Headers().Get("Kanz-Partition-Key") != record.PartitionKey {
					t.Fatal("prefetched broker headers differ")
				}
				ack, cancel := context.WithTimeout(f.ctx, 5*time.Second)
				err := msg.DoubleAck(ack)
				cancel()
				if err != nil {
					t.Fatal("prefetched broker acknowledgement failed")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("prefetched broker frame timed out")
			}
		}
		for _, batch := range batches {
			if _, more := <-batch.Messages(); more || batch.Error() != nil {
				t.Fatal("prefetched batch did not complete exactly")
			}
		}
		if f.pool.QueryRow(f.ctx, "SELECT count(*) FROM position_fills").Scan(&claims) != nil || claims != 1 || len(pendingRelay(t, f, key)) != 0 {
			t.Fatal("redelivery refolded or lost publication")
		}
	})

	t.Run("deep-domain-and-malformed-refuse-without-writes", func(t *testing.T) {
		f, g := newFixture(t), newFixture(t)
		capture := &projectorCapture{}
		oracle := projectorOracle(t, g, capture)
		env := projectorEnvelope(f.tenant, fillfact.SubjectFilled)
		var event orderpb.OrderFilled
		if err := proto.Unmarshal(projectorPayload(t, env.EventType, f.schema, fill()), &event); err != nil {
			t.Fatal(err)
		}
		// This field is not used by the position fold, but Go validates the whole event.
		event.State.LimitPrice = &commonpb.Decimal{Coefficient: 1, Exponent: 65}
		for _, payload := range [][]byte{wire(t, &event), {255}} {
			before := positionDurable(t, f)
			if oracle.Handle(projectorContext(g.ctx, env), env, payload) == nil {
				t.Fatal("Go accepted invalid complete payload")
			}
			expect(t, run(t, projectorRequest(t, f, env, payload)), "InvalidInput")
			if !reflect.DeepEqual(before, positionDurable(t, f)) || len(capture.records) != 0 {
				t.Fatal("invalid payload changed durable owners or announced")
			}
		}
	})
	t.Run("commit-rollback-and-go-rust-go-handoff", func(t *testing.T) {
		f := newFixture(t)
		portfolio := f.schema + ".*%é"
		v := fill()
		capture := &projectorCapture{}
		oracle := projectorOracle(t, f, capture)
		consumers := projectorConsumers(t, f, projectorSubjects(f, portfolio, v))
		env := projectorEnvelope(f.tenant, fillfact.SubjectFilled)
		payload := projectorPayload(t, env.EventType, portfolio, v)
		if err := oracle.Handle(projectorContext(f.ctx, env), env, payload); err != nil {
			t.Fatal(err)
		}
		before := positionDurable(t, f)
		if _, err := f.pool.Exec(f.ctx, `CREATE FUNCTION refuse_projector_commit() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'fixture'; END$$; CREATE CONSTRAINT TRIGGER refuse_projector_commit AFTER INSERT ON outbox DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION refuse_projector_commit()`); err != nil {
			t.Fatal(err)
		}
		next := proto.Clone(v).(*orderpb.Fill)
		next.FillId, next.VenueExecutionId = "next", "next"
		payload = projectorPayload(t, env.EventType, portfolio, next)
		request := projectorRequest(t, f, env, payload)
		expect(t, run(t, request), "CommitUnconfirmed")
		if !reflect.DeepEqual(before, positionDurable(t, f)) {
			t.Fatal("deferred commit failure changed position/history/claim/fee/outbox")
		}
		if _, err := f.pool.Exec(f.ctx, "DROP TRIGGER refuse_projector_commit ON outbox"); err != nil {
			t.Fatal(err)
		}
		since := time.Now()
		result := runResult(t, request)
		expect(t, result.Status, "handled")
		if result.Sent != 2 || !result.SessionsClosed {
			t.Fatal("Rust retry did not publish and close")
		}
		// Read Rust-owned bytes through the unchanged Go store after a fresh connection.
		actual, err := position.NewPostgres(f.open(f.tenant), "USD").Snapshot(f.ctx, portfolio, at)
		if err != nil {
			t.Fatal(err)
		}
		independent := position.NewBook("USD")
		for _, item := range []*orderpb.Fill{v, next} {
			if _, err := independent.Apply(f.ctx, portfolio, item, item.ExecutedAt.AsTime(), nil); err != nil {
				t.Fatal(err)
			}
		}
		want, err := independent.Snapshot(f.ctx, portfolio, at)
		if err != nil || !proto.Equal(actual, want) {
			t.Fatal("cross-writer state differs from complete replay")
		}
		// Go redelivery after Rust must preserve the two execution claims and reconstruct identical records.
		start := len(capture.records)
		if err := oracle.Handle(projectorContext(f.ctx, env), env, payload); err != nil {
			t.Fatal(err)
		}
		verifyProjectorRecords(t, capture.records[start:], consumers, since)
		var claims int
		if f.pool.QueryRow(f.ctx, "SELECT count(*) FROM position_fills").Scan(&claims) != nil || claims != 2 {
			t.Fatal("Go rollback handoff refolded Rust execution")
		}
	})

	t.Run("whole-input-capacity-refuses-without-prefix", func(t *testing.T) {
		f := newFixture(t)
		env := projectorEnvelope(f.tenant, fillfact.SubjectFilled)
		payload := projectorPayload(t, env.EventType, f.schema, fill())
		payload = protowire.AppendTag(payload, 199, protowire.BytesType)
		payload = protowire.AppendBytes(payload, make([]byte, 1<<20))
		before := positionDurable(t, f)
		expect(t, run(t, projectorRequest(t, f, env, payload)), "Capacity")
		if !reflect.DeepEqual(before, positionDurable(t, f)) {
			t.Fatal("bounded-input refusal changed durable owners")
		}
	})
	t.Run("legacy-system-scope-and-strict-recovery-tenant", func(t *testing.T) {
		f, g := newFixture(t), newFixture(t)
		f.tenant, g.tenant = bus.SystemTenant, bus.SystemTenant
		f.pool, g.pool = f.open(f.tenant), g.open(g.tenant)
		capture := &projectorCapture{}
		oracle := projectorOracle(t, g, capture)
		for _, tenant := range []string{"", "foreign"} {
			v := fill()
			v.FillId, v.VenueExecutionId = uuid.NewString(), uuid.NewString()
			env := projectorEnvelope(tenant, fillfact.SubjectFilled)
			payload := projectorPayload(t, env.EventType, f.schema, v)
			if err := oracle.Handle(projectorContext(g.ctx, env), env, payload); err != nil {
				t.Fatal(err)
			}
			result := runResult(t, projectorRequest(t, f, env, payload))
			expect(t, result.Status, "handled")
			if !result.SessionsClosed || result.Sent != 2 {
				t.Fatal("legacy shared bucket behavior changed")
			}
			actual, err := position.NewPostgres(f.pool, "USD").Snapshot(f.ctx, f.schema, at)
			want, oracleErr := position.NewPostgres(g.pool, "USD").Snapshot(g.ctx, f.schema, at)
			if err != nil || oracleErr != nil || !proto.Equal(actual, want) {
				t.Fatal("legacy shared bucket state differs")
			}
			env.EventType = fillfact.SubjectRecovered
			digest, err := fillfact.ExecutionDigest(v)
			if err != nil {
				t.Fatal(err)
			}
			v.Recovery = &orderpb.ExecutionRecoveryProvenance{CaseId: "case", MappingVersion: "v1", SourceCursor: "cursor", PayloadDigest: strings.Repeat("a", 64), ExecutionDigest: digest}
			payload = projectorPayload(t, env.EventType, f.schema, v)
			before := positionDurable(t, f)
			if oracle.Handle(projectorContext(g.ctx, env), env, payload) == nil {
				t.Fatal("Go accepted cross-scope recovery")
			}
			expect(t, run(t, projectorRequest(t, f, env, payload)), "InvalidInput")
			if !reflect.DeepEqual(before, positionDurable(t, f)) {
				t.Fatal("foreign recovery changed durable owners")
			}
		}
	})

}
