package custody

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/bustest"
	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestPostgresJetStreamStatementIdentityDLQAckAndReplay(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" || os.Getenv("TEST_POSTGRES_URL") == "" {
		t.Skip("requires real restricted-role PostgreSQL and JetStream")
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	tenant := "custody-identity-" + suffix
	pool := newCustodyPool(t, tenant)
	requireRestrictedCustodyRole(t, pool)
	applyCustodySchema(t, pool)
	ctx, cancel := context.WithTimeout(bus.WithTenantID(t.Context(), tenant), 60*time.Second)
	defer cancel()
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	// Keep source and DLQ message-ID deduplication domains separate, even on an
	// empty fixture broker. Parking the original FACT must not dedup against it.
	bustest.EnsureSubjects(t, ctx, js, "CUSTODY_IDENTITY_"+suffix, []string{SubjectStatement, SubjectRun, SubjectActionRecorded})
	bustest.EnsureSubjects(t, ctx, js, "CUSTODY_IDENTITY_DLQ_"+suffix, []string{"dlq." + SubjectStatement})
	inputStream, err := bustest.StreamFor(ctx, js, SubjectStatement)
	if err != nil {
		t.Fatal(err)
	}
	deadStream, err := bustest.StreamFor(ctx, js, "dlq."+SubjectStatement)
	if err != nil {
		t.Fatal(err)
	}
	deadName := "identity-dlq-" + suffix
	dead, err := js.CreateConsumer(ctx, deadStream, jetstream.ConsumerConfig{Name: deadName, FilterSubject: "dlq." + SubjectStatement, DeliverPolicy: jetstream.DeliverNewPolicy, AckPolicy: jetstream.AckExplicitPolicy})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = js.DeleteConsumer(context.Background(), deadStream, deadName) })
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "custody-identity-" + suffix})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "custody-fixture", ProducerVersion: "test", Tenant: tenant})
	if err != nil {
		t.Fatal(err)
	}
	type delivery struct {
		env     *envelopepb.Envelope
		payload []byte
		err     error
	}
	handled := make(chan delivery, 16)
	start := func(store Store, group string) func() {
		t.Helper()
		consumer, err := bus.NewConsumer(client, bus.WithDLQ(client), bus.WithDedupWindow(0, 0))
		if err != nil {
			t.Fatal(err)
		}
		owner, err := NewStatementConsumer(store, tenant, testLogger())
		if err != nil {
			t.Fatal(err)
		}
		inputCtx, stop := context.WithCancel(ctx)
		ready, done := make(chan struct{}), make(chan error, 1)
		go func() {
			done <- consumer.SubscribeReady(inputCtx, SubjectStatement, group, func(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
				if env.GetTenantId() != tenant {
					return nil
				}
				err := owner.Handle(ctx, env, payload)
				select {
				case handled <- delivery{env: proto.Clone(env).(*envelopepb.Envelope), payload: bytes.Clone(payload), err: err}:
				case <-ctx.Done():
					return ctx.Err()
				}
				return err
			}, func() { close(ready) })
		}()
		stopped := false
		join := func() {
			if stopped {
				return
			}
			stopped = true
			stop()
			select {
			case err := <-done:
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Error(err)
				}
			case <-time.After(10 * time.Second):
				t.Error("statement identity consumer did not join")
			}
		}
		t.Cleanup(join)
		select {
		case <-ready:
		case err := <-done:
			stopped = true
			stop()
			t.Fatalf("statement subscription exited: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		return join
	}
	next := func(conflict bool) delivery {
		t.Helper()
		select {
		case got := <-handled:
			if (conflict && !errors.Is(got.err, ErrStatementIdentityConflict)) || (!conflict && got.err != nil) || bus.Validate(got.env) != nil || got.env.GetTenantId() != tenant {
				t.Fatalf("canonical ingress outcome: conflict=%t err=%v", conflict, got.err)
			}
			return got
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		return delivery{}
	}
	assertAcked := func(group string) {
		t.Helper()
		stream, err := js.Stream(ctx, inputStream)
		if err != nil {
			t.Fatal(err)
		}
		var name string
		listing := stream.ListConsumers(ctx)
		for info := range listing.Info() {
			if strings.HasPrefix(info.Name, group+"-") && info.Config.FilterSubject == SubjectStatement {
				if name != "" {
					t.Fatal("ambiguous fixture source durable")
				}
				name = info.Name
			}
		}
		if listing.Err() != nil || name == "" {
			t.Fatalf("source durable missing: %v", listing.Err())
		}
		t.Cleanup(func() { _ = js.DeleteConsumer(context.Background(), inputStream, name) })
		source, err := js.Consumer(ctx, inputStream, name)
		if err != nil {
			t.Fatal(err)
		}
		for {
			info, err := source.Info(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if info.NumAckPending == 0 && info.NumPending == 0 {
				return
			}
			select {
			case <-time.After(10 * time.Millisecond):
			case <-ctx.Done():
				t.Fatal("source receipt was not durably acknowledged after store/DLQ handling")
			}
		}
	}
	publish := func(s Statement, conflict bool) delivery {
		t.Helper()
		msg := &accountingpb.CustodianStatement{StatementId: s.StatementID, PortfolioId: s.PortfolioID, CustodianId: s.CustodianID,
			BusinessDate: timestamppb.New(s.BusinessDate), ReceivedAt: timestamppb.New(s.ReceivedAt), Grain: accountingpb.StatementGrain_STATEMENT_GRAIN_BALANCES_ONLY,
			Positions: []*accountingpb.StatementPosition{{InstrumentId: "AAPL", Quantity: &commonpb.Decimal{Coefficient: s.Positions["AAPL"].Num().Int64()}}}}
		if err := producer.Publish(ctx, bus.Event{Subject: SubjectStatement, EventType: SubjectStatement, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT,
			SchemaVersion: 1, Domain: "accounting", EventTime: s.ReceivedAt, PartitionKey: s.PortfolioID, PayloadSchemaRef: schemaRefStatement, Payload: msg}); err != nil {
			t.Fatal(err)
		}
		got := next(conflict)
		var actual accountingpb.CustodianStatement
		if err := proto.Unmarshal(got.payload, &actual); err != nil || !proto.Equal(&actual, msg) {
			t.Fatalf("real ingress changed source payload: %v", err)
		}
		if conflict {
			parked, err := dead.Next(jetstream.FetchMaxWait(5 * time.Second))
			if err != nil {
				t.Fatal(err)
			}
			env, payload, err := bus.Unframe(parked.Data())
			if err != nil || !proto.Equal(env, got.env) || !bytes.Equal(payload, got.payload) || parked.Headers().Get(bus.HeaderDLQOriginalSubject) != SubjectStatement ||
				parked.Headers().Get(bus.HeaderDLQAttempts) != "1" || !strings.Contains(parked.Headers().Get(bus.HeaderDLQError), ErrStatementIdentityConflict.Error()) {
				t.Fatalf("DLQ did not retain the rejected source identity/frame: %v", err)
			}
			if err := parked.DoubleAck(ctx); err != nil {
				t.Fatal(err)
			}
		}
		return got
	}
	group := "identity-input-" + suffix
	stop := start(NewPostgres(pool), group)
	original := statement("identity-original", map[string]int64{"AAPL": 90}, nil)
	first := publish(original, false)
	retained := map[string]delivery{first.env.GetEventId(): first}
	store := NewPostgres(pool)
	r, err := NewReconciler(store, loaderFor(bookWith(map[string]int64{"AAPL": 100}, nil)), producer, nil, nil, testLogger(), func() time.Time { return t0 })
	if err != nil {
		t.Fatal(err)
	}
	run, err := r.Reconcile(ctx, subject())
	if err != nil || run.Outcome != OutcomeBreaks || run.StatementID != original.StatementID {
		t.Fatalf("fixture did not establish original immutable comparison: %+v %v", run, err)
	}
	prior := investigatedReferenceBreaks(t, ctx, store)
	snapshot := func(store *Postgres) []byte {
		t.Helper()
		var data []byte
		if err := store.pool.QueryRow(ctx, `SELECT jsonb_build_array(
 (SELECT jsonb_agg(to_jsonb(s) ORDER BY statement_id) FROM custody_statements s),
 (SELECT jsonb_agg(to_jsonb(r) ORDER BY run_id) FROM custody_runs r),
 (SELECT jsonb_agg(to_jsonb(b) ORDER BY break_id) FROM custody_breaks b),
 (SELECT jsonb_agg(to_jsonb(a) ORDER BY request_id) FROM custody_actions a),
 (SELECT jsonb_agg(to_jsonb(o) ORDER BY id) FROM outbox o))`).Scan(&data); err != nil {
			t.Fatal(err)
		}
		return data
	}
	before := snapshot(store)
	for _, change := range statementSubjectChanges() {
		conflicting := cloneStatement(original)
		change.change(&conflicting)
		got := publish(conflicting, true)
		retained[got.env.GetEventId()] = got
		assertReferenceLifecycle(t, ctx, store, prior)
		if !bytes.Equal(before, snapshot(store)) {
			t.Fatalf("rejected %s relabeling changed source/run/lifecycle/outbox history", change.name)
		}
		if _, err := store.LatestStatement(ctx, conflicting.Subject()); !errors.Is(err, ErrNoStatement) {
			t.Fatalf("refused subject acquired independent evidence: %v", err)
		}
	}
	assertAcked(group)
	stop()
	pool.Close()
	reconnected := newCustodyPool(t, tenant)
	requireRestrictedCustodyRole(t, reconnected)
	restarted := NewPostgres(reconnected)
	replayGroup := "identity-replay-" + suffix
	stopReplay := start(restarted, replayGroup)
	// These are the same retained FACT identities and payloads, without another
	// publication. DLQ's message-ID window may suppress duplicate parking.
	for range len(retained) {
		var got delivery
		select {
		case got = <-handled:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		prior, ok := retained[got.env.GetEventId()]
		if !ok || !proto.Equal(prior.env, got.env) || !bytes.Equal(prior.payload, got.payload) ||
			((prior.err == nil) != (got.err == nil)) || (got.err != nil && !errors.Is(got.err, ErrStatementIdentityConflict)) {
			t.Fatal("restart did not reproduce original retained ingress outcomes")
		}
		delete(retained, got.env.GetEventId())
	}
	assertAcked(replayGroup)
	if !bytes.Equal(before, snapshot(restarted)) {
		t.Fatal("retained replay moved or rewrote source/run/lifecycle/outbox history")
	}
	assertReferenceLifecycle(t, ctx, restarted, prior)
	repair := statement("identity-independent-repair", map[string]int64{"AAPL": 100}, nil)
	repair.ReceivedAt = t0.Add(time.Hour)
	publish(repair, false)
	assertAcked(replayGroup)
	stopReplay()
	recovered, err := NewReconciler(restarted, loaderFor(bookWith(map[string]int64{"AAPL": 100}, nil)), producer, nil, nil, testLogger(), func() time.Time { return t0.Add(2 * time.Hour) })
	if err != nil {
		t.Fatal(err)
	}
	clean, err := recovered.Reconcile(ctx, subject())
	if err != nil || clean.Outcome != OutcomeClean || clean.StatementID != repair.StatementID || clean.RunID == run.RunID {
		t.Fatalf("independent evidence did not restore a supported comparison: %+v %v", clean, err)
	}
	var originalQuantity string
	if err := reconnected.QueryRow(ctx, `SELECT positions->>'AAPL' FROM custody_statements WHERE statement_id=$1`, original.StatementID).Scan(&originalQuantity); err != nil || originalQuantity != "90" {
		t.Fatalf("repair changed earlier statement evidence: %s %v", originalQuantity, err)
	}
}
