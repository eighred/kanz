package server

import (
	"bytes"
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/bustest"
	"github.com/eighred/kanz/internal/outbox"
	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/custody"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
)

func TestPostgresJetStreamAccountingScopedActionFACT(t *testing.T) {
	broker := os.Getenv("TEST_NATS_URL")
	if broker == "" || os.Getenv("TEST_POSTGRES_URL") == "" {
		t.Skip("requires real restricted PostgreSQL and canonical NATS JetStream")
	}
	pool := newServerPool(t)
	st, ids := scopeSeedStored(t, pool)
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	nc, err := nats.Connect(broker)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	bustest.EnsureSubjects(t, ctx, js, "ACCOUNTING_SCOPE_"+suffix, []string{custody.SubjectActionRecorded})
	stream, err := bustest.StreamFor(ctx, js, custody.SubjectActionRecorded)
	if err != nil {
		t.Fatal(err)
	}
	name := "accounting-scope-" + suffix
	consumer, err := js.CreateConsumer(ctx, stream, jetstream.ConsumerConfig{Name: name, FilterSubject: custody.SubjectActionRecorded, DeliverPolicy: jetstream.DeliverNewPolicy, AckPolicy: jetstream.AckExplicitPolicy})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = js.DeleteConsumer(context.Background(), stream, name) })
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: broker, Name: name})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "accounting", ProducerVersion: "scope-test", Tenant: scopeTenant})
	if err != nil {
		t.Fatal(err)
	}
	relay, err := outbox.NewRelay(st.Outbox(), producer, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(New(&Readiness{}, nil, ledger.NewPostgres(pool), "USD", WithTenant(scopeTenant), WithBreakStore(st)))
	defer h.Close()
	t.Run("denied_HTTP_does_not_enqueue_or_publish", func(t *testing.T) {
		before := scopeSnapshot(t, pool)
		code, body := scopeActionHTTP(t, h, scopeTenant, ids["PF-B"], "scope-denied", "1", `["PF-A"]`)
		if code != 404 || scopeSnapshot(t, pool) != before {
			t.Fatalf("denied action changed state: %d %s", code, body)
		}
		n, err := relay.Flush(ctx, ids["PF-B"])
		if err != nil || n != 0 {
			t.Fatalf("denied FACT published n=%d %v", n, err)
		}
	})
	request := "scope-broker-" + suffix
	var receipt string
	var original []byte
	var sequence uint64
	t.Run("trusted_HTTP_commits_one_canonical_FACT", func(t *testing.T) {
		code, body := scopeActionHTTP(t, h, scopeTenant, ids["PF-A"], request, "1", `["PF-A"]`)
		if code != 200 {
			t.Fatalf("authorized HTTP %d %s", code, body)
		}
		receipt = body
		n, err := relay.Flush(ctx, ids["PF-A"])
		if err != nil || n != 1 {
			t.Fatalf("canonical relay publish n=%d %v", n, err)
		}
		message, err := consumer.Next(jetstream.FetchMaxWait(5 * time.Second))
		if err != nil {
			t.Fatal(err)
		}
		metadata, err := message.Metadata()
		if err != nil {
			t.Fatal(err)
		}
		original, sequence = bytes.Clone(message.Data()), metadata.Sequence.Stream
		env, payload, err := bus.Unframe(message.Data())
		if err != nil {
			t.Fatal(err)
		}
		if err := bus.Validate(env); err != nil {
			t.Fatalf("canonical envelope: %v", err)
		}
		var fact accountingpb.CustodyActionRecorded
		if err := proto.Unmarshal(payload, &fact); err != nil {
			t.Fatal(err)
		}
		if env.GetTenantId() != scopeTenant || env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_FACT || env.GetPartitionKey() != ids["PF-A"] || env.GetCorrelationId() != request || env.GetPayloadSchemaRef() != "accounting.v1.CustodyActionRecorded:1" || fact.GetActor() != "alice@kanz" || fact.GetBreakId() != ids["PF-A"] || fact.GetBefore().GetRevision() != 1 || fact.GetAfter().GetRevision() != 2 || fact.GetValues().GetIbor() != "100" || fact.GetValues().GetDifference() != "10" {
			t.Fatalf("foreign/inexact canonical action: env=%v fact=%v", env, &fact)
		}
		if strings.Contains(string(payload), "PortfolioScope") {
			t.Fatal("authorization context became historical financial payload")
		}
		if err := message.DoubleAck(ctx); err != nil {
			t.Fatal(err)
		}
		info, err := consumer.Info(ctx)
		if err != nil || info.NumAckPending != 0 || info.NumPending != 0 || info.AckFloor.Stream == 0 {
			t.Fatalf("real FACT ACK missing: %+v %v", info, err)
		}
	})
	if receipt == "" {
		t.Fatal("no committed receipt to qualify restart")
	}
	t.Run("reopen_retry_and_revocation_emit_no_second_FACT", func(t *testing.T) {
		reopened := scopePool(t, pool, scopeTenant)
		freshStore := custody.NewPostgres(reopened)
		fresh := httptest.NewServer(New(&Readiness{}, nil, ledger.NewPostgres(reopened), "USD", WithTenant(scopeTenant), WithBreakStore(freshStore)))
		defer fresh.Close()
		before := scopeSnapshot(t, pool)
		code, body := scopeActionHTTP(t, fresh, scopeTenant, ids["PF-A"], request, "1", `["PF-A","PF-B"]`)
		if code != 200 || body != receipt {
			t.Fatalf("reopened exact receipt: %d %s", code, body)
		}
		code, body = scopeActionHTTP(t, fresh, scopeTenant, ids["PF-A"], request, "1", `["PF-B"]`)
		if code != 404 || scopeSnapshot(t, pool) != before {
			t.Fatalf("revocation changed historical evidence: %d %s", code, body)
		}
		freshRelay, err := outbox.NewRelay(freshStore.Outbox(), producer, nil)
		if err != nil {
			t.Fatal(err)
		}
		n, err := freshRelay.Flush(ctx, ids["PF-A"])
		if err != nil || n != 0 {
			t.Fatalf("retry enqueued duplicate FACT n=%d %v", n, err)
		}
		info, err := consumer.Info(ctx)
		if err != nil || info.NumAckPending != 0 || info.NumPending != 0 {
			t.Fatalf("duplicate broker FACT: %+v %v", info, err)
		}
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE correlation_id=$1 AND published_at IS NOT NULL`, request).Scan(&count); err != nil || count != 1 {
			t.Fatalf("durable exactly-once evidence count=%d %v", count, err)
		}
	})
	t.Run("fresh_consumer_replays_original_retained_FACT_and_ACKs", func(t *testing.T) {
		name := "accounting-scope-replay-" + suffix
		replay, err := js.CreateConsumer(ctx, stream, jetstream.ConsumerConfig{Name: name, FilterSubject: custody.SubjectActionRecorded, DeliverPolicy: jetstream.DeliverByStartSequencePolicy, OptStartSeq: sequence, AckPolicy: jetstream.AckExplicitPolicy})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = js.DeleteConsumer(context.Background(), stream, name) })
		message, err := replay.Next(jetstream.FetchMaxWait(5 * time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(message.Data(), original) {
			t.Fatal("retained action history changed across fresh consumer")
		}
		if err := message.DoubleAck(ctx); err != nil {
			t.Fatal(err)
		}
		info, err := replay.Info(ctx)
		if err != nil || info.NumPending != 0 || info.NumAckPending != 0 || info.AckFloor.Stream < sequence {
			t.Fatalf("fresh retained ACK: %+v %v", info, err)
		}
	})
}
