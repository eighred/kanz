package consume

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	alt "github.com/eighred/kanz/internal/alternatives"
	"github.com/eighred/kanz/internal/bustest"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/alternatives/internal/fund"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestPostgresJetStreamRequiredInputsDLQAckAndRetainedRefusal(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" || os.Getenv("TEST_POSTGRES_URL") == "" {
		t.Skip("requires real JetStream and NOSUPERUSER/NOBYPASSRLS PostgreSQL")
	}
	poolFor := requiredInputPostgres(t)
	suffix := fmt.Sprint(time.Now().UnixNano())
	tenant := "alt-required-" + suffix
	pool := poolFor(tenant)
	store := fund.NewPostgres(pool)
	ctx, cancel := context.WithTimeout(bus.WithTenantID(t.Context(), tenant), 90*time.Second)
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
	kinds := requiredInputKinds()
	var subjects, deadSubjects []string
	for _, kind := range kinds {
		subjects = append(subjects, kind.subject)
		deadSubjects = append(deadSubjects, "dlq."+kind.subject)
	}
	// Reuse the canonical bootstrapped streams. Separate source/DLQ streams
	// also prevent the parked original UUID deduplicating against its source.
	bustest.EnsureSubjects(t, ctx, js, "ALT_REQUIRED_"+suffix, subjects)
	bustest.EnsureSubjects(t, ctx, js, "ALT_REQUIRED_DLQ_"+suffix, deadSubjects)
	type route struct {
		stream string
		dead   jetstream.Consumer
	}
	routes := make(map[string]route)
	for _, kind := range kinds {
		input, err := bustest.StreamFor(ctx, js, kind.subject)
		if err != nil {
			t.Fatal(err)
		}
		deadStream, err := bustest.StreamFor(ctx, js, "dlq."+kind.subject)
		if err != nil {
			t.Fatal(err)
		}
		name := "alt-required-dead-" + kind.name + "-" + suffix
		dead, err := js.CreateConsumer(ctx, deadStream, jetstream.ConsumerConfig{Name: name, FilterSubject: "dlq." + kind.subject,
			DeliverPolicy: jetstream.DeliverNewPolicy, AckPolicy: jetstream.AckExplicitPolicy})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = js.DeleteConsumer(context.Background(), deadStream, name) })
		routes[kind.subject] = route{input, dead}
	}
	publisher, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "alt-required-publisher-" + suffix})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = publisher.Close() }()
	producer, err := bus.NewProducer(publisher, bus.ProducerConfig{Source: "alternatives-fixture", ProducerVersion: "test", Tenant: tenant})
	if err != nil {
		t.Fatal(err)
	}
	type delivery struct {
		env     *envelopepb.Envelope
		payload []byte
		err     error
	}
	handled := make(chan delivery, 256)
	start := func(store fund.Store, group string) func() {
		t.Helper()
		client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: group})
		if err != nil {
			t.Fatal(err)
		}
		consumer, err := bus.NewConsumer(client, bus.WithDLQ(client), bus.WithDedupWindow(0, 0))
		if err != nil {
			_ = client.Close()
			t.Fatal(err)
		}
		inputCtx, stop := context.WithCancel(ctx)
		done := make(chan error, len(kinds))
		started, completed := 0, 0
		stopped := false
		join := func() {
			if stopped {
				return
			}
			stopped = true
			stop()
			for completed < started {
				select {
				case err := <-done:
					completed++
					if err != nil && !errors.Is(err, context.Canceled) {
						t.Error(err)
					}
				case <-time.After(10 * time.Second):
					t.Error("alternatives consumer did not join")
					_ = client.Close()
					return
				}
			}
			_ = client.Close()
		}
		t.Cleanup(join)
		for _, kind := range kinds {
			folder, err := NewFolder(tenant, store, DecodeProto(kind.subject))
			if err != nil {
				t.Fatal(err)
			}
			ready := make(chan struct{})
			started++
			go func() {
				done <- consumer.SubscribeReady(inputCtx, kind.subject, group, func(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
					if env.GetTenantId() != tenant {
						return nil // Ignore unrelated retained fixture tenants.
					}
					err := folder.Handle(ctx, env, payload)
					select {
					case handled <- delivery{proto.Clone(env).(*envelopepb.Envelope), bytes.Clone(payload), err}:
					case <-ctx.Done():
						return ctx.Err()
					}
					return err
				}, func() { close(ready) })
			}()
			select {
			case <-ready:
			case err := <-done:
				completed++
				t.Fatalf("alternatives subscription exited before catch-up: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
		return join
	}
	next := func() delivery {
		t.Helper()
		select {
		case got := <-handled:
			if err := bus.Validate(got.env); err != nil || got.env.GetTenantId() != tenant {
				t.Fatalf("noncanonical ingress: %v", err)
			}
			return got
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		return delivery{}
	}
	type retainedFact struct {
		delivery
		frame []byte
		field string
		seq   uint64
	}
	retained := make(map[string]retainedFact)
	lastSequence := make(map[string]uint64)
	// Envelope event_time is independently valid. Copying an invalid payload
	// date into it would test only the envelope validator and miss this owner.
	publish := func(kind requiredInputKind, msg proto.Message, errorField string) delivery {
		t.Helper()
		if err := producer.Publish(ctx, bus.Event{Subject: kind.subject, EventType: kind.subject, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT,
			SchemaVersion: 1, Domain: "alternatives", EventTime: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
			PartitionKey: msg.(interface{ GetCommitmentId() string }).GetCommitmentId(), Payload: msg}); err != nil {
			t.Fatal(err)
		}
		got := next()
		if got.env.GetEventType() != kind.subject || !bytes.Equal(got.payload, marshalRequiredInput(t, msg)) ||
			(errorField == "" && got.err != nil) || (errorField != "" && (got.err == nil || !strings.Contains(got.err.Error(), errorField))) {
			t.Fatalf("wrong ingress outcome for %s/%s: %v", kind.name, errorField, got.err)
		}
		stream, err := js.Stream(ctx, routes[kind.subject].stream)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := stream.GetLastMsgForSubject(ctx, kind.subject)
		if err != nil {
			t.Fatal(err)
		}
		env, payload, err := bus.Unframe(raw.Data)
		if err != nil || !proto.Equal(env, got.env) || !bytes.Equal(payload, got.payload) {
			t.Fatalf("stored original frame did not match real ingress: %v", err)
		}
		lastSequence[kind.subject] = raw.Sequence
		retained[got.env.GetEventId()] = retainedFact{got, bytes.Clone(raw.Data), errorField, raw.Sequence}
		if errorField != "" {
			dead, err := routes[kind.subject].dead.Next(jetstream.FetchMaxWait(5 * time.Second))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(dead.Data(), raw.Data) || dead.Headers().Get(bus.HeaderDLQOriginalSubject) != kind.subject ||
				dead.Headers().Get(bus.HeaderDLQAttempts) != "1" || !strings.Contains(dead.Headers().Get(bus.HeaderDLQError), errorField) {
				t.Fatal("DLQ lost original bytes, subject, attempt count or decode reason")
			}
			if err := dead.DoubleAck(ctx); err != nil {
				t.Fatal(err)
			}
		}
		return got
	}
	assertAcked := func(group string) {
		t.Helper()
		for _, kind := range kinds {
			stream, err := js.Stream(ctx, routes[kind.subject].stream)
			if err != nil {
				t.Fatal(err)
			}
			var name string
			listing := stream.ListConsumers(ctx)
			for info := range listing.Info() {
				if strings.HasPrefix(info.Name, group+"-") && info.Config.FilterSubject == kind.subject {
					if name != "" {
						t.Fatal("ambiguous source durable")
					}
					name = info.Name
				}
			}
			if listing.Err() != nil || name == "" {
				t.Fatalf("missing source durable: %v", listing.Err())
			}
			t.Cleanup(func() { _ = js.DeleteConsumer(context.Background(), routes[kind.subject].stream, name) })
			source, err := js.Consumer(ctx, routes[kind.subject].stream, name)
			if err != nil {
				t.Fatal(err)
			}
			for {
				info, err := source.Info(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if info.NumPending == 0 && info.NumAckPending == 0 && info.AckFloor.Stream >= lastSequence[kind.subject] {
					break
				}
				select {
				case <-time.After(10 * time.Millisecond):
				case <-ctx.Done():
					t.Fatal("source was not durably ACKed after append/DLQ")
				}
			}
		}
	}
	group := "alt-required-input-" + suffix
	stop := start(store, group)
	for _, kind := range kinds {
		for _, fault := range requiredInputFaults() {
			id := kind.name + "_" + fault.name
			book := kind.commitment(id, "book_"+id)
			bad := publish(kind, kind.message(id, book, fault.amt, fault.at), fault.errorField)
			rows, err := store.Journal(ctx, book)
			if err != nil || len(rows) != 0 {
				t.Fatalf("parked input wrote a financial row: %+v %v", rows, err)
			}
			corrected := publish(kind, kind.message(id, book, &commonpb.Decimal{Coefficient: 10125, Exponent: -2}, timestamppb.New(t0())), "")
			if corrected.env.GetEventId() == bad.env.GetEventId() {
				t.Fatal("corrected notice reused a parked frame identity")
			}
			rows, err = store.Journal(ctx, book)
			if err != nil || len(rows) != 1 || rows[0].EventID != id || rows[0].Amount.RatString() != "405/4" || !rows[0].Date.Equal(t0()) {
				t.Fatalf("refusal consumed domain ID before corrected fresh frame: %+v %v", rows, err)
			}
		}
	}
	const book = "broker-financial-book"
	for _, fact := range requiredFinancialFacts(book) {
		publish(fact.kind, fact.msg, "")
	}
	beforeView := assertRequiredFinancialView(t, store, book, 100, 2, 1)
	before := requiredJournalSnapshot(t, pool)
	publish(kinds[3], kinds[3].message("poison-nav", book, nil, timestamppb.New(t0())), "amount")
	publish(kinds[1], kinds[1].message("poison-date", book, &commonpb.Decimal{Coefficient: 100}, nil), "date")
	if !bytes.Equal(before, requiredJournalSnapshot(t, pool)) || assertRequiredFinancialView(t, store, book, 100, 2, 1) != beforeView {
		t.Fatal("parked inputs changed existing history or served financial state")
	}
	end := timestamppb.New(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	// A fresh frame with an already accepted domain ID cannot rewrite history.
	publish(kinds[3], kinds[3].message("mark", book, &commonpb.Decimal{Coefficient: 120}, end), "")
	assertRequiredFinancialView(t, store, book, 100, 2, 1)
	publish(kinds[3], kinds[3].message("mark-restated", book, &commonpb.Decimal{Coefficient: 120}, end), "")
	publish(kinds[3], kinds[3].message("mark-restated", book, &commonpb.Decimal{Coefficient: 120}, end), "")
	assertRequiredFinancialView(t, store, book, 120, 2.2, 1.2)
	assertAcked(group)
	stop()
	before = requiredJournalSnapshot(t, pool)
	pool.Close()
	restartedPool := poolFor(tenant)
	restarted := fund.NewPostgres(restartedPool)
	replayGroup := "alt-required-replay-" + suffix
	stopReplay := start(restarted, replayGroup)
	// No republishing: a fresh client/Consumer reads the original retained FACTs
	// and must refuse them again even after corrected same-domain-ID facts exist.
	for range len(retained) {
		got := next()
		prior, ok := retained[got.env.GetEventId()]
		if !ok || !proto.Equal(prior.env, got.env) || !bytes.Equal(prior.payload, got.payload) ||
			(prior.field == "" && got.err != nil) || (prior.field != "" && (got.err == nil || !strings.Contains(got.err.Error(), prior.field))) {
			t.Fatalf("retained replay changed ingress identity/bytes/refusal: %v", got.err)
		}
		stream, err := js.Stream(ctx, routes[got.env.GetEventType()].stream)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := stream.GetMsg(ctx, prior.seq)
		if err != nil || !bytes.Equal(raw.Data, prior.frame) {
			t.Fatalf("retained source frame changed or disappeared: %v", err)
		}
		delete(retained, got.env.GetEventId())
	}
	assertAcked(replayGroup)
	stopReplay()
	if !bytes.Equal(before, requiredJournalSnapshot(t, restartedPool)) {
		t.Fatal("retained replay rewrote or duplicated historical source records")
	}
	assertRequiredFinancialView(t, restarted, book, 120, 2.2, 1.2)
	other := fund.NewPostgres(poolFor(tenant + "-other"))
	rows, err := other.Journal(ctx, book)
	if err != nil || len(rows) != 0 {
		t.Fatalf("broker-folded financial journal leaked across RLS: %+v %v", rows, err)
	}
	var marks int
	rows, err = restarted.Journal(ctx, book)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range rows {
		if e.Type == alt.EventNAVMark {
			marks++
			if e.EventID == "mark" && e.Amount.RatString() != "100" {
				t.Fatal("fresh frames/restatement rewrote the original NAV")
			}
		}
	}
	if len(rows) != 5 || marks != 2 {
		t.Fatalf("restart lost immutable original/distinct restatement: rows=%d marks=%d", len(rows), marks)
	}
}
