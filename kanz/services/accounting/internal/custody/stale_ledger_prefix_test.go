package custody

import (
	"context"
	"errors"
	"math/big"
	"os"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/bustest"
	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/eighred/kanz/services/accounting/internal/recon"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/proto"
)

func TestSignedDatedCloseRefusesBackdatedBookingAfterFold(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("requires JetStream")
	}
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	bustest.EnsureSubjects(t, t.Context(), js, "PREFIX_"+uuid.NewString(), []string{SubjectRun})
	streamName, err := js.StreamNameBySubject(t.Context(), SubjectRun)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := js.Stream(t.Context(), streamName)
	if err != nil {
		t.Fatal(err)
	}

	for _, mode := range []string{"scheduled", "manual"} {
		t.Run(mode, func(t *testing.T) {
			tenant := "signed-prefix-" + mode
			ctx := bus.WithTenantID(t.Context(), tenant)
			pool := newCustodyPool(t, tenant)
			requireRestrictedCustodyRole(t, pool)
			applyCustodySchema(t, pool)
			store, journal := NewPostgres(pool), ledger.NewPostgres(pool)
			s := subject()
			s.BusinessDate = BusinessDay(s.BusinessDate)
			signed := manualSignedFeed(t, ctx, store)
			appendCash := func(id string, amount int64, effective time.Time) {
				t.Helper()
				e := &ledger.Event{EntryID: id, PortfolioID: s.PortfolioID, VenueAccountID: "ACC1", Type: ledger.EntryCash,
					CashCurrency: "USD", Cash: big.NewRat(amount, 1), Effective: effective, Knowledge: time.Now().UTC()}
				if err := journal.Append(ctx, e, nil); err != nil {
					t.Fatal(err)
				}
			}
			// Cover the first ledger append after an empty fold as well as a
			// backdated append on an existing, nonzero portfolio prefix.
			opening, prefix := int64(0), int64(0)
			if mode == "scheduled" {
				opening, prefix = 100, 1
				appendCash("opening", opening, t0)
			}
			first := signed(0, int(opening+10))
			client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: tenant})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = client.Close() }()
			producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "accounting", ProducerVersion: "test", Tenant: tenant})
			if err != nil {
				t.Fatal(err)
			}
			reg := prometheus.NewRegistry()
			metrics := NewMetrics(reg)
			now := time.Now().UTC().Truncate(time.Microsecond)
			writer := func(load BookLoader) *Reconciler {
				t.Helper()
				r, err := NewReconciler(store, load, producer, nil, metrics, testLogger(), func() time.Time { return now })
				if err != nil {
					t.Fatal(err)
				}
				return r
			}
			load := LedgerBookLoader(journal, closeScope(t, true), testLogger())
			prior, err := writer(load).Reconcile(ctx, s)
			if err != nil || prior.StatementID != first || prior.Outcome != OutcomeBreaks || prior.Basis.JournalPosition != prefix || len(prior.Breaks) != 1 {
				t.Fatal("original comparison", prior, err)
			}
			old := prior.Breaks[0]
			if old.Kind != recon.BreakCash || old.Key != "USD" || old.Diff.Cmp(big.NewRat(-10, 1)) != 0 {
				t.Fatal("original cash discrepancy", old)
			}
			corrected := signed(0, int(opening))
			last := func() *jetstream.RawStreamMsg {
				t.Helper()
				msg, err := stream.GetLastMsgForSubject(ctx, SubjectRun)
				if err != nil {
					t.Fatal(err)
				}
				return msg
			}
			before := last().Sequence
			now = now.Add(time.Second)
			request := uuid.NewString()
			compare := func(r *Reconciler) (Run, error) {
				if mode == "manual" {
					return r.ReconcileManual(ctx, s, request, "operator:cash")
				}
				return r.Reconcile(ctx, s)
			}
			// The actual loader has returned its book and prefix. Complete the
			// competing journal transaction before the run may take its lock.
			appendAfterFold := func(id string, amount int64, at time.Time, want int64) BookLoader {
				return func(ctx context.Context, s Subject) (*ledger.Book, []ledger.Execution, *RunBasis, error) {
					book, trades, basis, err := load(ctx, s)
					if err != nil {
						t.Fatal(err)
					}
					if basis.JournalPosition != want {
						t.Fatalf("fold position %d want %d", basis.JournalPosition, want)
					}
					appendCash(id, amount, at)
					return book, trades, basis, nil
				}
			}
			stale, err := compare(writer(appendAfterFold("late-in-day", 5, t0.Add(time.Hour), prefix)))
			if !errors.Is(err, ErrBookBasisChanged) || stale.RunID != "" {
				t.Fatalf("backdated booking committed stale comparison: run=%q outcome=%s err=%v", stale.RunID, stale.Outcome, err)
			}
			live, err := store.LoadBreak(ctx, old.BreakID)
			if err != nil || live.Status != BreakOpen || live.Revision != old.Revision || !live.LastSeenAt.Equal(old.LastSeenAt) || live.Diff.Cmp(big.NewRat(-10, 1)) != 0 {
				t.Fatal("prior discrepancy changed", live, err)
			}
			var runs, completions, claimed int
			err = pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM custody_runs),
				(SELECT count(*) FROM outbox WHERE event_type=$1),
				(SELECT count(*) FROM custody_runs WHERE run_id=$2)`, SubjectRun, "manual:"+request).Scan(&runs, &completions, &claimed)
			if err != nil || runs != 1 || completions != 1 || claimed != 0 || last().Sequence != before {
				t.Fatal("refusal persisted or published a completion", runs, completions, claimed, err)
			}
			freshness, ok := gaugeValue(t, reg, "kanz_accounting_reconciliation_last_success_timestamp_seconds", map[string]string{"portfolio": s.PortfolioID, "custodian": s.CustodianID})
			if !ok || freshness != float64(prior.CompletedAt.Unix()) {
				t.Fatal("stale comparison refreshed the close", freshness)
			}
			// A simple cash append at the next UTC midnight has no economic
			// effect on D. Retrying with the same request can use prefix p+1.
			now = now.Add(time.Second)
			fresh, err := compare(writer(appendAfterFold("future-cash", 200, s.BusinessDate.AddDate(0, 0, 1), prefix+1)))
			if err != nil || fresh.Outcome != OutcomeBreaks || fresh.StatementID != corrected || fresh.Basis.JournalPosition != prefix+1 || len(fresh.Breaks) != 1 {
				t.Fatal("fresh comparison", fresh, err)
			}
			b := fresh.Breaks[0]
			if b.BreakID != old.BreakID || b.Status != BreakOpen || b.IBOR.Cmp(big.NewRat(opening+5, 1)) != 0 || b.Custodian.Cmp(big.NewRat(opening, 1)) != 0 || b.Diff.Cmp(big.NewRat(5, 1)) != 0 {
				t.Fatal("fresh signed cash discrepancy", b)
			}
			msg := last()
			env, payload, err := bus.Unframe(msg.Data)
			var fact accountingpb.ReconciliationRun
			if err != nil || msg.Sequence <= before || env.GetTenantId() != tenant || env.GetCorrelationId() != fresh.RunID || proto.Unmarshal(payload, &fact) != nil || fact.GetRunId() != fresh.RunID || fact.GetStatementId() != corrected || len(fact.GetBreaks()) != 1 {
				t.Fatal("retained fresh completion", err)
			}
			if mode == "manual" && (env.GetCausationId() != request || fact.GetRequestId() != request) {
				t.Fatal("retry lost original request")
			}
		})
	}
}
