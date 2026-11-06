package storeconformance

import (
	"bytes"
	"fmt"
	"math/big"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/execution"
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

type recoveryFixture struct {
	*fixture
	state     *orderpb.OrderState
	version   int64
	c         order.RecoveryCase
	history   *orderpb.ExecutionRecoveryHistory
	digest    string
	posted    []*orderpb.Fill
	approvals map[string]*orderpb.ExecutionFeeApproval
}

func recoveryFill(id string, quantity, price int64) *orderpb.Fill {
	f := fill()
	f.FillId = id
	f.VenueExecutionId = id
	f.Quantity = &commonpb.Decimal{Coefficient: quantity}
	f.Price = &commonpb.Decimal{Coefficient: price}
	return f
}
func prepareRecovery(f *fixture, posted, history []*orderpb.Fill, terminal bool) *recoveryFixture {
	f.t.Helper()
	store := order.NewPostgres(f.pool)
	st := &orderpb.OrderState{OrderId: "order", PortfolioId: f.tenant + "-portfolio", Venue: "VENUE", VenueAccountId: "account", InstrumentId: "instrument", Side: orderpb.Side_SIDE_BUY, Status: orderpb.OrderStatus_ORDER_STATUS_ROUTED, OrderedQuantity: &commonpb.Decimal{Coefficient: 10}, LeavesQuantity: &commonpb.Decimal{Coefficient: 10}}
	st.ProtoReflect().SetUnknown(varField(1000, 7))
	if e := store.Create(f.ctx, st, nil); e != nil {
		f.t.Fatal(e)
	}
	var version int64
	for _, fill := range posted {
		next, e := order.ApplyFill(st, fill, at)
		if e != nil {
			f.t.Fatal(e)
		}
		fact, e := order.NewEmitter(nil).FillFact(bus.WithTenantID(f.ctx, f.tenant), fill, next)
		if e != nil {
			f.t.Fatal(e)
		}
		if e = store.Save(f.ctx, next, version, []outbox.Record{fact}, fill.FillId); e != nil {
			f.t.Fatal(e)
		}
		st = next
		version++
	}
	if terminal {
		next, _, e := order.Cancel(st, at)
		if e != nil {
			f.t.Fatal(e)
		}
		if e = store.Save(f.ctx, next, version, nil, ""); e != nil {
			f.t.Fatal(e)
		}
		st = next
		version++
	}
	m := order.RecoveryMapping{Tenant: f.tenant, Portfolio: st.PortfolioId, Venue: st.Venue, Account: st.VenueAccountId, ExchangeAccount: "verified-fixture"}
	mapping, e := store.RecordRecoveryMapping(f.ctx, m, execution.AccountProof{Verified: true, ExchangeAccountID: m.ExchangeAccount})
	if e != nil {
		f.t.Fatal(e)
	}
	c, e := store.ObserveRecovery(f.ctx, "case", "order", "cursor", []byte("independent fixture observation"))
	if e != nil {
		f.t.Fatal(e)
	}
	quantity := new(big.Rat)
	for _, fill := range history {
		quantity.Add(quantity, dec.FromProto(fill.Quantity))
	}
	total, ok := dec.ToProtoScaled(quantity)
	if !ok {
		f.t.Fatal("fixture total")
	}
	if e = store.FreezeRecoveryHistory(bus.WithTenantID(f.ctx, f.tenant), c, mapping, st, execution.OrderView{State: execution.OrderViewCancelled, ExecutedQuantity: total, Fills: history}, at); e != nil {
		f.t.Fatal(e)
	}
	c, e = store.RecoveryCase(f.ctx, c.ID)
	if e != nil {
		f.t.Fatal(e)
	}
	frozen, digest, e := store.RecoveryHistory(f.ctx, c.ID)
	if e != nil {
		f.t.Fatal(e)
	}
	return &recoveryFixture{fixture: f, state: st, version: version, c: c, history: frozen, digest: digest, posted: posted, approvals: map[string]*orderpb.ExecutionFeeApproval{}}
}
func (f *recoveryFixture) approve() {
	f.t.Helper()
	store := order.NewPostgres(f.pool)
	ctx := bus.WithTenantID(f.ctx, f.tenant)
	if e := store.BlockRecovery(ctx, f.c, f.state, "fee requires approval", at); e != nil {
		f.t.Fatal(e)
	}
	c, e := store.RecoveryCase(ctx, f.c.ID)
	if e != nil {
		f.t.Fatal(e)
	}
	if e = store.ProposeFeeCorrection(ctx, c, "user:maker", "verified fixture statement", at); e != nil {
		f.t.Fatal(e)
	}
	proposal, e := store.FeeProposal(ctx, c.ID)
	if e != nil {
		f.t.Fatal(e)
	}
	approvedAt := at.Add(time.Minute)
	if e = store.ApproveFeeCorrection(ctx, c, "user:checker", proposal.Digest, approvedAt); e != nil {
		f.t.Fatal(e)
	}
	f.c, e = store.RecoveryCase(ctx, c.ID)
	if e != nil {
		f.t.Fatal(e)
	}
	// ProposeFeeCorrection performs an order CAS. Real CommitRecovery reloads
	// this version before preparing its durable tail; do not reuse the seed CAS.
	previousVersion := f.version
	f.state, f.version, e = store.Load(ctx, f.c.OrderID)
	if e != nil || f.version <= previousVersion {
		f.t.Fatal("approved fixture did not reload proposal CAS")
	}
	for _, change := range proposal.Changes {
		digest, e := fillfact.EconomicDigest(change.Previous)
		if e != nil {
			f.t.Fatal(e)
		}
		f.approvals[fillfact.ExecutionKey(change.Previous)] = &orderpb.ExecutionFeeApproval{ProposalId: c.ID, Digest: proposal.Digest, Proposer: "user:maker", Approver: "user:checker", ApprovedAt: timestamppb.New(approvedAt), PreviousExecutionDigest: digest, PreviousFee: change.Previous.Fee}
	}
}

// Preparation intentionally remains Go-owned. This test adapter uses canonical
// folds and exact complete-history averaging; it is not a second production owner.
func (f *recoveryFixture) request() map[string]any {
	f.t.Helper()
	next := proto.Clone(f.state).(*orderpb.OrderState)
	posted := map[string]bool{}
	rawPosted := [][]byte{}
	for _, p := range f.posted {
		posted[fillfact.ExecutionKey(p)] = true
		rawPosted = append(rawPosted, wire(f.t, p))
	}
	all := []*orderpb.Fill{}
	missing := []string{}
	now := at.Add(2 * time.Minute)
	for _, original := range f.history.Fills {
		fill := proto.Clone(original).(*orderpb.Fill)
		digest, e := fillfact.ExecutionDigest(fill)
		if e != nil {
			f.t.Fatal(e)
		}
		fill.Recovery = &orderpb.ExecutionRecoveryProvenance{CaseId: f.c.ID, MappingVersion: f.c.MappingVersion, SourceCursor: f.c.SourceCursor, PayloadDigest: f.digest, ExecutionDigest: digest, ObservedAt: f.history.ObservedAt, FeeApproval: f.approvals[fillfact.ExecutionKey(fill)]}
		all = append(all, fill)
		key := fillfact.ExecutionKey(fill)
		if posted[key] {
			continue
		}
		working := proto.Clone(next).(*orderpb.OrderState)
		terminal := order.IsTerminal(next)
		if terminal {
			working.Status = orderpb.OrderStatus_ORDER_STATUS_ROUTED
		}
		leaves, ok := dec.ToProtoScaled(new(big.Rat).Sub(dec.FromProto(next.OrderedQuantity), dec.FromProto(next.FilledQuantity)))
		if !ok {
			f.t.Fatal("fixture leaves")
		}
		working.LeavesQuantity = leaves
		result, e := order.ApplyFill(working, fill, now)
		if e != nil {
			f.t.Fatal(e)
		}
		if terminal {
			result.Status = next.Status
		}
		next = result
		missing = append(missing, key)
	}
	if dec.IsPositive(f.history.ExecutedQuantity) {
		cost := new(big.Rat)
		for _, fill := range f.history.Fills {
			cost.Add(cost, new(big.Rat).Mul(dec.FromProto(fill.Quantity), dec.FromProto(fill.Price)))
		}
		average, ok := dec.ToProtoScaled(new(big.Rat).Quo(cost, dec.FromProto(f.history.ExecutedQuantity)))
		if !ok {
			f.t.Fatal("fixture average")
		}
		next.AverageFillPrice = average
	}
	records := []map[string]any{}
	appendFact := func(subject string, payload proto.Message) {
		f.t.Helper()
		record, e := outbox.From(bus.WithTenantID(f.ctx, f.tenant), bus.Event{Subject: subject, EventType: subject, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, Domain: order.Domain, EventTime: now, PartitionKey: next.OrderId, PayloadSchemaRef: fmt.Sprintf("%s:1", payload.ProtoReflect().Descriptor().FullName()), Payload: payload})
		if e != nil {
			f.t.Fatal(e)
		}
		records = append(records, factJSON(record))
	}
	for _, fill := range all {
		appendFact(fillfact.SubjectRecovered, &orderpb.ExecutionRecovered{Fill: fill, State: next})
	}
	status, reason := "investigating", "OMS committed execution evidence; awaiting position and ledger acknowledgements"
	if len(all) == 0 {
		status, reason = "corrected", ""
	}
	appendFact(order.EventTypeRecoveryRecorded, &orderpb.ExecutionRecoveryRecorded{CaseId: f.c.ID, OrderId: f.c.OrderID, PortfolioId: next.PortfolioId, Venue: next.Venue, VenueAccountId: next.VenueAccountId, MappingVersion: f.c.MappingVersion, SourceCursor: f.c.SourceCursor, PayloadDigest: f.c.Digest, Status: status, Reason: reason, Checkpoint: uint32(len(all)), RecordedAt: timestamppb.New(now)})
	return map[string]any{"mode": "recovery", "schema": f.schema, "tenant": f.tenant, "state": wire(f.t, next), "expected_version": f.version, "case_version": f.c.Version, "history_digest": f.digest, "missing": missing, "posted": rawPosted, "records": records}
}

// Capture every changed durable owner; omit generated timestamps/IDs only.
func (f *recoveryFixture) snapshot() map[string][]string {
	f.t.Helper()
	queries := map[string]string{
		"orders":     "SELECT order_id||':'||version||':'||encode(state,'hex') FROM orders ORDER BY order_id",
		"claims":     "SELECT order_id||':'||fill_id||':'||raw_fill_id FROM order_fills ORDER BY fill_id",
		"executions": "SELECT order_id||':'||fill_id||':'||encode(fill,'hex') FROM order_executions ORDER BY fill_id",
		"fees":       "SELECT book||':'||execution_key||':'||case_id||':'||approval_digest||':'||encode(previous_fill,'hex')||':'||encode(revised_fill,'hex') FROM execution_fee_revisions ORDER BY revision_sequence",
		"case":       "SELECT case_id||':'||version||':'||checkpoint||':'||expected_ack_count||':'||status||':'||reason FROM execution_recovery_cases ORDER BY case_id",
		"targets":    "SELECT case_id||':'||execution_key||':'||payload_digest||':'||execution_digest FROM execution_recovery_targets ORDER BY execution_key",
		"outbox":     "SELECT producer_service||':'||envelope_tenant_id||':'||partition_key||':'||subject||':'||event_type||':'||event_class||':'||schema_version||':'||domain||':'||payload_schema_ref||':'||encode(payload,'hex') FROM outbox ORDER BY subject,payload",
	}
	result := map[string][]string{}
	for key, query := range queries {
		rows, e := f.pool.Query(f.ctx, query)
		if e != nil {
			f.t.Fatal(e)
		}
		values := []string{}
		for rows.Next() {
			var value string
			if e = rows.Scan(&value); e != nil {
				f.t.Fatal(e)
			}
			values = append(values, value)
		}
		if rows.Err() != nil {
			f.t.Fatal(rows.Err())
		}
		rows.Close()
		result[key] = values
	}
	return result
}
func sameRecovery(t *testing.T, a, b map[string][]string) {
	t.Helper()
	for key, want := range a {
		if !reflect.DeepEqual(want, b[key]) {
			t.Fatalf("recovery durable owner differs: %s", key)
		}
	}
}
func TestRustRecoveryTransactions(t *testing.T) {
	t.Run("go-differential-terminal-restart", func(t *testing.T) {
		old, newFill := recoveryFill("first", 1, 100), recoveryFill("second", 2, 101)
		newFill.ProtoReflect().SetUnknown(varField(1000, 19))
		goFixture := prepareRecovery(newFixture(t), []*orderpb.Fill{old}, []*orderpb.Fill{old, newFill}, true)
		rust := prepareRecovery(newFixture(t), []*orderpb.Fill{old}, []*orderpb.Fill{old, newFill}, true)
		if e := order.NewPostgres(goFixture.pool).CommitRecovery(bus.WithTenantID(goFixture.ctx, goFixture.tenant), goFixture.c, at.Add(2*time.Minute)); e != nil {
			t.Fatal(e)
		}
		req := rust.request()
		expect(t, run(t, req), "committed")
		rust.pool = rust.open(rust.tenant)
		sameRecovery(t, goFixture.snapshot(), rust.snapshot())
		var next orderpb.OrderState
		if e := proto.Unmarshal(req["state"].([]byte), &next); e != nil {
			t.Fatal(e)
		}
		// Independent exact weighted mean (100+2*101)/3 rounded once to 8 places.
		if next.Status != orderpb.OrderStatus_ORDER_STATUS_CANCELLED || next.AverageFillPrice.Coefficient != 10066666667 || next.AverageFillPrice.Exponent != -8 || dec.Cmp(next.FilledQuantity, &commonpb.Decimal{Coefficient: 3}) != 0 {
			t.Fatal("independent terminal projection differs")
		}
		before := rust.snapshot()
		readError(t, runResult(t, req), "Conflict")
		sameRecovery(t, before, rust.snapshot())
	})
	t.Run("zero-history-corrected", func(t *testing.T) {
		goFixture := prepareRecovery(newFixture(t), nil, nil, false)
		rust := prepareRecovery(newFixture(t), nil, nil, false)
		if e := order.NewPostgres(goFixture.pool).CommitRecovery(bus.WithTenantID(goFixture.ctx, goFixture.tenant), goFixture.c, at.Add(2*time.Minute)); e != nil {
			t.Fatal(e)
		}
		expect(t, run(t, rust.request()), "committed")
		sameRecovery(t, goFixture.snapshot(), rust.snapshot())
	})
	t.Run("approved-fee-go-differential", func(t *testing.T) {
		old := recoveryFill("first", 1, 100)
		revised := proto.Clone(old).(*orderpb.Fill)
		revised.Fee.Amount = &commonpb.Decimal{Coefficient: 5, Exponent: -1}
		goFixture := prepareRecovery(newFixture(t), []*orderpb.Fill{old}, []*orderpb.Fill{revised}, false)
		rust := prepareRecovery(newFixture(t), []*orderpb.Fill{old}, []*orderpb.Fill{revised}, false)
		goFixture.approve()
		rust.approve()
		if e := order.NewPostgres(goFixture.pool).CommitRecovery(bus.WithTenantID(goFixture.ctx, goFixture.tenant), goFixture.c, at.Add(2*time.Minute)); e != nil {
			t.Fatal(e)
		}
		expect(t, run(t, rust.request()), "committed")
		sameRecovery(t, goFixture.snapshot(), rust.snapshot())
	})
	t.Run("mandatory-facts-and-provenance", func(t *testing.T) {
		f := prepareRecovery(newFixture(t), nil, []*orderpb.Fill{recoveryFill("first", 1, 100)}, false)
		before := f.snapshot()
		for _, name := range []string{"missing-status", "missing-execution", "wrong-tenant", "wrong-class", "wrong-state", "wrong-digest", "duplicate-execution", "wrong-checkpoint"} {
			t.Run(name, func(t *testing.T) {
				req := f.request()
				records := req["records"].([]map[string]any)
				switch name {
				case "missing-status":
					req["records"] = records[:1]
				case "missing-execution":
					req["records"] = records[1:]
				case "wrong-tenant":
					records[0]["tenant"] = "tenant-b"
				case "wrong-class":
					records[0]["event_class"] = int32(2)
				case "wrong-state":
					var event orderpb.ExecutionRecovered
					if e := proto.Unmarshal(records[0]["payload"].([]byte), &event); e != nil {
						t.Fatal(e)
					}
					event.State.PortfolioId = "other"
					records[0]["payload"] = wire(t, &event)
				case "wrong-digest":
					req["history_digest"] = fmt.Sprintf("%064d", 0)
				case "duplicate-execution":
					req["records"] = []map[string]any{records[0], records[0], records[1]}
				case "wrong-checkpoint":
					var event orderpb.ExecutionRecoveryRecorded
					if e := proto.Unmarshal(records[1]["payload"].([]byte), &event); e != nil {
						t.Fatal(e)
					}
					event.Checkpoint = 2
					records[1]["payload"] = wire(t, &event)
				}
				readError(t, runResult(t, req), "InvalidInput")
				sameRecovery(t, before, f.snapshot())
			})
		}
	})
	t.Run("case-cas-rollback-all-owners", func(t *testing.T) {
		f := prepareRecovery(newFixture(t), nil, []*orderpb.Fill{recoveryFill("first", 1, 100)}, false)
		req := f.request()
		req["case_version"] = f.c.Version + 1
		before := f.snapshot()
		readError(t, runResult(t, req), "Conflict")
		sameRecovery(t, before, f.snapshot())
		expect(t, run(t, f.request()), "committed")
	})
	t.Run("order-cas-rollback", func(t *testing.T) {
		f := prepareRecovery(newFixture(t), nil, []*orderpb.Fill{recoveryFill("first", 1, 100)}, false)
		before := f.snapshot()
		req := f.request()
		req["expected_version"] = f.version + 1
		readError(t, runResult(t, req), "Conflict")
		sameRecovery(t, before, f.snapshot())
		expect(t, run(t, f.request()), "committed")
	})
	t.Run("unsafe-recovery-rls", func(t *testing.T) {
		f := prepareRecovery(newFixture(t), nil, []*orderpb.Fill{recoveryFill("first", 1, 100)}, false)
		before := f.snapshot()
		if _, e := f.pool.Exec(f.ctx, "ALTER TABLE execution_recovery_targets NO FORCE ROW LEVEL SECURITY"); e != nil {
			t.Fatal(e)
		}
		readError(t, runResult(t, f.request()), "UnsafeRole")
		sameRecovery(t, before, f.snapshot())
	})
	t.Run("fee-and-claims-rollback", func(t *testing.T) {
		old := recoveryFill("first", 1, 100)
		revised := proto.Clone(old).(*orderpb.Fill)
		revised.Fee.Amount = &commonpb.Decimal{Coefficient: 5, Exponent: -1}
		f := prepareRecovery(newFixture(t), []*orderpb.Fill{old}, []*orderpb.Fill{revised, recoveryFill("second", 1, 101)}, false)
		f.approve()
		before := f.snapshot()
		if _, e := f.pool.Exec(f.ctx, `CREATE SEQUENCE recovery_fee_reached; CREATE FUNCTION recovery_fee_witness() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM nextval('recovery_fee_reached'); RETURN NEW; END $$; CREATE TRIGGER recovery_fee_witness BEFORE INSERT ON execution_fee_revisions FOR EACH ROW EXECUTE FUNCTION recovery_fee_witness()`); e != nil {
			t.Fatal(e)
		}
		req := f.request()
		req["case_version"] = f.c.Version + 1
		readError(t, runResult(t, req), "Conflict")
		sameRecovery(t, before, f.snapshot())
		var reached bool
		if e := f.pool.QueryRow(f.ctx, "SELECT is_called FROM recovery_fee_reached").Scan(&reached); e != nil || !reached {
			t.Fatal("fee append was not reached before case refusal")
		}
		expect(t, run(t, f.request()), "committed")
	})
	t.Run("order-cas-and-push-race", func(t *testing.T) {
		fill := recoveryFill("first", 1, 100)
		f := prepareRecovery(newFixture(t), nil, []*orderpb.Fill{fill}, false)
		req := f.request()
		next := f.candidate(f.state, fill)
		fact, e := order.NewEmitter(nil).FillFact(bus.WithTenantID(f.ctx, f.tenant), fill, next)
		if e != nil {
			t.Fatal(e)
		}
		start := make(chan struct{})
		results := make(chan string, 2)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); <-start; results <- run(t, req) }()
		go func() {
			defer wg.Done()
			<-start
			e := order.NewPostgres(f.pool).Save(f.ctx, next, f.version, []outbox.Record{fact}, fill.FillId)
			if e == nil {
				results <- "push"
			} else if e == order.ErrFillApplied || e == order.ErrConflict {
				results <- "loser"
			} else {
				t.Error(e)
				results <- "unexpected"
			}
		}()
		close(start)
		wg.Wait()
		close(results)
		got := []string{}
		for r := range results {
			got = append(got, r)
		}
		sort.Strings(got)
		if !reflect.DeepEqual(got, []string{"Conflict", "push"}) && !reflect.DeepEqual(got, []string{"committed", "loser"}) {
			t.Fatalf("unexpected race outcomes %v", got)
		}
		snapshot := f.snapshot()
		if len(snapshot["claims"]) != 1 || len(snapshot["executions"]) != 1 {
			t.Fatal("double execution")
		}
	})
	t.Run("concurrent-recovery-one-checkpoint", func(t *testing.T) {
		f := prepareRecovery(newFixture(t), nil, []*orderpb.Fill{recoveryFill("first", 1, 100)}, false)
		req := f.request()
		results := make(chan string, 2)
		var wg sync.WaitGroup
		for range 2 {
			wg.Add(1)
			go func() { defer wg.Done(); results <- run(t, req) }()
		}
		wg.Wait()
		close(results)
		got := []string{}
		for r := range results {
			got = append(got, r)
		}
		sort.Strings(got)
		if !reflect.DeepEqual(got, []string{"Conflict", "committed"}) {
			t.Fatal("recovery CAS did not have one winner")
		}
		s := f.snapshot()
		if len(s["claims"]) != 1 || len(s["targets"]) != 1 || len(s["outbox"]) != 3 {
			t.Fatal("duplicate recovery durable owners")
		}
	})
	t.Run("target-outbox-commit-rollback", func(t *testing.T) {
		for _, stage := range []string{"target", "outbox", "commit"} {
			t.Run(stage, func(t *testing.T) {
				f := prepareRecovery(newFixture(t), nil, []*orderpb.Fill{recoveryFill("first", 1, 100)}, false)
				before := f.snapshot()
				table, timing, deferred := "execution_recovery_targets", "BEFORE", ""
				if stage == "outbox" {
					table = "outbox"
				}
				if stage == "commit" {
					table = "execution_recovery_cases"
					timing = "AFTER"
					deferred = "DEFERRABLE INITIALLY DEFERRED"
				}
				ddl := "CREATE SEQUENCE recovery_failure_reached; CREATE FUNCTION recovery_injected_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM nextval('recovery_failure_reached'); RAISE EXCEPTION 'fixture recovery failure'; END $$; CREATE "
				if stage == "commit" {
					ddl += "CONSTRAINT "
				}
				ddl += "TRIGGER recovery_injected_failure " + timing + " INSERT OR UPDATE ON " + table + " " + deferred + " FOR EACH ROW EXECUTE FUNCTION recovery_injected_failure()"
				if _, e := f.pool.Exec(f.ctx, ddl); e != nil {
					t.Fatal(e)
				}
				status := "Database"
				if stage == "commit" {
					status = "CommitUnconfirmed"
				}
				readError(t, runResult(t, f.request()), status)
				sameRecovery(t, before, f.snapshot())
				var reached bool
				if e := f.pool.QueryRow(f.ctx, "SELECT is_called FROM recovery_failure_reached").Scan(&reached); e != nil || !reached {
					t.Fatal("failure stage was not reached")
				}
			})
		}
	})
	t.Run("tenant-same-case-and-execution", func(t *testing.T) {
		a := newFixture(t)
		b := *a
		b.tenant = "tenant-b"
		b.pool = b.open(b.tenant)
		ar := prepareRecovery(a, nil, []*orderpb.Fill{recoveryFill("first", 1, 100)}, false)
		br := prepareRecovery(&b, nil, []*orderpb.Fill{recoveryFill("first", 1, 200)}, false)
		before := br.snapshot()
		expect(t, run(t, ar.request()), "committed")
		sameRecovery(t, before, br.snapshot())
		expect(t, run(t, br.request()), "committed")
		if bytes.Equal(ar.request()["state"].([]byte), br.request()["state"].([]byte)) {
			t.Fatal("tenant fixture lacks distinct economics")
		}
		for _, f := range []*recoveryFixture{ar, br} {
			var n int
			if e := f.pool.QueryRow(f.ctx, "SELECT count(*) FROM outbox WHERE envelope_tenant_id<>$1", f.tenant).Scan(&n); e != nil || n != 0 {
				t.Fatal("cross-tenant recovery fact")
			}
		}
	})
}
