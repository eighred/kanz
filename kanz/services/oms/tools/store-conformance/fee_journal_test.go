package storeconformance

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/fillfact"
	"github.com/eighred/kanz/internal/outbox"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/oms/internal/order"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func revision(t *testing.T, previous *orderpb.Fill, id string, amount *commonpb.Decimal) *orderpb.Fill {
	t.Helper()
	digest, e := fillfact.EconomicDigest(previous)
	if e != nil {
		t.Fatal(e)
	}
	next := proto.Clone(previous).(*orderpb.Fill)
	next.Fee.Amount = proto.Clone(amount).(*commonpb.Decimal)
	next.Recovery = &orderpb.ExecutionRecoveryProvenance{CaseId: id, FeeApproval: &orderpb.ExecutionFeeApproval{ProposalId: id, Digest: strings.Repeat("a", 64), Proposer: "maker", Approver: "checker", ApprovedAt: timestamppb.New(at), PreviousExecutionDigest: digest, PreviousFee: proto.Clone(previous.Fee).(*commonpb.Money)}}
	return next
}
func independentEconomicDigest(parts ...string) string {
	h := sha256.New()
	var n [8]byte
	for _, part := range parts {
		binary.BigEndian.PutUint64(n[:], uint64(len(part)))
		h.Write(n[:])
		h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}

type feeDigestCase struct {
	name   string
	fill   *orderpb.Fill
	digest string
}

func feeDigestCases(t *testing.T) []feeDigestCase {
	t.Helper()
	identity := sha256.Sum256([]byte(`["VENUE","account","instrument","execution"]`))
	key := "execution:v1:" + hex.EncodeToString(identity[:])
	var result []feeDigestCase
	for _, date := range []string{"1970-01-01T00:00:01Z", "2000-02-29T12:34:56.123Z", "2100-03-01T00:00:00.000000001Z", "2400-02-29T23:59:59.12Z", "9999-12-31T23:59:59.999999999Z"} {
		eventTime, e := time.Parse(time.RFC3339Nano, date)
		if e != nil {
			t.Fatal(e)
		}
		f := fill()
		f.ExecutedAt = timestamppb.New(eventTime)
		f.Quantity = &commonpb.Decimal{Coefficient: 125, Exponent: -3}
		f.Price = &commonpb.Decimal{Coefficient: 10005, Exponent: -2}
		f.Fee.Amount = &commonpb.Decimal{Coefficient: -123, Exponent: -2}
		want := independentEconomicDigest("execution-economics-v1", "order", key, "SIDE_BUY", "1/8", "2001/20", "-123/100", "USD", date)
		got, e := fillfact.EconomicDigest(f)
		if e != nil || got != want {
			t.Fatal("independent rational/time digest oracle differs from Go")
		}
		result = append(result, feeDigestCase{date, f, want})
	}
	return result
}
func TestFeeEconomicIndependentOracle(t *testing.T) { _ = feeDigestCases(t) }
func TestRustFeeEconomicCompatibility(t *testing.T) {
	fixtureBinary(t)
	for _, tc := range feeDigestCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			expect(t, run(t, map[string]any{"mode": "fee-digest", "fill": wire(t, tc.fill)}), tc.digest)
		})
	}
	for _, tc := range []struct {
		name     string
		old, new *commonpb.Decimal
		delta    string
	}{
		{"fractional-refund", &commonpb.Decimal{Coefficient: 1}, &commonpb.Decimal{Coefficient: 25, Exponent: -2}, "3/4"},
		{"additional-debit", &commonpb.Decimal{Coefficient: 1}, &commonpb.Decimal{Coefficient: 2}, "-1"},
		{"rebate-change", &commonpb.Decimal{Coefficient: -25, Exponent: -2}, &commonpb.Decimal{Coefficient: -5, Exponent: -1}, "1/4"},
		{"scale-alias", &commonpb.Decimal{Coefficient: 100, Exponent: -2}, &commonpb.Decimal{Coefficient: 50, Exponent: -2}, "1/2"},
		{"beyond-int64-delta", &commonpb.Decimal{Coefficient: 9223372036854775807}, &commonpb.Decimal{Coefficient: -9223372036854775808}, "18446744073709551615"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := fill()
			original.Fee.Amount = tc.old
			revised := revision(t, original, "revision", tc.new)
			delta, e := fillfact.ApprovedFeeDelta(original, revised)
			if e != nil || delta.RatString() != tc.delta {
				t.Fatal("independent exact delta differs from Go")
			}
			r := runResult(t, map[string]any{"mode": "fee-delta", "fill": wire(t, revised), "other": wire(t, original)})
			if r.Status != "delta" || r.Delta == nil || *r.Delta != tc.delta {
				t.Fatal("Rust exact unquantized delta differs")
			}
		})
	}
}
func TestRustFeeTermsCompatibility(t *testing.T) {
	fixtureBinary(t)
	original := fill()
	original.Fee.ProtoReflect().SetUnknown(varField(1000, 7))
	original.Fee.Amount.ProtoReflect().SetUnknown(msgField(1001, []byte("fee-unknown")))
	revised := revision(t, original, "revision", &commonpb.Decimal{Coefficient: 5, Exponent: -1})
	revised.ProtoReflect().SetUnknown(varField(1002, 8))
	previous, e := fillfact.FeeRevisionTerms(revised)
	if e != nil {
		t.Fatal(e)
	}
	r := runResult(t, map[string]any{"mode": "fee-terms", "fill": wire(t, revised)})
	if r.Status != "terms" || !bytes.Equal(r.State, wire(t, previous)) {
		t.Fatal("previous-fee clone/unknown fields differ")
	}
	for _, tc := range []struct {
		name   string
		change func(*orderpb.Fill)
	}{
		{"same-actor", func(f *orderpb.Fill) {
			f.Recovery.FeeApproval.Proposer = "\u0130"
			f.Recovery.FeeApproval.Approver = " i "
		}},
		{"blank-actor", func(f *orderpb.Fill) { f.Recovery.FeeApproval.Approver = "\u2003" }},
		{"case-binding", func(f *orderpb.Fill) { f.Recovery.CaseId = "other" }},
		{"malformed-digest", func(f *orderpb.Fill) { f.Recovery.FeeApproval.Digest = "z" + strings.Repeat("a", 63) }},
		{"previous-digest-binding", func(f *orderpb.Fill) { f.Recovery.FeeApproval.PreviousExecutionDigest = strings.Repeat("0", 64) }},
		{"approval-clock", func(f *orderpb.Fill) { f.Recovery.FeeApproval.ApprovedAt = &timestamppb.Timestamp{} }},
		{"bad-nanos", func(f *orderpb.Fill) { f.Recovery.FeeApproval.ApprovedAt.Nanos = 1000000000 }},
		{"unknown-price", func(f *orderpb.Fill) { f.Price = nil }},
		{"currency-change", func(f *orderpb.Fill) { f.Fee.CurrencyCode = "EUR" }},
		{"unchanged-fee", func(f *orderpb.Fill) { f.Fee = proto.Clone(f.Recovery.FeeApproval.PreviousFee).(*commonpb.Money) }},
		{"outside-decimal-domain", func(f *orderpb.Fill) { f.Recovery.FeeApproval.PreviousFee.Amount.Exponent = 65 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := proto.Clone(revised).(*orderpb.Fill)
			tc.change(bad)
			if _, e := fillfact.FeeRevisionTerms(bad); e == nil {
				t.Fatal("Go accepted intended invalid evidence")
			}
			readError(t, runResult(t, map[string]any{"mode": "fee-terms", "fill": wire(t, bad)}), "FeeRevision")
		})
	}
}

func (f *fixture) feeSeed() (*orderpb.OrderState, int64, *orderpb.Fill) {
	f.t.Helper()
	s, v := f.seed()
	original := fill()
	next := f.candidate(s, original)
	r, e := order.NewEmitter(nil).FillFact(bus.WithTenantID(f.ctx, f.tenant), original, next)
	if e != nil {
		f.t.Fatal(e)
	}
	if e = order.NewPostgres(f.pool).Save(f.ctx, next, v, []outbox.Record{r}, original.FillId); e != nil {
		f.t.Fatal(e)
	}
	return next, v + 1, original
}
func (f *fixture) feeRequest(s *orderpb.OrderState, v int64, original, revised *orderpb.Fill, announce bool) map[string]any {
	f.t.Helper()
	records := []map[string]any{}
	if announce {
		// Disposable caller-authored canonical recovery FACT. This proves storage
		// atomicity, not authentication of a recovery case or broker publication.
		payload := &orderpb.ExecutionRecovered{Fill: revised, State: s}
		event := bus.Event{Subject: fillfact.SubjectRecovered, EventType: fillfact.SubjectRecovered, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, Domain: order.Domain, EventTime: at, PartitionKey: s.OrderId, PayloadSchemaRef: fmt.Sprintf("%s:1", payload.ProtoReflect().Descriptor().FullName()), Payload: payload}
		record, e := outbox.From(bus.WithTenantID(f.ctx, f.tenant), event)
		if e != nil {
			f.t.Fatal(e)
		}
		records = append(records, factJSON(record))
	}
	return map[string]any{"mode": "fee-revision", "schema": f.schema, "tenant": f.tenant, "state": wire(f.t, s), "expected_version": v, "fill": wire(f.t, revised), "other": wire(f.t, original), "records": records}
}
func feeResult(t *testing.T, r fixtureResult, status, delta string, fresh bool) {
	t.Helper()
	if r.Status != status || r.Delta == nil || *r.Delta != delta || r.Fresh == nil || *r.Fresh != fresh {
		t.Fatal("fee outcome/delta/freshness differs")
	}
}
func (f *fixture) feeCounts(version int64, revisions, announcements int) {
	f.t.Helper()
	var v int64
	if e := f.pool.QueryRow(f.ctx, "SELECT version FROM orders WHERE order_id='order'").Scan(&v); e != nil || v != version {
		f.t.Fatal("fee transaction changed wrong order version")
	}
	for table, want := range map[string]int{"execution_fee_revisions": revisions, "outbox": announcements, "order_fills": 1, "order_executions": 1} {
		var n int
		if e := f.pool.QueryRow(f.ctx, "SELECT count(*) FROM "+table).Scan(&n); e != nil || n != want {
			f.t.Fatalf("durable %s count differs", table)
		}
	}
}
func (f *fixture) feeHead(original, want *orderpb.Fill) {
	f.t.Helper()
	tx, e := f.pool.Begin(f.ctx)
	if e != nil {
		f.t.Fatal(e)
	}
	defer func() { _ = tx.Rollback(f.ctx) }()
	got, e := fillfact.FeeHead(f.ctx, tx, "oms", original)
	if e != nil || !proto.Equal(got, want) {
		f.t.Fatal("independent Go fee head differs")
	}
	var raw []byte
	if e = f.pool.QueryRow(f.ctx, "SELECT fill FROM order_executions WHERE order_id='order'").Scan(&raw); e != nil || !bytes.Equal(raw, wire(f.t, original)) {
		f.t.Fatal("original execution evidence changed")
	}
}
func TestRustFeeJournalTransactions(t *testing.T) {
	t.Run("commit-restart-aba-replay", func(t *testing.T) {
		f := newFixture(t)
		s, v, original := f.feeSeed()
		first := revision(t, original, "first", &commonpb.Decimal{Coefficient: 5, Exponent: -1})
		second := revision(t, first, "second", &commonpb.Decimal{Coefficient: 1})
		first.ProtoReflect().SetUnknown(joined(varField(1000, 1), varField(1001, 2)))
		feeResult(t, runResult(t, f.feeRequest(s, v, original, first, true)), "committed", "1/2", true)
		v++
		f.pool = f.open(f.tenant)
		f.feeHead(original, first)
		feeResult(t, runResult(t, f.feeRequest(s, v, original, second, true)), "committed", "-1/2", true)
		v++
		// Known field reordering is semantically equal and cannot refund the old case again.
		req := f.feeRequest(s, v, original, first, false)
		req["fill"] = reversedFields(t, wire(t, first))
		feeResult(t, runResult(t, req), "committed", "0", false)
		v++
		f.feeCounts(v, 2, 3)
		f.feeHead(original, second)
		rows, e := f.pool.Query(f.ctx, "SELECT previous_fill,revised_fill FROM execution_fee_revisions WHERE book='oms' ORDER BY revision_sequence")
		if e != nil {
			t.Fatal(e)
		}
		defer rows.Close()
		expected := [][2]*orderpb.Fill{{original, first}, {first, second}}
		i := 0
		for rows.Next() {
			var before, after []byte
			if e = rows.Scan(&before, &after); e != nil || i >= len(expected) || !bytes.Equal(before, wire(t, expected[i][0])) || !bytes.Equal(after, wire(t, expected[i][1])) {
				t.Fatal("journal bytes differ from Go deterministic representation")
			}
			i++
		}
		if rows.Err() != nil || i != 2 {
			t.Fatal("incomplete journal read")
		}
		for _, sql := range []string{"UPDATE execution_fee_revisions SET approval_digest=repeat('b',64)", "DELETE FROM execution_fee_revisions", "TRUNCATE execution_fee_revisions"} {
			if _, e = f.pool.Exec(f.ctx, sql); e == nil {
				t.Fatal("immutable fee journal mutation succeeded")
			}
		}
	})
	t.Run("case-conflict-stale-head", func(t *testing.T) {
		f := newFixture(t)
		s, v, original := f.feeSeed()
		first := revision(t, original, "first", &commonpb.Decimal{Coefficient: 5, Exponent: -1})
		feeResult(t, runResult(t, f.feeRequest(s, v, original, first, true)), "committed", "1/2", true)
		v++
		changed := proto.Clone(first).(*orderpb.Fill)
		changed.ProtoReflect().SetUnknown(varField(1000, 1))
		readError(t, runResult(t, f.feeRequest(s, v, original, changed, true)), "FeeRevision")
		stale := revision(t, original, "stale", &commonpb.Decimal{Coefficient: 25, Exponent: -2})
		readError(t, runResult(t, f.feeRequest(s, v, original, stale, true)), "FeeRevision")
		f.feeCounts(v, 1, 2)
		f.feeHead(original, first)
	})
	t.Run("concurrent-cas-one-revision", func(t *testing.T) {
		f := newFixture(t)
		s, v, original := f.feeSeed()
		first := revision(t, original, "first", &commonpb.Decimal{Coefficient: 5, Exponent: -1})
		req := f.feeRequest(s, v, original, first, true)
		results := make(chan fixtureResult, 2)
		var wg sync.WaitGroup
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); results <- runResult(t, req) }()
		}
		wg.Wait()
		close(results)
		counts := map[string]int{}
		for r := range results {
			counts[r.Status]++
			if r.Status == "committed" {
				feeResult(t, r, "committed", "1/2", true)
			} else {
				readError(t, r, "Conflict")
			}
		}
		if counts["committed"] != 1 || counts["Conflict"] != 1 {
			t.Fatal("concurrent revision did not have one CAS winner")
		}
		f.feeCounts(v+1, 1, 2)
	})
	t.Run("explicit-rollback", func(t *testing.T) {
		f := newFixture(t)
		s, v, original := f.feeSeed()
		first := revision(t, original, "first", &commonpb.Decimal{Coefficient: 5, Exponent: -1})
		req := f.feeRequest(s, v, original, first, true)
		req["rollback"] = true
		feeResult(t, runResult(t, req), "rolled_back", "1/2", true)
		f.feeCounts(v, 0, 1)
		f.feeHead(original, original)
		feeResult(t, runResult(t, f.feeRequest(s, v, original, first, true)), "committed", "1/2", true)
	})
	t.Run("outbox-failure-atomic", func(t *testing.T) {
		f := newFixture(t)
		s, v, original := f.feeSeed()
		first := revision(t, original, "first", &commonpb.Decimal{Coefficient: 5, Exponent: -1})
		req := f.feeRequest(s, v, original, first, true)
		req["records"].([]map[string]any)[0]["tenant"] = ""
		readError(t, runResult(t, req), "InvalidInput")
		f.feeCounts(v, 0, 1)
		f.feeHead(original, original)
	})
	t.Run("deferred-commit-atomic", func(t *testing.T) {
		f := newFixture(t)
		s, v, original := f.feeSeed()
		first := revision(t, original, "first", &commonpb.Decimal{Coefficient: 5, Exponent: -1})
		if _, e := f.pool.Exec(f.ctx, `CREATE SEQUENCE fee_commit_attempt; CREATE FUNCTION fee_commit_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM nextval('fee_commit_attempt'); RAISE EXCEPTION 'fixture deferred fee rejection'; END $$; CREATE CONSTRAINT TRIGGER fee_commit_failure AFTER INSERT ON execution_fee_revisions DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION fee_commit_failure()`); e != nil {
			t.Fatal(e)
		}
		readError(t, runResult(t, f.feeRequest(s, v, original, first, true)), "CommitUnconfirmed")
		f.feeCounts(v, 0, 1)
		var called bool
		if e := f.pool.QueryRow(f.ctx, "SELECT is_called FROM fee_commit_attempt").Scan(&called); e != nil || !called {
			t.Fatal("commit trigger was not reached")
		}
	})
	t.Run("tenant-same-execution-case", func(t *testing.T) {
		a := newFixture(t)
		sa, va, original := a.feeSeed()
		b := *a
		b.tenant = "tenant-b"
		b.pool = b.open(b.tenant)
		sb, vb, bo := b.feeSeed()
		ar := revision(t, original, "same", &commonpb.Decimal{Coefficient: 5, Exponent: -1})
		br := revision(t, bo, "same", &commonpb.Decimal{Coefficient: 25, Exponent: -2})
		feeResult(t, runResult(t, a.feeRequest(sa, va, original, ar, true)), "committed", "1/2", true)
		feeResult(t, runResult(t, b.feeRequest(sb, vb, bo, br, true)), "committed", "3/4", true)
		a.feeCounts(va+1, 1, 2)
		b.feeCounts(vb+1, 1, 2)
		a.feeHead(original, ar)
		b.feeHead(bo, br)
		for _, f := range []*fixture{a, &b} {
			var n int
			if e := f.pool.QueryRow(f.ctx, "SELECT count(*) FROM outbox WHERE envelope_tenant_id<>$1", f.tenant).Scan(&n); e != nil || n != 0 {
				t.Fatal("cross-tenant announcement")
			}
		}
	})
	t.Run("other-book-not-oms-head", func(t *testing.T) {
		f := newFixture(t)
		s, v, original := f.feeSeed()
		ledger := revision(t, original, "ledger", &commonpb.Decimal{Coefficient: 25, Exponent: -2})
		tx, e := f.pool.Begin(f.ctx)
		if e != nil {
			t.Fatal(e)
		}
		if _, _, e = fillfact.RecordFeeRevision(f.ctx, tx, "ledger", original, ledger); e != nil {
			_ = tx.Rollback(f.ctx)
			t.Fatal(e)
		}
		if e = tx.Commit(f.ctx); e != nil {
			t.Fatal(e)
		}
		next := revision(t, original, "oms", &commonpb.Decimal{Coefficient: 5, Exponent: -1})
		feeResult(t, runResult(t, f.feeRequest(s, v, original, next, true)), "committed", "1/2", true)
		f.feeHead(original, next)
	})
	t.Run("corrupt-bounded-head", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			raw    []byte
			status string
		}{{"corrupt", []byte{255}, "InvalidInput"}, {"oversized", make([]byte, (1<<20)+1), "Capacity"}} {
			t.Run(tc.name, func(t *testing.T) {
				f := newFixture(t)
				s, v, original := f.feeSeed()
				key := fillfact.ExecutionKey(original)
				if _, e := f.pool.Exec(f.ctx, "INSERT INTO execution_fee_revisions(book,execution_key,case_id,approval_digest,previous_fill,revised_fill,order_id) VALUES('oms',$1,'corrupt',repeat('a',64),$2,$3,'order')", key, wire(t, original), tc.raw); e != nil {
					t.Fatal(e)
				}
				next := revision(t, original, "next", &commonpb.Decimal{Coefficient: 5, Exponent: -1})
				readError(t, runResult(t, f.feeRequest(s, v, original, next, true)), tc.status)
				f.feeCounts(v, 1, 1)
			})
		}
	})
	t.Run("scope-and-order-refusal", func(t *testing.T) {
		f := newFixture(t)
		s, v, original := f.feeSeed()
		next := revision(t, original, "first", &commonpb.Decimal{Coefficient: 5, Exponent: -1})
		req := f.feeRequest(s, v, original, next, true)
		req["tenant"] = ""
		readError(t, runResult(t, req), "InvalidInput")
		foreign := proto.Clone(original).(*orderpb.Fill)
		foreign.OrderId = "other"
		foreignRevision := revision(t, foreign, "first", &commonpb.Decimal{Coefficient: 5, Exponent: -1})
		readError(t, runResult(t, f.feeRequest(s, v, foreign, foreignRevision, true)), "FeeRevision")
		f.feeCounts(v, 0, 1)
	})
}
