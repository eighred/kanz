package custody

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/bustest"
	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/fillfact"
	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/consume"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/eighred/kanz/services/accounting/internal/recon"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestBookedFeeCorrectionRecomparesSameSignedStatement(t *testing.T) {
	const tenant, account = "synthetic-custody-pilot", "sandbox-account"
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("requires JetStream")
	}
	pool, _, venue, governance := assignmentTestPools(t, tenant)
	ctx := bus.WithTenantID(t.Context(), tenant)
	attest := signedAccountFixture(t, pool, ctx)
	if err := attest(venue, "venue_identity", nil); err != nil {
		t.Fatal(err)
	}
	if err := attest(governance, "custody_assignment", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO custody_assignment_readers VALUES(session_user,$1)", tenant); err != nil {
		t.Fatal(err)
	}
	store, journal := NewPostgres(pool), ledger.NewPostgres(pool)
	day := time.Date(2020, 2, 4, 0, 0, 0, 0, time.UTC)
	s := Subject{PortfolioID: "PF1", CustodianID: "CUST-A", BusinessDate: day}
	scope, err := NewBookScope([]Subject{s}, map[string]map[string][]string{"PF1": {"CUST-A": {account}}})
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := scope.AdmitAssignments(ctx, day, store)
	if err != nil {
		t.Fatal(err)
	}

	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	for _, subjects := range [][]string{{SubjectRun, SubjectStatement}, {"order.>"}, {consume.SubjectPortfolioCash}} {
		bustest.EnsureSubjects(t, ctx, js, "FEE_"+uuid.NewString(), subjects)
	}
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "booked-fee"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	producer := func(source string) *bus.Producer {
		t.Helper()
		p, err := bus.NewProducer(client, bus.ProducerConfig{Source: source, ProducerVersion: "test", Tenant: tenant})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	last := func(subject, source string) (*envelopepb.Envelope, []byte) {
		t.Helper()
		name, err := js.StreamNameBySubject(ctx, subject)
		if err != nil {
			t.Fatal(err)
		}
		stream, err := js.Stream(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		msg, err := stream.GetLastMsgForSubject(ctx, subject)
		if err != nil {
			t.Fatal(err)
		}
		env, payload, err := bus.Unframe(msg.Data)
		if err != nil || bus.Validate(env) != nil || env.GetSource() != source || env.GetTenantId() != tenant || env.GetEventType() != subject || env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_FACT {
			t.Fatalf("unexpected %s FACT %v: %v", subject, env, err)
		}
		return env, payload
	}

	accounting, oms := producer("accounting"), producer("oms")
	folder := func() *consume.Folder {
		t.Helper()
		a := consume.NewAnnouncer(journal, accounting, "USD", consume.EntrySourcePosture{}, testLogger(), time.Now)
		f, err := consume.NewFolder(tenant, journal, "USD", consume.WithAnnouncer(a))
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	fill := &orderpb.Fill{FillId: "external-7", VenueExecutionId: "external-7", OrderId: "order-external-7", Venue: "XSIM", VenueAccountId: account, InstrumentId: "SYN", Side: orderpb.Side_SIDE_BUY, ExecutedAt: timestamppb.New(day), Quantity: &commonpb.Decimal{Coefficient: 105, Exponent: -1}, Price: &commonpb.Decimal{Coefficient: 1}, Fee: &commonpb.Money{CurrencyCode: "USD", Amount: &commonpb.Decimal{Coefficient: 25, Exponent: -2}}}
	state := &orderpb.OrderState{OrderId: fill.OrderId, PortfolioId: s.PortfolioID, Venue: fill.Venue, VenueAccountId: account, InstrumentId: "SYN", Side: fill.Side, Status: orderpb.OrderStatus_ORDER_STATUS_FILLED}
	publishFill := func(subject string, fact proto.Message) (*envelopepb.Envelope, []byte) {
		t.Helper()
		if err := oms.Publish(ctx, bus.Event{Subject: subject, EventType: subject, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, Domain: "order", PartitionKey: fill.OrderId, EventTime: time.Now(), Payload: fact}); err != nil {
			t.Fatal(err)
		}
		return last(subject, "oms")
	}
	env, payload := publishFill(fillfact.SubjectFilled, &orderpb.OrderFilled{State: state, Fill: fill})
	if err := folder().Handle(ctx, env, payload); err != nil {
		t.Fatal(err)
	}

	read := func(name string) []byte {
		t.Helper()
		v, err := os.ReadFile(filepath.Join("../../cmd/custody_batch/testdata/execution_economics", name))
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	var authority BatchAuthority
	if err := json.Unmarshal(read("authority.json"), &authority); err != nil {
		t.Fatal(err)
	}
	importer, err := NewBatchImporter(authority, tenant, []string{"PF1:CUST-A"}, "PF1:CUST-A:"+account, pool, producer(BatchProducer), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	raw := read("statement-r1.json")
	signedSHA := fmt.Sprintf("%x", sha256.Sum256(raw))
	statement, err := importer.Import(ctx, raw, read("statement-r1.sig"))
	if err != nil {
		t.Fatal(err)
	}
	owner, err := NewSignedBatchConsumer(store, tenant, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	env, payload = last(SubjectStatement, BatchProducer)
	if err := owner.Handle(ctx, env, payload); err != nil {
		t.Fatal(err)
	}
	r, err := NewReconciler(store, LedgerBookLoader(journal, admitted, testLogger()), accounting, nil, nil, testLogger(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	original, err := r.Reconcile(ctx, s)
	if err != nil || original.Outcome != OutcomeClean || original.StatementID != statement || original.Basis.JournalPosition != 1 {
		t.Fatal("original comparison", original, err)
	}
	checkHistoricalEconomics(t, original, false)
	receipt := func(run Run) DatedResult {
		t.Helper()
		got, err := store.HistoricalReceipt(ctx, s, account, run.RunID)
		if err != nil || got.State != "recorded" || !got.AccountAssignmentVerified || got.SignedStatementSHA256 != signedSHA || got.StatementID != statement || got.Run.Outcome != run.Outcome || got.Run.Basis.JournalPosition != run.Basis.JournalPosition || !proto.Equal(got.Run.ExecutionEconomics, run.ExecutionEconomics) {
			t.Fatal("retained comparison", got, err)
		}
		return got
	}
	storedBytes := func(id string) []byte {
		t.Helper()
		var b []byte
		if err := pool.QueryRow(ctx, "SELECT manual_receipt FROM custody_runs WHERE run_id=$1", id).Scan(&b); err != nil || len(b) == 0 {
			t.Fatal("stored comparison bytes", err)
		}
		return b
	}
	receipt(original)
	originalBytes := storedBytes(original.RunID)

	corrected := feeCorrectionFact(t, tenant, fill, state)
	env, payload = publishFill(fillfact.SubjectRecovered, corrected)
	if err := folder().Handle(ctx, env, payload); err != nil {
		t.Fatal(err)
	}
	_, ackBytes := last(fillfact.RecoveryLedgerApplied, "accounting")
	var ack orderpb.ExecutionRecoveryApplied
	if err := proto.Unmarshal(ackBytes, &ack); err != nil || ack.CaseId != corrected.Fill.Recovery.CaseId || ack.ExecutionDigest != corrected.Fill.Recovery.ExecutionDigest || ack.ExecutionKey != fillfact.ExecutionKey(fill) {
		t.Fatal("booked correction acknowledgement", &ack, err)
	}
	pending, err := store.DatedResult(ctx, s)
	if err != nil || pending.State != "pending" || pending.Reason != "journal_changed" || pending.StatementID != statement || pending.Run.RunID != "" {
		t.Fatal("journal invalidation", pending, err)
	}
	receipt(original)
	fresh, err := r.ReconcileManual(ctx, s, uuid.NewString(), "operator:fee-review")
	if err != nil || fresh.RunID == original.RunID || fresh.Outcome != OutcomeBreaks || fresh.StatementID != statement || fresh.Basis.JournalPosition != 2 || len(fresh.Breaks) != 1 {
		t.Fatal("recomparison", fresh, err)
	}
	cash := fresh.Breaks[0]
	if cash.Kind != recon.BreakCash || cash.Key != "USD" || cash.IBOR.Cmp(big.NewRat(-11, 1)) != 0 || cash.Custodian.Cmp(big.NewRat(-43, 4)) != 0 || cash.Diff.Cmp(big.NewRat(-1, 4)) != 0 {
		t.Fatal("cash discrepancy", cash)
	}
	rows := fresh.ExecutionEconomics.GetRows()
	if len(rows) != 1 || rows[0].Status != "equal" || rows[0].EntryId != original.ExecutionEconomics.Rows[0].EntryId {
		t.Fatal("corrected execution economics", rows)
	}
	for _, figures := range []*accountingpb.ExecutionEconomicsFigures{rows[0].Book, rows[0].Custodian} {
		for _, v := range []struct {
			got  *commonpb.Decimal
			want string
		}{{figures.GetQuantity(), "21/2"}, {figures.GetPrice(), "1"}, {figures.GetCommission(), "1/2"}, {figures.GetGrossCash(), "-21/2"}, {figures.GetNetCash(), "-11"}, {figures.GetSettledCash(), "-11"}} {
			if v.got == nil || dec.FromProto(v.got).RatString() != v.want {
				t.Fatalf("corrected economics value %v want %s", v.got, v.want)
			}
		}
	}
	freshBytes := storedBytes(fresh.RunID)
	// A restarted folder sees the same retained OMS envelope. Replay must neither
	// post another fee nor advance the current custody basis.
	if err := folder().Handle(ctx, env, payload); err != nil {
		t.Fatal(err)
	}
	current, err := store.DatedResult(ctx, s)
	if err != nil || current.State != "recorded" || current.Run.RunID != fresh.RunID || !proto.Equal(current.Run.ExecutionEconomics, fresh.ExecutionEconomics) {
		t.Fatal("latest after replay", current, err)
	}
	receipt(original)
	receipt(fresh)
	bookPrior, err := store.PreviousBookComparison(ctx, s, account, fresh.RunID)
	if err != nil || bookPrior.State != "recorded" || !bookPrior.AccountAssignmentVerified || bookPrior.StatementID != statement ||
		bookPrior.SignedStatementSHA256 != signedSHA || bookPrior.Run.RunID != original.RunID || bookPrior.Run.Basis.JournalPosition != 1 ||
		bookPrior.Run.Outcome != OutcomeClean || !proto.Equal(bookPrior.Run.ExecutionEconomics, original.ExecutionEconomics) {
		t.Fatal("prior comparison of the same signed statement", bookPrior, err)
	}
	checkHistoricalEconomics(t, bookPrior.Run, false)
	sourcePrior, err := store.PreviousComparison(ctx, s, account, fresh.RunID)
	if err != nil || sourcePrior.State != "unknown" || sourcePrior.StatementID != "" || sourcePrior.Run.RunID != "" {
		t.Fatal("revision one has no prior signed statement", sourcePrior, err)
	}
	if !bytes.Equal(originalBytes, storedBytes(original.RunID)) || !bytes.Equal(freshBytes, storedBytes(fresh.RunID)) {
		t.Fatal("retained comparison bytes changed")
	}
	var receipts, entries, revisions, runs int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM custody_batch_receipts),(SELECT count(*) FROM ledger_entries),(SELECT count(*) FROM execution_fee_revisions WHERE book='ledger'),(SELECT count(*) FROM custody_runs)`).Scan(&receipts, &entries, &revisions, &runs); err != nil || receipts != 1 || entries != 2 || revisions != 1 || runs != 2 {
		t.Fatalf("receipts %d entries %d fee revisions %d runs %d: %v", receipts, entries, revisions, runs, err)
	}
}

// Model the typed OMS output at the Accounting service boundary. The source
// history, reviewed terms, prior economic head and revised execution each have
// independent content identities.
func feeCorrectionFact(t *testing.T, tenant string, previous *orderpb.Fill, state *orderpb.OrderState) *orderpb.ExecutionRecovered {
	t.Helper()
	revised := proto.Clone(previous).(*orderpb.Fill)
	revised.Fee.Amount = &commonpb.Decimal{Coefficient: 50, Exponent: -2}
	source := &orderpb.ExecutionRecoveryHistory{OrderId: previous.OrderId, MappingVersion: "xsim-fee-v1", Fills: []*orderpb.Fill{revised}, ExecutedQuantity: revised.Quantity}
	history, err := proto.MarshalOptions{Deterministic: true}.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	p := &orderpb.ExecutionFeeCorrectionProposed{CaseId: "synthetic-fee-correction", OrderId: previous.OrderId, PortfolioId: state.PortfolioId, MappingVersion: source.MappingVersion, HistoryDigest: fmt.Sprintf("%x", sha256.Sum256(history)), Proposer: "user:maker", ProposedAt: timestamppb.New(now.Add(-time.Minute)), ExpiresAt: timestamppb.New(now.Add(time.Hour)), Changes: []*orderpb.ExecutionFeeChange{{Previous: previous, Revised: revised}}, Reason: "independently revised execution commission"}
	proposalDigest, err := fillfact.FeeProposalDigest(tenant, p)
	if err != nil {
		t.Fatal(err)
	}
	priorDigest, err := fillfact.EconomicDigest(previous)
	if err != nil {
		t.Fatal(err)
	}
	revisedDigest, err := fillfact.ExecutionDigest(revised)
	if err != nil {
		t.Fatal(err)
	}
	revised.Recovery = &orderpb.ExecutionRecoveryProvenance{CaseId: p.CaseId, MappingVersion: p.MappingVersion, SourceCursor: "synthetic-history-1", PayloadDigest: p.HistoryDigest, ExecutionDigest: revisedDigest, ObservedAt: timestamppb.New(now), FeeApproval: &orderpb.ExecutionFeeApproval{ProposalId: p.CaseId, Digest: proposalDigest, Proposer: p.Proposer, Approver: "user:checker", ApprovedAt: timestamppb.New(now), PreviousExecutionDigest: priorDigest, PreviousFee: previous.Fee}}
	return &orderpb.ExecutionRecovered{State: state, Fill: revised}
}
