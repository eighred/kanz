package consume_test

import (
	"context"
	"fmt"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/bustest"
	"github.com/eighred/kanz/internal/fillfact"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/consume"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestRecoveredExecutionsBookOnceAndAcknowledgeThroughRealSpine(t *testing.T) {
	url, dsn := os.Getenv("TEST_NATS_URL"), os.Getenv("TEST_POSTGRES_URL")
	if url == "" || dsn == "" {
		t.Skip("requires real PostgreSQL and JetStream")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	suffix := fmt.Sprint(time.Now().UnixNano())
	tenant := "recovery-" + suffix
	pool := tenantPool(t, ctx, dsn, tenant)
	var super, bypass bool
	if err := pool.QueryRow(ctx, `SELECT rolsuper,rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&super, &bypass); err != nil || super || bypass {
		t.Fatalf("restricted role required: %v", err)
	}
	admin, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	js, err := jetstream.New(admin)
	if err != nil {
		t.Fatal(err)
	}
	bustest.EnsureSubjects(t, ctx, js, "RECOVERY_BOOK_"+suffix, []string{"order.>"})
	bustest.EnsureSubjects(t, ctx, js, "RECOVERY_CASH_"+suffix, []string{consume.SubjectPortfolioCash})
	stream, err := bustest.StreamFor(ctx, js, fillfact.SubjectRecovered)
	if err != nil {
		t.Fatal(err)
	}
	pull := func(name, subject string) jetstream.Consumer {
		c, err := js.CreateConsumer(ctx, stream, jetstream.ConsumerConfig{Name: name + suffix, FilterSubject: subject, DeliverPolicy: jetstream.DeliverNewPolicy, AckPolicy: jetstream.AckExplicitPolicy})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = js.DeleteConsumer(context.Background(), stream, name+suffix) })
		return c
	}
	input, acks := pull("recover-input-", fillfact.SubjectRecovered), pull("recover-ack-", fillfact.RecoveryLedgerApplied)
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "recovery-books"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	oms, err := bus.NewProducer(client, bus.ProducerConfig{Source: "oms", ProducerVersion: "test", Tenant: tenant})
	if err != nil {
		t.Fatal(err)
	}
	accounting, err := bus.NewProducer(client, bus.ProducerConfig{Source: "accounting", ProducerVersion: "test", Tenant: tenant})
	if err != nil {
		t.Fatal(err)
	}
	newFolder := func() *consume.Folder {
		store := ledger.NewPostgres(pool)
		a := consume.NewAnnouncer(store, accounting, "USD", consume.EntrySourcePosture{}, nil, nil)
		f, err := consume.NewFolder(tenant, store, "USD", consume.WithAnnouncer(a))
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	ctx = bus.WithTenantID(ctx, tenant)
	var original *orderpb.ExecutionRecovered
	for _, account := range []string{"account-a", "account-b"} {
		fill := &orderpb.Fill{FillId: "raw-7", VenueExecutionId: "7", OrderId: "order-" + account, InstrumentId: "BTC-USD", Venue: "BINANCE", VenueAccountId: account, Side: orderpb.Side_SIDE_BUY,
			Quantity: &commonpb.Decimal{Coefficient: 1}, Price: &commonpb.Decimal{Coefficient: 100}, Fee: &commonpb.Money{Amount: &commonpb.Decimal{Coefficient: 1}, CurrencyCode: "USD"}, ExecutedAt: timestamppb.New(time.Unix(1700000000, 0))}
		digest, err := fillfact.ExecutionDigest(fill)
		if err != nil {
			t.Fatal(err)
		}
		fill.Recovery = &orderpb.ExecutionRecoveryProvenance{CaseId: "case-" + account, MappingVersion: "mapping", SourceCursor: "cursor", PayloadDigest: strings.Repeat("a", 64), ExecutionDigest: digest, ObservedAt: timestamppb.Now()}
		fact := &orderpb.ExecutionRecovered{Fill: fill, State: &orderpb.OrderState{OrderId: fill.OrderId, PortfolioId: "fund", Venue: fill.Venue, VenueAccountId: account, InstrumentId: fill.InstrumentId, Side: fill.Side, Status: orderpb.OrderStatus_ORDER_STATUS_CANCELLED}}
		original = fact
		if err := oms.Publish(ctx, bus.Event{Subject: fillfact.SubjectRecovered, EventType: fillfact.SubjectRecovered, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, Domain: "order", PartitionKey: fill.OrderId, EventTime: time.Now(), Payload: fact}); err != nil {
			t.Fatal(err)
		}
		var sequence uint64
		for delivery := 0; delivery < 2; delivery++ {
			msg, err := input.Next(jetstream.FetchMaxWait(5 * time.Second))
			if err != nil {
				t.Fatal(err)
			}
			env, payload, err := bus.Unframe(msg.Data())
			if err != nil {
				t.Fatal(err)
			}
			if err := bus.Validate(env); err != nil {
				t.Fatal(err)
			}
			if err := newFolder().Handle(ctx, env, payload); err != nil {
				t.Fatal(err)
			}
			meta, err := msg.Metadata()
			if err != nil {
				t.Fatal(err)
			}
			if delivery == 0 {
				sequence = meta.Sequence.Stream
				if err := msg.Nak(); err != nil {
					t.Fatal(err)
				}
			} else {
				if meta.Sequence.Stream != sequence || meta.NumDelivered < 2 {
					t.Fatal("no post-commit broker redelivery")
				}
				if err := msg.DoubleAck(ctx); err != nil {
					t.Fatal(err)
				}
			}
			ackMsg, err := acks.Next(jetstream.FetchMaxWait(5 * time.Second))
			if err != nil {
				t.Fatal(err)
			}
			ackEnv, ackBytes, err := bus.Unframe(ackMsg.Data())
			if err != nil {
				t.Fatal(err)
			}
			var ack orderpb.ExecutionRecoveryApplied
			if err := proto.Unmarshal(ackBytes, &ack); err != nil || ack.GetExecutionDigest() != digest || ack.GetExecutionKey() != fillfact.ExecutionKey(fill) || ackEnv.GetTenantId() != tenant {
				t.Fatalf("ack=%v err=%v", &ack, err)
			}
			if err := ackMsg.DoubleAck(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	store := ledger.NewPostgres(pool)
	journal, err := store.Journal(ctx, "fund")
	if err != nil || len(journal) != 2 {
		t.Fatalf("entries=%d err=%v", len(journal), err)
	}
	book := ledger.Replay("fund", journal)
	if book.CashBalance("USD").Cmp(big.NewRat(-202, 1)) != 0 || book.Positions["BTC-USD"].Qty.Cmp(big.NewRat(2, 1)) != 0 {
		t.Fatalf("cash=%v position=%v", book.CashBalance("USD"), book.Positions["BTC-USD"])
	}
	// A later conflicting fee cannot hide behind successful deduplication.
	changed := proto.Clone(original).(*orderpb.ExecutionRecovered)
	changed.Fill.Fee.Amount.Coefficient = 2
	changed.Fill.Recovery.ExecutionDigest, err = fillfact.ExecutionDigest(changed.Fill)
	if err != nil {
		t.Fatal(err)
	}
	data, err := proto.Marshal(changed)
	if err != nil {
		t.Fatal(err)
	}
	env := &envelopepb.Envelope{TenantId: tenant, EventType: fillfact.SubjectRecovered, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, EventTime: timestamppb.Now()}
	if err := newFolder().Handle(ctx, env, data); err == nil {
		t.Fatal("unapproved fee correction posted")
	}
	journal, err = store.Journal(ctx, "fund")
	if err != nil || len(journal) != 2 {
		t.Fatalf("fee refusal changed journal: %d %v", len(journal), err)
	}
	previousDigest, err := fillfact.EconomicDigest(original.Fill)
	if err != nil {
		t.Fatal(err)
	}
	changed.Fill.Recovery.CaseId = "approved-fee-case"
	changed.Fill.Recovery.FeeApproval = &orderpb.ExecutionFeeApproval{ProposalId: "approved-fee-case", Digest: strings.Repeat("b", 64), Proposer: "user:maker", Approver: "user:checker", ApprovedAt: timestamppb.Now(), PreviousExecutionDigest: previousDigest, PreviousFee: proto.Clone(original.Fill.Fee).(*commonpb.Money)}
	if err := oms.Publish(ctx, bus.Event{Subject: fillfact.SubjectRecovered, EventType: fillfact.SubjectRecovered, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, Domain: "order", PartitionKey: changed.Fill.OrderId, EventTime: time.Now(), Payload: changed}); err != nil {
		t.Fatal(err)
	}
	var feeSequence uint64
	for delivery := 0; delivery < 2; delivery++ {
		msg, err := input.Next(jetstream.FetchMaxWait(5 * time.Second))
		if err != nil {
			t.Fatal(err)
		}
		wire, body, err := bus.Unframe(msg.Data())
		if err != nil {
			t.Fatal(err)
		}
		if err := newFolder().Handle(ctx, wire, body); err != nil {
			t.Fatal(err)
		}
		meta, err := msg.Metadata()
		if err != nil {
			t.Fatal(err)
		}
		if delivery == 0 {
			feeSequence = meta.Sequence.Stream
			if err := msg.Nak(); err != nil {
				t.Fatal(err)
			}
		} else {
			if meta.Sequence.Stream != feeSequence || meta.NumDelivered < 2 {
				t.Fatal("fee correction was not redelivered")
			}
			if err := msg.DoubleAck(ctx); err != nil {
				t.Fatal(err)
			}
		}
		ackMsg, err := acks.Next(jetstream.FetchMaxWait(5 * time.Second))
		if err != nil {
			t.Fatal(err)
		}
		_, ackBytes, err := bus.Unframe(ackMsg.Data())
		if err != nil {
			t.Fatal(err)
		}
		var ack orderpb.ExecutionRecoveryApplied
		if err := proto.Unmarshal(ackBytes, &ack); err != nil || ack.CaseId != "approved-fee-case" || ack.ExecutionDigest != changed.Fill.Recovery.ExecutionDigest {
			t.Fatalf("fee ack=%v err=%v", &ack, err)
		}
		if err := ackMsg.DoubleAck(ctx); err != nil {
			t.Fatal(err)
		}
	}
	journal, err = store.Journal(ctx, "fund")
	if err != nil || len(journal) != 3 {
		t.Fatalf("fee entries=%d err=%v", len(journal), err)
	}
	book = ledger.Replay("fund", journal)
	if book.CashBalance("USD").Cmp(big.NewRat(-203, 1)) != 0 || book.Positions["BTC-USD"].Qty.Cmp(big.NewRat(2, 1)) != 0 {
		t.Fatalf("fee redelivery changed economics: cash=%v position=%v", book.CashBalance("USD"), book.Positions["BTC-USD"])
	}
}
