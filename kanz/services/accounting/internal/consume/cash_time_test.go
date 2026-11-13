package consume

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var cashTimeAt = time.Unix(1700000000, 123456789).UTC()

func cashTimeEntry() *accountingpb.LedgerEntry {
	return &accountingpb.LedgerEntry{EntryId: "cash:time", PortfolioId: "TIME", VenueAccountId: "ACCOUNT", Cash: decv(-125, -2), CashCurrency: "USD", SourceRef: "time-source",
		KnowledgeTime: timestamppb.New(cashTimeAt), EffectiveTime: timestamppb.New(cashTimeAt)}
}

func cashTimeBytes(t *testing.T, entry *accountingpb.LedgerEntry) []byte {
	t.Helper()
	payload, err := proto.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func badCashTimes() map[string]*timestamppb.Timestamp {
	return map[string]*timestamppb.Timestamp{
		"negative one nanos": {Seconds: cashTimeAt.Unix(), Nanos: -1},
		"negative nanos":     {Seconds: cashTimeAt.Unix() + 1, Nanos: -1000000000},
		"overflow nanos":     {Seconds: cashTimeAt.Unix(), Nanos: 1000000000},
		"before year one":    {Seconds: -62135596801},
		"after year 9999":    {Seconds: 253402300800},
	}
}

type cashTimeStore struct {
	*ledger.MemoryStore
	appends int
}

func (s *cashTimeStore) Append(ctx context.Context, entry *ledger.Event, announce ledger.Announcer) error {
	s.appends++
	return s.MemoryStore.Append(ctx, entry, announce)
}

func TestCashPayloadTimestampRefusesBeforeAppend(t *testing.T) {
	for _, field := range []string{"knowledge", "effective"} {
		for name, stamp := range badCashTimes() {
			for _, typ := range []string{cashEventSubscription, cashEventRedemption, cashEventFee} {
				t.Run(field+"/"+name+"/"+typ, func(t *testing.T) {
					entry := cashTimeEntry()
					if field == "knowledge" {
						entry.KnowledgeTime = stamp
					} else {
						entry.EffectiveTime = stamp
					}
					st := &cashTimeStore{MemoryStore: ledger.NewMemoryStore()}
					pub := &capturingPublisher{}
					ann := NewAnnouncer(st, pub, "USD", testPosture, nil, func() time.Time { return cashTimeAt.Add(time.Hour) })
					f, err := NewFolder("acme", st, "USD", WithAnnouncer(ann))
					if err != nil {
						t.Fatal(err)
					}
					env := &envelopepb.Envelope{TenantId: "acme", EventType: typ, IngestionTime: timestamppb.New(cashTimeAt.Add(time.Minute))}
					if err := f.HandleCash(foldCtx(), env, cashTimeBytes(t, entry)); err == nil {
						t.Fatal("malformed present payload time was accepted")
					}
					journal, err := st.Journal(foldCtx(), "TIME")
					if err != nil {
						t.Fatal(err)
					}
					pending, err := st.Outbox().Pending(foldCtx(), "TIME", 10)
					if err != nil || st.appends != 0 || len(journal) != 0 || len(pending) != 0 || len(pub.events) != 0 {
						t.Fatalf("refusal produced effects: appends=%d journal=%d outbox=%d published=%d err=%v", st.appends, len(journal), len(pending), len(pub.events), err)
					}
				})
			}
		}
	}
}

func TestCashPayloadTimestampFallbackEpochAndPrecision(t *testing.T) {
	envAt := cashTimeAt.Add(time.Hour)
	epoch := time.Unix(0, 0).UTC()
	zero := timestamppb.New(time.Time{})
	for _, tc := range []struct {
		name         string
		known, eff   *timestamppb.Timestamp
		wantK, wantE time.Time
	}{
		{"both absent", nil, nil, envAt, envAt},
		{"knowledge absent", nil, timestamppb.New(cashTimeAt), envAt, cashTimeAt},
		{"effective absent", timestamppb.New(cashTimeAt), nil, cashTimeAt, cashTimeAt},
		{"explicit epoch", timestamppb.New(epoch), timestamppb.New(epoch), epoch, epoch},
		{"epoch knowledge with absent effective", timestamppb.New(epoch), nil, epoch, epoch},
		{"explicit zero legacy fallback", zero, zero, envAt, envAt},
		{"zero effective legacy fallback", timestamppb.New(cashTimeAt), zero, cashTimeAt, cashTimeAt},
		{"nanoseconds retained", timestamppb.New(cashTimeAt), timestamppb.New(cashTimeAt.Add(-time.Nanosecond)), cashTimeAt, cashTimeAt.Add(-time.Nanosecond)},
		{"valid negative-nanos counterpart", timestamppb.New(time.Unix(cashTimeAt.Unix()-1, 999999999)), timestamppb.New(time.Unix(cashTimeAt.Unix()-1, 999999999)), time.Unix(cashTimeAt.Unix()-1, 999999999), time.Unix(cashTimeAt.Unix()-1, 999999999)},
		{"valid billion-nanos counterpart", timestamppb.New(time.Unix(cashTimeAt.Unix()+1, 0)), timestamppb.New(time.Unix(cashTimeAt.Unix()+1, 0)), time.Unix(cashTimeAt.Unix()+1, 0), time.Unix(cashTimeAt.Unix()+1, 0)},
		{"minimum nonzero", &timestamppb.Timestamp{Seconds: -62135596800, Nanos: 1}, &timestamppb.Timestamp{Seconds: -62135596800, Nanos: 1}, time.Unix(-62135596800, 1).UTC(), time.Unix(-62135596800, 1).UTC()},
		{"maximum", &timestamppb.Timestamp{Seconds: 253402300799, Nanos: 999999999}, &timestamppb.Timestamp{Seconds: 253402300799, Nanos: 999999999}, time.Unix(253402300799, 999999999).UTC(), time.Unix(253402300799, 999999999).UTC()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, typ := range []string{cashEventSubscription, cashEventRedemption, cashEventFee} {
				entry := cashTimeEntry()
				entry.KnowledgeTime, entry.EffectiveTime = tc.known, tc.eff
				got, err := decodeCash(typ, cashTimeBytes(t, entry), envAt)
				if err != nil || got == nil || !got.Knowledge.Equal(tc.wantK) || !got.Effective.Equal(tc.wantE) {
					t.Fatalf("%s: decoded=%+v error=%v, want knowledge=%v effective=%v", typ, got, err, tc.wantK, tc.wantE)
				}
				wantType := ledger.EntryCash
				if typ == cashEventFee {
					wantType = ledger.EntryFee
				}
				if got.Type != wantType || got.Cash.Cmp(big.NewRat(-5, 4)) != 0 || got.CashCurrency != "USD" || got.VenueAccountID != "ACCOUNT" || got.SourceRef != "time-source" {
					t.Fatalf("timestamp handling changed signed cash/classification/attribution: %+v", got)
				}
			}
		})
	}
}

func TestCashPayloadTimestampFallbackFoldIsIdempotentAndTenantScoped(t *testing.T) {
	st := &cashTimeStore{MemoryStore: ledger.NewMemoryStore()}
	pub := &capturingPublisher{}
	ann := NewAnnouncer(st, pub, "USD", testPosture, nil, func() time.Time { return cashTimeAt.Add(time.Hour) })
	f, err := NewFolder("acme", st, "USD", WithAnnouncer(ann))
	if err != nil {
		t.Fatal(err)
	}
	entry := cashTimeEntry()
	entry.KnowledgeTime, entry.EffectiveTime = nil, nil
	payload := cashTimeBytes(t, entry)
	env := &envelopepb.Envelope{TenantId: "other", EventType: cashEventSubscription, IngestionTime: timestamppb.New(cashTimeAt)}
	if err := f.HandleCash(foldCtx(), env, payload); err == nil || st.appends != 0 {
		t.Fatal("another tenant's cash reached Append")
	}
	env.TenantId = "acme"
	for range 2 {
		if err := f.HandleCash(foldCtx(), env, payload); err != nil {
			t.Fatal(err)
		}
	}
	journal, err := st.Journal(foldCtx(), "TIME")
	if err != nil || len(journal) != 1 || !journal[0].Knowledge.Equal(cashTimeAt) || !journal[0].Effective.Equal(cashTimeAt) || journal[0].Cash.Cmp(big.NewRat(-5, 4)) != 0 || len(pub.events) != 1 {
		t.Fatalf("fallback/replay changed the immutable cash leg: %+v announcements=%d error=%v", journal, len(pub.events), err)
	}
}

func TestCashPayloadTimestampDoesNotRewriteLegacyEpoch(t *testing.T) {
	st := ledger.NewMemoryStore()
	epoch := time.Unix(0, 0).UTC()
	old := &ledger.Event{EntryID: "cash:time", PortfolioID: "TIME", VenueAccountID: "ACCOUNT", Type: ledger.EntryCash, Cash: big.NewRat(-5, 4), CashCurrency: "USD", SourceRef: "time-source", Effective: epoch, Knowledge: epoch}
	if err := st.Append(foldCtx(), old, nil); err != nil {
		t.Fatal(err)
	}
	pub := &capturingPublisher{}
	ann := NewAnnouncer(st, pub, "USD", testPosture, nil, func() time.Time { return cashTimeAt })
	f, err := NewFolder("acme", st, "USD", WithAnnouncer(ann))
	if err != nil {
		t.Fatal(err)
	}
	entry := cashTimeEntry()
	entry.KnowledgeTime, entry.EffectiveTime = nil, nil
	env := &envelopepb.Envelope{TenantId: "acme", EventType: cashEventSubscription, IngestionTime: timestamppb.New(cashTimeAt)}
	if err := f.HandleCash(foldCtx(), env, cashTimeBytes(t, entry)); !errors.Is(err, ledger.ErrCashConflict) {
		t.Fatalf("changed retry must preserve first-writer history: %v", err)
	}
	journal, err := st.Journal(foldCtx(), "TIME")
	if err != nil || len(journal) != 1 || !journal[0].Effective.Equal(epoch) || !journal[0].Knowledge.Equal(epoch) || len(pub.events) != 0 {
		t.Fatal("legacy epoch was rewritten or reannounced")
	}
}
