package custody

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"os"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/bustest"
	"github.com/eighred/kanz/internal/outbox"
	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/eighred/kanz/services/accounting/internal/recon"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

func TestPostgresJetStreamStatementPresenceThroughCompletion(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" || os.Getenv("TEST_POSTGRES_URL") == "" {
		t.Skip("requires real restricted-role PostgreSQL and JetStream")
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	tenant := "custody-presence-" + suffix
	pool := newCustodyPool(t, tenant)
	requireRestrictedCustodyRole(t, pool)
	applyCustodySchema(t, pool)
	ctx, cancel := context.WithTimeout(bus.WithTenantID(t.Context(), tenant), 45*time.Second)
	defer cancel()
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	bustest.EnsureSubjects(t, ctx, js, "CUSTODY_PRESENCE_"+suffix, []string{SubjectStatement, SubjectRun, "dlq." + SubjectStatement})
	stream, err := bustest.StreamFor(ctx, js, SubjectRun)
	if err != nil {
		t.Fatal(err)
	}
	outputName := "presence-output-" + suffix
	output, err := js.CreateConsumer(ctx, stream, jetstream.ConsumerConfig{Name: outputName, FilterSubject: SubjectRun, DeliverPolicy: jetstream.DeliverNewPolicy, AckPolicy: jetstream.AckExplicitPolicy})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = js.DeleteConsumer(context.Background(), stream, outputName) })
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "custody-presence"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "custody-fixture", ProducerVersion: "test", Tenant: tenant})
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := bus.NewConsumer(client, bus.WithDLQ(client), bus.WithDedupWindow(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewStatementConsumer(NewPostgres(pool), tenant, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	inputCtx, stopInput := context.WithCancel(ctx)
	ready, done := make(chan struct{}), make(chan error, 1)
	ingested := make(chan error, 3)
	go func() {
		done <- consumer.SubscribeReady(inputCtx, SubjectStatement, "presence-input-"+suffix, func(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
			// The canonical stream can retain other fixtures. Only this fixture's
			// tenant is dispatched to the real tenant-pinned statement handler.
			if env.GetTenantId() != tenant {
				return nil
			}
			err := handler.Handle(ctx, env, payload)
			select {
			case ingested <- err:
			default:
			}
			return err
		}, func() { close(ready) })
	}()
	joined := false
	defer func() {
		stopInput()
		if !joined {
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Error("statement consumer did not join")
			}
		}
	}()
	select {
	case <-ready:
	case err := <-done:
		joined = true
		t.Fatalf("statement subscription exited: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	publish := func(msg *accountingpb.CustodianStatement) error {
		t.Helper()
		if err := producer.Publish(ctx, bus.Event{Subject: SubjectStatement, EventType: SubjectStatement, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT,
			SchemaVersion: 1, Domain: "accounting", EventTime: msg.ReceivedAt.AsTime(), PartitionKey: msg.PortfolioId, PayloadSchemaRef: schemaRefStatement, Payload: msg}); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-ingested:
			return err
		case err := <-done:
			joined = true
			t.Fatalf("statement subscription exited: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		return nil
	}
	for _, field := range []string{"quantity", "balance"} {
		bad := presenceStatement("missing-" + field)
		if field == "quantity" {
			bad.Positions[0].Quantity = nil
		} else {
			bad.Cash[0].Balance = nil
		}
		if err := publish(bad); err == nil {
			t.Fatalf("real ingress accepted missing required %s", field)
		}
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM custody_statements`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("refused ingress wrote partial state: count=%d err=%v", count, err)
	}
	msg := presenceStatement("accepted")
	msg.Transactions = []*accountingpb.StatementTransaction{
		{ExternalRef: "unavailable"},
		{ExternalRef: "zero", Quantity: &commonpb.Decimal{}, Price: &commonpb.Decimal{}, Cash: &commonpb.Decimal{}},
		{ExternalRef: "tiny", Quantity: &commonpb.Decimal{Coefficient: 1, Exponent: -18}},
	}
	if err := publish(msg); err != nil {
		t.Fatal(err)
	}
	stopInput()
	select {
	case err := <-done:
		joined = true
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	pool.Close()
	reconnected := newCustodyPool(t, tenant)
	requireRestrictedCustodyRole(t, reconnected)
	store := NewPostgres(reconnected)
	statement, err := store.LatestStatement(ctx, subject())
	if err != nil || len(statement.Transactions) != 3 || statement.Transactions[0].Quantity != nil || statement.Transactions[0].Price != nil || statement.Transactions[0].Cash != nil {
		t.Fatalf("reconnected statement changed unavailable figures: %+v %v", statement, err)
	}
	scope, err := NewBookScope([]Subject{subject()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewReconciler(store, LedgerBookLoader(ledger.NewPostgres(reconnected), scope, testLogger()), nil, nil, nil, testLogger(), func() time.Time { return t0 })
	if err != nil {
		t.Fatal(err)
	}
	run, err := r.Reconcile(ctx, subject())
	if !errors.Is(err, ErrPublicationPending) || run.Outcome != OutcomeBreaks || len(run.Breaks) != 3 {
		t.Fatalf("reference-only discrepancy did not commit with pending delivery: %+v %v", run, err)
	}
	assertPresenceRun(t, run)
	for _, item := range run.Breaks {
		if item.Key == "unavailable" {
			// A later queue update must not rewrite the earlier completion's
			// unavailable amount when CommitRun recovers its immutable FACT.
			item.Custodian, item.Diff = big.NewRat(7, 1), big.NewRat(-7, 1)
			if _, err := store.UpsertBreaks(ctx, subject(), []Break{item}, t0.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
		}
	}
	reconnected.Close()
	restarted := NewPostgres(newCustodyPool(t, tenant))
	// Idempotent recovery reads the original completion FACT, not current queue
	// values. The decoder must preserve its omitted amounts after a reconnect.
	retry, err := restarted.CommitRun(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	assertPresenceRun(t, retry)
	accountingProducer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "accounting", ProducerVersion: "test", Tenant: tenant})
	if err != nil {
		t.Fatal(err)
	}
	relay, err := outbox.NewRelay(restarted.Outbox(), accountingProducer, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if n, err := relay.DrainOnce(ctx); err != nil || n != 1 {
		t.Fatalf("completion recovery: n=%d err=%v", n, err)
	}
	var sequence uint64
	for delivery := 0; delivery < 2; delivery++ {
		message, err := output.Next(jetstream.FetchMaxWait(5 * time.Second))
		if err != nil {
			t.Fatal(err)
		}
		env, payload, err := bus.Unframe(message.Data())
		if err != nil {
			t.Fatal(err)
		}
		if err := bus.Validate(env); err != nil {
			t.Fatal(err)
		}
		if env.GetTenantId() != tenant || env.GetEventType() != SubjectRun || env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_FACT || env.GetCorrelationId() != run.RunID {
			t.Fatalf("wrong completion envelope: %v", env)
		}
		fact, err := runFromRecord(payload)
		if err != nil || fact.RunID != run.RunID {
			t.Fatalf("invalid completion FACT: %+v %v", fact, err)
		}
		assertPresenceRun(t, fact)
		meta, err := message.Metadata()
		if err != nil {
			t.Fatal(err)
		}
		if delivery == 0 {
			sequence = meta.Sequence.Stream
			if err := message.Nak(); err != nil {
				t.Fatal(err)
			}
		} else {
			if meta.Sequence.Stream != sequence || meta.NumDelivered < 2 {
				t.Fatal("completion was not genuinely redelivered by JetStream")
			}
			if err := message.DoubleAck(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	retry, err = restarted.CommitRun(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	assertPresenceRun(t, retry)
	if n, err := relay.DrainOnce(ctx); err != nil || n != 0 {
		t.Fatalf("retry duplicated the completion: n=%d err=%v", n, err)
	}
}

func assertPresenceRun(t *testing.T, run Run) {
	t.Helper()
	if run.Outcome != OutcomeBreaks || len(run.Breaks) != 3 {
		t.Fatalf("lost reference-level discrepancies: %+v", run)
	}
	seen := map[string]bool{}
	for _, item := range run.Breaks {
		if seen[item.Key] {
			t.Fatalf("duplicate reference: %s", item.Key)
		}
		seen[item.Key] = true
		if item.Kind != recon.BreakExecutionMissingInIBOR || !item.ValuesVerified || item.IBOR == nil || item.IBOR.Sign() != 0 || !item.Status.Outstanding() {
			t.Fatalf("lost known book zero, provenance or outstanding state: %+v", item)
		}
		switch item.Key {
		case "unavailable":
			if item.Custodian != nil || item.Diff != nil {
				t.Fatalf("unavailable FACT amount became zero: %+v", item)
			}
		case "zero":
			if item.Custodian == nil || item.Custodian.Sign() != 0 || item.Diff == nil || item.Diff.Sign() != 0 {
				t.Fatalf("reported zero became unavailable: %+v", item)
			}
		case "tiny":
			want := new(big.Rat).SetFrac(big.NewInt(1), big.NewInt(1000000000000000000))
			if item.Custodian == nil || item.Custodian.Cmp(want) != 0 || item.Diff == nil || item.Diff.Cmp(new(big.Rat).Neg(want)) != 0 {
				t.Fatalf("FACT rounded the final 1e-18 unit: %+v", item)
			}
		default:
			t.Fatalf("unexpected reference: %s", item.Key)
		}
	}
}
