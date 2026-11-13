package consume_test

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/bustest"
	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/outbox"
	"github.com/eighred/kanz/internal/pg"
	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/consume"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

const cashTimeSubject = "accounting.cash.subscription"

func cashTimeRestrictedPool(t *testing.T, ctx context.Context, dsn, tenant string) *pgxpool.Pool {
	t.Helper()
	pool := tenantPool(t, ctx, dsn, tenant)
	var privileged bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles
		WHERE pg_has_role(current_user, oid, 'MEMBER') AND (rolsuper OR rolbypassrls))`).Scan(&privileged); err != nil || privileged {
		t.Fatalf("restricted PostgreSQL role and memberships required: privileged=%v error=%v", privileged, err)
	}
	t.Log("actual PostgreSQL role and memberships: NOSUPERUSER NOBYPASSRLS; real tenant-scoped accounting pool")
	return pool
}

func cashTimeWireEntry(id, portfolio string, coefficient int64, known, effective *timestamppb.Timestamp) *accountingpb.LedgerEntry {
	return &accountingpb.LedgerEntry{EntryId: id, PortfolioId: portfolio, VenueAccountId: "ACCOUNT", EntryType: accountingpb.EntryType_ENTRY_TYPE_CASH,
		Cash: &commonpb.Decimal{Coefficient: coefficient, Exponent: -2}, CashCurrency: "USD", SourceRef: "cash-time-fixture", KnowledgeTime: known, EffectiveTime: effective}
}

func marshalCashTime(t *testing.T, entry *accountingpb.LedgerEntry) []byte {
	t.Helper()
	payload, err := proto.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func malformedCashTimeEntries(portfolio string, at time.Time) []*accountingpb.LedgerEntry {
	var entries []*accountingpb.LedgerEntry
	for _, field := range []string{"knowledge", "effective"} {
		for _, stamp := range []*timestamppb.Timestamp{
			{Seconds: at.Unix(), Nanos: -1}, {Seconds: at.Unix() + 1, Nanos: -1000000000},
			{Seconds: at.Unix(), Nanos: 1000000000}, {Seconds: -62135596801}, {Seconds: 253402300800},
		} {
			e := cashTimeWireEntry(fmt.Sprintf("cash:bad-%d", len(entries)), portfolio, 10000, timestamppb.New(at), timestamppb.New(at))
			if field == "knowledge" {
				e.KnowledgeTime = stamp
			} else {
				e.EffectiveTime = stamp
			}
			entries = append(entries, e)
		}
	}
	return entries
}

type cashTimeCapture struct{ events []bus.Event }

func (p *cashTimeCapture) Publish(_ context.Context, e bus.Event) error {
	p.events = append(p.events, e)
	return nil
}

func cashTimeAnnouncer(st ledger.Store, pub consume.Publisher, at time.Time) *consume.Announcer {
	return consume.NewAnnouncer(st, pub, "USD", consume.EntrySourcePosture{Produced: []string{"cash", "fee"}, Unproduced: []string{"trade", "corporate_action", "accrual"}}, nil, func() time.Time { return at })
}

func cashTimeCounts(t *testing.T, ctx context.Context, pool *pgxpool.Pool, portfolio string) (int, int) {
	t.Helper()
	var entries, records int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM ledger_entries WHERE portfolio_id=$1),
		(SELECT count(*) FROM outbox WHERE partition_key=$1)`, portfolio).Scan(&entries, &records); err != nil {
		t.Fatal(err)
	}
	return entries, records
}

func cashTimeStream(t *testing.T, ctx context.Context, js jetstream.JetStream, subject string) jetstream.Stream {
	t.Helper()
	name, err := bustest.StreamFor(ctx, js, subject)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := js.Stream(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	return stream
}

func TestCashPayloadTimestampPostgresRefusalFallbackAndLegacyHistory(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("requires TEST_POSTGRES_URL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	tenant := "cash-time-pg"
	pool := cashTimeRestrictedPool(t, ctx, dsn, tenant)
	st := ledger.NewPostgres(pool)
	at := time.Unix(1700000000, 123456000).UTC()
	pub := &cashTimeCapture{}
	ann := cashTimeAnnouncer(st, pub, at)
	f, err := consume.NewFolder(tenant, st, "USD", consume.WithAnnouncer(ann))
	if err != nil {
		t.Fatal(err)
	}
	ctx = bus.WithCausationID(bus.WithCorrelationID(bus.WithTenantID(ctx, tenant), "cash-time-correlation"), "cash-time-cause")
	env := &envelopepb.Envelope{TenantId: tenant, EventType: cashTimeSubject, IngestionTime: timestamppb.New(at)}
	for n, entry := range malformedCashTimeEntries("TIME", at) {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			if err := f.HandleCash(ctx, env, marshalCashTime(t, entry)); err == nil {
				t.Fatal("malformed cash timestamp was appended")
			}
			entries, records := cashTimeCounts(t, ctx, pool, "TIME")
			if entries != 0 || records != 0 || len(pub.events) != 0 {
				t.Fatal("timestamp refusal left partial journal/outbox/announcement effects")
			}
		})
	}
	valid := cashTimeWireEntry("cash:valid", "TIME", -125, nil, nil)
	for range 2 {
		if err := f.HandleCash(ctx, env, marshalCashTime(t, valid)); err != nil {
			t.Fatal(err)
		}
	}
	journal, err := st.Journal(ctx, "TIME")
	entries, records := cashTimeCounts(t, ctx, pool, "TIME")
	if err != nil || len(journal) != 1 || entries != 1 || records != 1 || len(pub.events) != 1 || !journal[0].Knowledge.Equal(at) || !journal[0].Effective.Equal(at) || journal[0].Cash.Cmp(big.NewRat(-5, 4)) != 0 {
		t.Fatalf("new fallback/redelivery was not exact and idempotent: %+v records=%d announcements=%d error=%v", journal, records, len(pub.events), err)
	}
	// A legacy row may already have the old bug's epoch. Preserve its exact
	// row and pending FACT bytes when corrected fallback is a changed retry.
	epoch := time.Unix(0, 0).UTC()
	old := &ledger.Event{EntryID: "cash:legacy", PortfolioID: "LEGACY", VenueAccountID: "ACCOUNT", Type: ledger.EntryCash, Cash: big.NewRat(1, 1), CashCurrency: "USD", SourceRef: "cash-time-fixture", Effective: epoch, Knowledge: epoch}
	if err := st.Append(ctx, old, func(ctx context.Context, view ledger.Store) ([]outbox.Record, error) {
		return ann.Records(ctx, view, "LEGACY")
	}); err != nil {
		t.Fatal(err)
	}
	readBytes := func() (string, string) {
		var row, records string
		if err := pool.QueryRow(ctx, `SELECT row_to_json(e)::text FROM ledger_entries e WHERE entry_id='cash:legacy'`).Scan(&row); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, `SELECT json_agg(o ORDER BY id)::text FROM outbox o WHERE partition_key='LEGACY'`).Scan(&records); err != nil {
			t.Fatal(err)
		}
		return row, records
	}
	beforeRow, beforeOutbox := readBytes()
	if err := f.HandleCash(ctx, env, marshalCashTime(t, cashTimeWireEntry("cash:legacy", "LEGACY", 100, nil, nil))); !errors.Is(err, ledger.ErrCashConflict) {
		t.Fatalf("legacy first-writer changed retry was not refused: %v", err)
	}
	afterRow, afterOutbox := readBytes()
	if beforeRow != afterRow || beforeOutbox != afterOutbox || len(pub.events) != 1 {
		t.Fatal("legacy effective/knowledge/journal/outbox bytes were rewritten or published")
	}
}

func cashTimeAwaitACK(t *testing.T, ctx context.Context, stream jetstream.Stream, group string, count int) {
	t.Helper()
	names := stream.ConsumerNames(ctx)
	var name string
	for candidate := range names.Name() {
		if strings.HasPrefix(candidate, group+"-") {
			if name != "" {
				t.Fatal("more than one fixture durable for the group")
			}
			name = candidate
		}
	}
	if err := names.Err(); err != nil || name == "" {
		t.Fatalf("fixture durable missing: %v", err)
	}
	c, err := stream.Consumer(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	for {
		info, err := c.Info(ctx)
		if err != nil {
			t.Fatal(err)
		}
		// Other tenants' retained frames are acknowledged by the fixture filter;
		// all fixture frames must also have reached the handler and broker ACK.
		if info.NumPending == 0 && info.NumAckPending == 0 && info.AckFloor.Consumer >= uint64(count) {
			return
		}
		select {
		case <-time.After(10 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal("fixture deliveries were not acknowledged")
		}
	}
}

func TestCashPayloadTimestampJetStreamJournalOutboxReplay(t *testing.T) {
	url, dsn := os.Getenv("TEST_NATS_URL"), os.Getenv("TEST_POSTGRES_URL")
	if url == "" || dsn == "" {
		t.Skip("requires TEST_NATS_URL and TEST_POSTGRES_URL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	id := fmt.Sprintf("cash-time-%d", time.Now().UnixNano())
	tenant, portfolio := id, "PF-"+id
	pool := cashTimeRestrictedPool(t, ctx, dsn, tenant)
	st := ledger.NewPostgres(pool)
	at := time.Unix(1700000000, 123456000).UTC()
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	bustest.EnsureSubjects(t, ctx, js, "CASH_TIME_INPUT", []string{cashSubjectWildcard})
	bustest.EnsureSubjects(t, ctx, js, "CASH_TIME_OUTPUT", []string{consume.SubjectPortfolioCash})
	bustest.EnsureSubjects(t, ctx, js, "CASH_TIME_DLQ", []string{"dlq.>"})
	inputStream := cashTimeStream(t, ctx, js, cashTimeSubject)
	outputStream := cashTimeStream(t, ctx, js, consume.SubjectPortfolioCash)
	dlqStream := cashTimeStream(t, ctx, js, "dlq."+cashTimeSubject)
	output, err := outputStream.CreateConsumer(ctx, jetstream.ConsumerConfig{Name: id + "-cash", FilterSubject: consume.SubjectPortfolioCash, DeliverPolicy: jetstream.DeliverNewPolicy, AckPolicy: jetstream.AckExplicitPolicy})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		for _, stream := range []jetstream.Stream{inputStream, outputStream, dlqStream} {
			names := stream.ConsumerNames(cleanup)
			for name := range names.Name() {
				if strings.HasPrefix(name, id) {
					if err := stream.DeleteConsumer(cleanup, name); err != nil {
						t.Error(err)
					}
				}
			}
			if err := names.Err(); err != nil {
				t.Error(err)
			}
		}
	}()
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: id})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "accounting", ProducerVersion: "cash-time-test", Tenant: tenant})
	if err != nil {
		t.Fatal(err)
	}
	bad := malformedCashTimeEntries(portfolio, at)
	bad = append(bad, cashTimeWireEntry("cash:foreign", portfolio, 10000, timestamppb.New(at), timestamppb.New(at)))
	good := []*accountingpb.LedgerEntry{
		cashTimeWireEntry("cash:both-absent", portfolio, 10000, nil, nil),
		cashTimeWireEntry("cash:effective-absent", portfolio, -125, timestamppb.New(at.Add(-time.Hour)), nil),
		cashTimeWireEntry("cash:knowledge-absent", portfolio, -250, nil, timestamppb.New(at.Add(-2*time.Hour))),
		cashTimeWireEntry("cash:epoch", portfolio, 75, timestamppb.New(time.Unix(0, 0)), timestamppb.New(time.Unix(0, 0))),
		cashTimeWireEntry("cash:zero-policy", portfolio, 100, timestamppb.New(time.Time{}), timestamppb.New(time.Time{})),
	}
	goodTypes := []string{cashTimeSubject, "accounting.cash.fee", "accounting.cash.redemption", cashTimeSubject, cashTimeSubject}
	trace := "00-" + strings.Repeat("a", 32) + "-" + strings.Repeat("b", 16) + "-01"
	publish := func(entry *accountingpb.LedgerEntry, typ string, foreign bool) {
		e := bus.Event{Subject: typ, EventType: typ, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, Domain: "accounting", PartitionKey: portfolio,
			EventTime: at, IngestionTime: at, CorrelationID: id, TraceContext: trace, Payload: entry}
		if foreign {
			e.TenantID = "other-" + tenant
		}
		if err := producer.Publish(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	for pass := range 2 {
		// Recreate the folder/consumer on the same durable store. The second
		// fresh durable replays retained original frames without local dedup.
		folder, err := consume.NewFolder(tenant, st, "USD", consume.WithAnnouncer(cashTimeAnnouncer(st, producer, at)))
		if err != nil {
			t.Fatal(err)
		}
		consumer, err := bus.NewConsumer(client, bus.WithDLQ(client), bus.WithDedupWindow(0, 0))
		if err != nil {
			t.Fatal(err)
		}
		dlq, err := dlqStream.CreateConsumer(ctx, jetstream.ConsumerConfig{Name: fmt.Sprintf("%s-dlq-%d", id, pass), FilterSubject: "dlq." + cashSubjectWildcard, DeliverPolicy: jetstream.DeliverNewPolicy, AckPolicy: jetstream.AckExplicitPolicy})
		if err != nil {
			t.Fatal(err)
		}
		type delivery struct {
			id, eventID string
			err         error
		}
		seen := make(chan delivery, len(bad)+len(good))
		subCtx, stop := context.WithCancel(ctx)
		defer stop()
		ready, done := make(chan struct{}), make(chan error, 1)
		group := fmt.Sprintf("%s-%d", id, pass)
		go func() {
			done <- consumer.SubscribeReady(subCtx, cashSubjectWildcard, group, func(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
				// Model the existing spine fixture's tenant binding while still
				// sending the deliberately foreign tenant to the real refusal.
				if env.GetTenantId() != tenant && env.GetTenantId() != "other-"+tenant {
					return nil
				}
				var entry accountingpb.LedgerEntry
				if err := proto.Unmarshal(payload, &entry); err != nil {
					return err
				}
				err := folder.HandleCash(ctx, env, payload)
				select {
				case seen <- delivery{entry.EntryId, env.GetEventId(), err}:
				case <-ctx.Done():
				}
				return err
			}, func() { close(ready) })
		}()
		select {
		case <-ready:
		case err := <-done:
			t.Fatalf("consumer stopped before ready: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if pass == 0 {
			for n, entry := range bad {
				publish(entry, cashTimeSubject, n == len(bad)-1)
			}
		}
		inputIDs := map[string]string{}
		await := func(entries []*accountingpb.LedgerEntry, refuse bool) {
			for _, want := range entries {
				select {
				case got := <-seen:
					if got.id != want.EntryId || (got.err != nil) != refuse {
						t.Fatalf("pass %d %s: unexpected disposition: %+v", pass, want.EntryId, got)
					}
					inputIDs[got.id] = got.eventID
				case err := <-done:
					t.Fatalf("consumer stopped before delivery: %v", err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
		}
		await(bad, true)
		parked := map[string]bool{}
		for range bad {
			msg, err := dlq.Next(jetstream.FetchMaxWait(5 * time.Second))
			if err != nil {
				t.Fatal(err)
			}
			env, payload, err := bus.Unframe(msg.Data())
			var entry accountingpb.LedgerEntry
			if err != nil || proto.Unmarshal(payload, &entry) != nil || inputIDs[entry.EntryId] != env.GetEventId() || parked[entry.EntryId] || msg.Headers().Get(bus.HeaderDLQOriginalSubject) != strings.TrimPrefix(msg.Subject(), "dlq.") {
				t.Fatal("DLQ lost source identity or duplicated a refusal")
			}
			parked[entry.EntryId] = true
			meta, err := msg.Metadata()
			if err != nil {
				t.Fatal(err)
			}
			retained, err := dlqStream.GetMsg(ctx, meta.Sequence.Stream)
			if err != nil {
				t.Fatal(err)
			}
			retainedEnv, _, err := bus.Unframe(retained.Data)
			if err != nil || retainedEnv.GetEventId() != env.GetEventId() {
				t.Fatal("refusal was not retained by JetStream")
			}
			if err := msg.Ack(); err != nil {
				t.Fatal(err)
			}
		}
		if pass == 0 {
			cashTimeAwaitACK(t, ctx, inputStream, group, len(bad))
			entries, records := cashTimeCounts(t, ctx, pool, portfolio)
			info, err := output.Info(ctx)
			if err != nil || entries != 0 || records != 0 || info.NumPending != 0 {
				t.Fatal("refused/foreign frames produced partial journal/outbox/cash FACT")
			}
			for n, entry := range good {
				publish(entry, goodTypes[n], false)
			}
		}
		await(good, false)
		cashTimeAwaitACK(t, ctx, inputStream, group, len(bad)+len(good))
		stop()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if pass == 0 {
			for n, want := range []*big.Rat{big.NewRat(100, 1), big.NewRat(395, 4), big.NewRat(385, 4), big.NewRat(97, 1), big.NewRat(98, 1)} {
				msg, err := output.Next(jetstream.FetchMaxWait(5 * time.Second))
				if err != nil {
					t.Fatal(err)
				}
				env, payload, err := bus.Unframe(msg.Data())
				var cash accountingpb.PortfolioCashBalance
				if err != nil || bus.Validate(env) != nil || proto.Unmarshal(payload, &cash) != nil {
					t.Fatal("cash announcement failed canonical framing/validation")
				}
				amount, ok := dec.FromProtoChecked(cash.GetTotal())
				if !ok || amount.Cmp(want) != 0 || env.GetTenantId() != tenant || env.GetCausationId() != inputIDs[good[n].EntryId] || env.GetCorrelationId() != id || !cash.GetAsOf().AsTime().Equal(at) {
					t.Fatal("cash FACT lost exact amount/time/tenant/lineage")
				}
				meta, err := msg.Metadata()
				if err != nil {
					t.Fatal(err)
				}
				retained, err := outputStream.GetMsg(ctx, meta.Sequence.Stream)
				if err != nil {
					t.Fatal(err)
				}
				storedEnv, _, err := bus.Unframe(retained.Data)
				if err != nil || storedEnv.GetEventId() != env.GetEventId() {
					t.Fatal("cash announcement was not retained")
				}
				if err := msg.Ack(); err != nil {
					t.Fatal(err)
				}
			}
		} else if info, err := output.Info(ctx); err != nil || info.NumPending != 0 {
			t.Fatal("replay generated duplicate cash FACTs")
		}
		journal, err := st.Journal(bus.WithTenantID(ctx, tenant), portfolio)
		entries, records := cashTimeCounts(t, ctx, pool, portfolio)
		if err != nil || len(journal) != len(good) || entries != len(good) || records != len(good) {
			t.Fatal("replay duplicated or lost journal/outbox rows")
		}
		wantKnown := []time.Time{at, at.Add(-time.Hour), at, time.Unix(0, 0).UTC(), at}
		wantEffective := []time.Time{at, at.Add(-time.Hour), at.Add(-2 * time.Hour), time.Unix(0, 0).UTC(), at}
		for n, frame := range good {
			found := false
			for _, entry := range journal {
				if entry.EntryID == frame.EntryId {
					found = entry.Knowledge.Equal(wantKnown[n]) && entry.Effective.Equal(wantEffective[n]) && entry.Cash.Cmp(big.NewRat(frame.Cash.Coefficient, 100)) == 0
				}
			}
			if !found {
				t.Fatalf("valid fallback/epoch/signed cash changed for %s", frame.EntryId)
			}
		}
		book, _, err := ledger.MaterializeCurrent(bus.WithTenantID(ctx, tenant), st, portfolio)
		if err != nil || book.CashBalance("USD").Cmp(big.NewRat(98, 1)) != 0 {
			t.Fatal("independent signed cash oracle changed")
		}
		t.Logf("pass %d: 10 malformed payloads and one foreign tenant refused/parked; five valid entries and exact cash FACTs retained once", pass)
	}
	other, err := pg.NewTenantPool(ctx, dsn, "other-"+tenant)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if entries, records := cashTimeCounts(t, ctx, other, portfolio); entries != 0 || records != 0 {
		t.Fatal("another tenant discovered journal or cash outbox state")
	}
}
