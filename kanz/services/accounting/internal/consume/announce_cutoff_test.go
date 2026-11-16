package consume

import (
	"context"
	"errors"
	"math/big"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/outbox"
	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

type cashJournalReads struct {
	ledger.Store
	reads int
}

func (s *cashJournalReads) Journal(ctx context.Context, portfolio string) ([]*ledger.Event, error) {
	s.reads++
	return s.Store.Journal(ctx, portfolio)
}

func (s *cashJournalReads) LoadSnapshot(context.Context, string) (*ledger.Snapshot, error) {
	return nil, errors.New("cash announcement must use its single journal view")
}

func TestCashAnnouncementUsesOneEffectiveCutoff(t *testing.T) {
	at := time.Unix(1_700_000_000, 0).UTC()
	for _, tc := range []struct {
		name      string
		currency  string
		amount    *big.Rat
		effective time.Time
		knowledge time.Time
		total     string
		byAsset   map[string]string
		excluded  []string
	}{
		{"tomorrow deposit", "USD", big.NewRat(25, 1), at.Add(24 * time.Hour), at, "100", map[string]string{"USD": "100"}, nil},
		{"tomorrow withdrawal", "USD", big.NewRat(-25, 1), at.Add(24 * time.Hour), at, "100", map[string]string{"USD": "100"}, nil},
		{"cutoff equality", "USD", big.NewRat(25, 1), at, at, "125", map[string]string{"USD": "125"}, nil},
		{"future foreign asset", "EUR", big.NewRat(25, 1), at.Add(time.Microsecond), at, "100", map[string]string{"USD": "100"}, nil},
		{"future foreign liability", "EUR", big.NewRat(-25, 1), at.Add(time.Microsecond), at, "100", map[string]string{"USD": "100"}, nil},
		// The transaction learned the correction after its economic effective
		// time, by the captured announcement clock.
		{"late correction", "USD", big.NewRat(-25, 1), at.Add(-24 * time.Hour), at, "75", map[string]string{"USD": "75"}, nil},
		{"signed negative level", "USD", big.NewRat(-125, 1), at, at, "-25", map[string]string{"USD": "-25"}, nil},
		{"current foreign liability", "EUR", big.NewRat(-25, 1), at, at, "100", map[string]string{"USD": "100", "EUR": "-25"}, []string{"EUR"}},
		{"future unrepresentable amount", "EUR", big.NewRat(1, 3), at.Add(time.Microsecond), at, "100", map[string]string{"USD": "100"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := ledger.NewMemoryStore()
			ctx := bus.WithTraceContext(bus.WithCausationID(bus.WithCorrelationID(foldCtx(), "corr"), "cause"), "trace")
			if err := st.Append(ctx, cashEntry("today", "PF1", "ACC1", "USD", 100), nil); err != nil {
				t.Fatal(err)
			}
			clockReads := 0
			pub := &capturingPublisher{}
			// Deliberately different from the callback store. Reading a.store
			// would lose both the committed 100 and the uncommitted entry.
			a := NewAnnouncer(ledger.NewMemoryStore(), pub, "USD", testPosture, nil, func() time.Time {
				clockReads++
				return at.Add(time.Duration(clockReads-1) * 24 * time.Hour)
			})
			entry := cashEntry("next", "PF1", "ACC1", tc.currency, 0)
			entry.Cash, entry.Effective, entry.Knowledge = tc.amount, tc.effective, tc.knowledge
			reads := &cashJournalReads{}
			err := st.Append(ctx, entry, func(ctx context.Context, view ledger.Store) ([]outbox.Record, error) {
				reads.Store = view
				return a.Records(ctx, reads, "PF1")
			})
			if err != nil {
				t.Fatalf("append with actual announcer: %v", err)
			}
			if clockReads != 1 || reads.reads != 1 {
				t.Fatalf("clock reads=%d, journal reads=%d; want one of each", clockReads, reads.reads)
			}
			pending, err := st.Outbox().Pending(ctx, "PF1", 10)
			if err != nil || len(pending) != 1 {
				t.Fatalf("pending=%v, err=%v; want one atomic FACT", pending, err)
			}
			rec := pending[0].Record
			if !rec.EventTime.Equal(at) || rec.TenantID != "acme" || rec.CorrelationID != "corr" || rec.CausationID != "cause" || rec.TraceContext != "trace" || rec.PartitionKey != "PF1" {
				t.Fatalf("FACT time, tenant or lineage changed: %+v", rec)
			}
			event, err := rec.Event()
			if err != nil {
				t.Fatal(err)
			}
			msg := event.Payload.(*accountingpb.PortfolioCashBalance)
			if !msg.GetAsOf().AsTime().Equal(at) || !msg.GetKnowledgeTime().AsTime().Equal(at) {
				t.Fatalf("FACT cutoff differs from captured clock: %v", msg)
			}
			if got := dec.FromProto(msg.GetTotal()).RatString(); got != tc.total {
				t.Fatalf("total=%s, want %s", got, tc.total)
			}
			got := map[string]string{}
			for _, row := range msg.GetByVenueAccount() {
				if row.GetVenueAccountId() != "ACC1" {
					t.Fatalf("unexpected account: %v", row)
				}
				got[row.GetAsset()] = dec.FromProto(row.GetAmount()).RatString()
			}
			if !reflect.DeepEqual(got, tc.byAsset) {
				t.Fatalf("account cash=%v, want %v at the same cutoff", got, tc.byAsset)
			}
			coverage := msg.GetCurrencyCoverage()
			if coverage.GetContributed() != 1 || coverage.GetExcludedCount() != uint32(len(tc.excluded)) || !reflect.DeepEqual(msg.GetExcludedCurrencies(), tc.excluded) {
				t.Fatalf("currency coverage=%v, excluded=%v; want base plus %v", coverage, msg.GetExcludedCurrencies(), tc.excluded)
			}
			if len(pub.events) != 0 {
				t.Fatal("callback published before commit instead of enqueuing")
			}
		})
	}
}

func TestCashAnnouncementRefusalRollsBackTheActualAppend(t *testing.T) {
	for _, tc := range []struct {
		name, currency, account, want string
		ctx                           context.Context
		amount                        *big.Rat
	}{
		{"total", "USD", "", "cash balance is not representable", foldCtx(), big.NewRat(1, 3)},
		{"account", "EUR", "ACC1", "EUR balance in ACC1 is not representable", foldCtx(), big.NewRat(1, 3)},
		{"missing tenant", "USD", "ACC1", "tenant", context.Background(), big.NewRat(25, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := ledger.NewMemoryStore()
			if err := st.Append(foldCtx(), cashEntry("today", "PF1", "ACC1", "USD", 100), nil); err != nil {
				t.Fatal(err)
			}
			a := NewAnnouncer(st, &capturingPublisher{}, "USD", testPosture, nil, func() time.Time { return time.Unix(1_700_000_000, 0) })
			entry := cashEntry("refused", "PF1", tc.account, tc.currency, 0)
			entry.Cash = tc.amount
			err := st.Append(tc.ctx, entry, func(ctx context.Context, view ledger.Store) ([]outbox.Record, error) {
				return a.Records(ctx, view, "PF1")
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("append=%v, want %q refusal", err, tc.want)
			}
			entries, err := st.Journal(foldCtx(), "PF1")
			if err != nil || len(entries) != 1 {
				t.Fatalf("refused entry committed: journal=%v err=%v", entries, err)
			}
			pending, err := st.Outbox().Pending(foldCtx(), "PF1", 10)
			if err != nil || len(pending) != 0 {
				t.Fatalf("refused FACT committed: pending=%v err=%v", pending, err)
			}
		})
	}
}
