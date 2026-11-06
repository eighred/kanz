package storeconformance

import (
	"sync"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/outbox"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/oms/internal/order"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func acknowledgementFixture(t *testing.T) *recoveryFixture {
	t.Helper()
	f := prepareRecovery(newFixture(t), nil, []*orderpb.Fill{recoveryFill("first", 1, 100), recoveryFill("second", 2, 101)}, true)
	if e := order.NewPostgres(f.pool).CommitRecovery(bus.WithTenantID(f.ctx, f.tenant), f.c, at.Add(2*time.Minute)); e != nil {
		t.Fatal(e)
	}
	return f
}
func (f *recoveryFixture) acknowledgement(index int) *orderpb.ExecutionRecoveryApplied {
	f.t.Helper()
	rows, e := f.pool.Query(f.ctx, "SELECT execution_key,payload_digest,execution_digest FROM execution_recovery_targets WHERE case_id=$1 ORDER BY execution_key", f.c.ID)
	if e != nil {
		f.t.Fatal(e)
	}
	defer rows.Close()
	acks := []*orderpb.ExecutionRecoveryApplied{}
	for rows.Next() {
		a := &orderpb.ExecutionRecoveryApplied{CaseId: f.c.ID, OrderId: f.c.OrderID}
		if e = rows.Scan(&a.ExecutionKey, &a.PayloadDigest, &a.ExecutionDigest); e != nil {
			f.t.Fatal(e)
		}
		acks = append(acks, a)
	}
	if rows.Err() != nil || index >= len(acks) {
		f.t.Fatal("missing actual acknowledgement target")
	}
	return acks[index]
}
func (f *recoveryFixture) ackRequest(index int, book string) map[string]any {
	f.t.Helper()
	store := order.NewPostgres(f.pool)
	c, e := store.RecoveryCase(f.ctx, f.c.ID)
	if e != nil {
		f.t.Fatal(e)
	}
	st, _, e := store.Load(f.ctx, c.OrderID)
	if e != nil {
		f.t.Fatal(e)
	}
	now := at.Add(3 * time.Minute)
	payload := &orderpb.ExecutionRecoveryRecorded{CaseId: c.ID, OrderId: c.OrderID, PortfolioId: st.PortfolioId, Venue: st.Venue, VenueAccountId: st.VenueAccountId, MappingVersion: c.MappingVersion, SourceCursor: c.SourceCursor, PayloadDigest: c.Digest, Status: "corrected", Checkpoint: uint32(c.Checkpoint), RecordedAt: timestamppb.New(now)}
	r, e := outbox.From(bus.WithTenantID(f.ctx, f.tenant), bus.Event{Subject: order.EventTypeRecoveryRecorded, EventType: order.EventTypeRecoveryRecorded, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, Domain: order.Domain, EventTime: now, PartitionKey: c.OrderID, PayloadSchemaRef: "order.v1.ExecutionRecoveryRecorded:1", Payload: payload})
	if e != nil {
		f.t.Fatal(e)
	}
	return map[string]any{"mode": "recovery-ack", "schema": f.schema, "tenant": f.tenant, "acknowledgement": wire(f.t, f.acknowledgement(index)), "book": book, "records": []map[string]any{factJSON(r)}}
}
func (f *recoveryFixture) ackSnapshot() map[string][]string {
	s := f.snapshot()
	rows, e := f.pool.Query(f.ctx, "SELECT case_id||':'||execution_key||':'||book FROM execution_recovery_acks ORDER BY execution_key,book")
	if e != nil {
		f.t.Fatal(e)
	}
	defer rows.Close()
	s["acks"] = []string{}
	for rows.Next() {
		var v string
		if e = rows.Scan(&v); e != nil {
			f.t.Fatal(e)
		}
		s["acks"] = append(s["acks"], v)
	}
	if rows.Err() != nil {
		f.t.Fatal(rows.Err())
	}
	return s
}
func primeAcknowledgements(f *recoveryFixture) {
	f.t.Helper()
	for _, step := range []struct {
		i int
		b string
	}{{0, "ledger"}, {0, "position"}, {1, "ledger"}} {
		expect(f.t, run(f.t, f.ackRequest(step.i, step.b)), "committed")
	}
}
func TestRustRecoveryAcknowledgements(t *testing.T) {
	t.Run("go-differential-both-books-restart", func(t *testing.T) {
		g, r := acknowledgementFixture(t), acknowledgementFixture(t)
		for _, step := range []struct {
			i int
			b string
		}{{0, "ledger"}, {0, "ledger"}, {1, "position"}, {0, "position"}, {1, "ledger"}, {1, "ledger"}} {
			before := r.ackSnapshot()
			if e := order.NewPostgres(g.pool).AcknowledgeRecovery(bus.WithTenantID(g.ctx, g.tenant), g.acknowledgement(step.i), step.b, at.Add(3*time.Minute)); e != nil {
				t.Fatal(e)
			}
			expect(t, run(t, r.ackRequest(step.i, step.b)), "committed")
			sameRecovery(t, g.ackSnapshot(), r.ackSnapshot())
			r.pool = r.open(r.tenant)
			if len(before["acks"]) < 3 && len(r.ackSnapshot()["outbox"]) != len(before["outbox"]) {
				t.Fatal("premature corrected FACT")
			}
		}
		end := r.ackSnapshot()
		if len(end["acks"]) != 4 {
			t.Fatal("must record exactly two books per target")
		}
		c, e := order.NewPostgres(r.pool).RecoveryCase(r.ctx, r.c.ID)
		if e != nil || c.Status != "corrected" {
			t.Fatal("all books did not close case")
		}
	})
	t.Run("concurrent-last-ack-one-fact", func(t *testing.T) {
		f := acknowledgementFixture(t)
		primeAcknowledgements(f)
		before := f.ackSnapshot()
		req := f.ackRequest(1, "position")
		var wg sync.WaitGroup
		results := make(chan string, 2)
		for range 2 {
			wg.Add(1)
			go func() { defer wg.Done(); results <- run(t, req) }()
		}
		wg.Wait()
		close(results)
		for v := range results {
			expect(t, v, "committed")
		}
		after := f.ackSnapshot()
		if len(after["acks"]) != 4 || len(after["outbox"]) != len(before["outbox"])+1 {
			t.Fatal("duplicate completion")
		}
	})
	t.Run("target-and-record-binding", func(t *testing.T) {
		f := acknowledgementFixture(t)
		before := f.ackSnapshot()
		for _, mutation := range []string{"order", "target", "payload", "execution", "tenant", "class", "schema", "status", "portfolio"} {
			t.Run(mutation, func(t *testing.T) {
				req := f.ackRequest(1, "position")
				ack := f.acknowledgement(1)
				records := req["records"].([]map[string]any)
				switch mutation {
				case "order":
					ack.OrderId = "wrong"
				case "target":
					ack.ExecutionKey = "wrong"
				case "payload":
					ack.PayloadDigest = "wrong"
				case "execution":
					ack.ExecutionDigest = "wrong"
				case "tenant":
					records[0]["tenant"] = "other"
				case "class":
					records[0]["event_class"] = 2
				case "schema":
					records[0]["payload_schema_ref"] = "wrong"
				case "status", "portfolio":
					var payload orderpb.ExecutionRecoveryRecorded
					if e := proto.Unmarshal(records[0]["payload"].([]byte), &payload); e != nil {
						t.Fatal(e)
					}
					if mutation == "status" {
						payload.Status = "investigating"
					} else {
						payload.PortfolioId = "other"
					}
					records[0]["payload"] = wire(t, &payload)
				}
				req["acknowledgement"] = wire(t, ack)
				result := run(t, req)
				if result != "InvalidInput" && result != "IdentityConflict" {
					t.Fatalf("binding accepted: %s", result)
				}
				sameRecovery(t, before, f.ackSnapshot())
			})
		}
	})
	t.Run("corrupt-counts-refuse-before-claim", func(t *testing.T) {
		for _, variant := range []string{"zero-expected", "extra-target"} {
			t.Run(variant, func(t *testing.T) {
				f := acknowledgementFixture(t)
				req := f.ackRequest(0, "ledger")
				query := "UPDATE execution_recovery_cases SET expected_ack_count=0"
				if variant == "extra-target" {
					query = "INSERT INTO execution_recovery_targets(case_id,execution_key,payload_digest,execution_digest) VALUES ('case','extra','digest','execution')"
				}
				if _, e := f.pool.Exec(f.ctx, query); e != nil {
					t.Fatal(e)
				}
				before := f.ackSnapshot()
				expect(t, run(t, req), "IdentityConflict")
				sameRecovery(t, before, f.ackSnapshot())
				// Record the deliberate corruption refusal difference, not a claim
				// that the old Go owner already enforced this stronger invariant.
				if e := order.NewPostgres(f.pool).AcknowledgeRecovery(bus.WithTenantID(f.ctx, f.tenant), f.acknowledgement(0), "ledger", at.Add(3*time.Minute)); e != nil {
					t.Fatal(e)
				}
				if len(f.ackSnapshot()["acks"]) != 1 {
					t.Fatal("Go corruption control did not insert pending acknowledgement")
				}
			})
		}
	})
	t.Run("outbox-and-commit-rollback", func(t *testing.T) {
		for _, stage := range []string{"outbox", "commit"} {
			t.Run(stage, func(t *testing.T) {
				f := acknowledgementFixture(t)
				primeAcknowledgements(f)
				before := f.ackSnapshot()
				req := f.ackRequest(1, "position")
				prefix, timing, table, deferred := "", "BEFORE", "outbox", ""
				if stage == "commit" {
					prefix = "CONSTRAINT "
					timing = "AFTER"
					table = "execution_recovery_cases"
					deferred = "DEFERRABLE INITIALLY DEFERRED"
				}
				ddl := "CREATE SEQUENCE ack_failure_reached; CREATE FUNCTION ack_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM nextval('ack_failure_reached'); RAISE EXCEPTION 'fixture ack failure'; END $$; CREATE " + prefix + "TRIGGER ack_failure " + timing + " INSERT OR UPDATE ON " + table + " " + deferred + " FOR EACH ROW EXECUTE FUNCTION ack_failure()"
				if _, e := f.pool.Exec(f.ctx, ddl); e != nil {
					t.Fatal(e)
				}
				want := "Database"
				if stage == "commit" {
					want = "CommitUnconfirmed"
				}
				expect(t, run(t, req), want)
				sameRecovery(t, before, f.ackSnapshot())
				var reached bool
				if e := f.pool.QueryRow(f.ctx, "SELECT is_called FROM ack_failure_reached").Scan(&reached); e != nil || !reached {
					t.Fatal("failure stage not reached")
				}
			})
		}
	})
	t.Run("tenant-same-case-positive", func(t *testing.T) {
		a := acknowledgementFixture(t)
		b := *a.fixture
		b.tenant = "tenant-b"
		b.pool = b.open(b.tenant)
		other := prepareRecovery(&b, nil, []*orderpb.Fill{recoveryFill("first", 1, 200)}, true)
		if e := order.NewPostgres(other.pool).CommitRecovery(bus.WithTenantID(other.ctx, other.tenant), other.c, at.Add(2*time.Minute)); e != nil {
			t.Fatal(e)
		}
		before := other.ackSnapshot()
		expect(t, run(t, a.ackRequest(0, "ledger")), "committed")
		sameRecovery(t, before, other.ackSnapshot())
		own := a.ackSnapshot()
		expect(t, run(t, other.ackRequest(0, "ledger")), "committed")
		expect(t, run(t, other.ackRequest(0, "position")), "committed")
		sameRecovery(t, own, a.ackSnapshot())
	})
	t.Run("missing-corrupt-and-unsafe-storage", func(t *testing.T) {
		for _, variant := range []string{"state", "oversized-state", "case", "rls"} {
			t.Run(variant, func(t *testing.T) {
				f := acknowledgementFixture(t)
				primeAcknowledgements(f)
				req := f.ackRequest(1, "position")
				q := "UPDATE orders SET state=decode('ff','hex')"
				want := "InvalidInput"
				if variant == "oversized-state" {
					q = "UPDATE orders SET state=decode(repeat('00',1048577),'hex')"
					want = "Capacity"
				}
				if variant == "case" {
					req["acknowledgement"] = wire(t, &orderpb.ExecutionRecoveryApplied{CaseId: "missing", OrderId: "order", ExecutionKey: "key", PayloadDigest: "digest", ExecutionDigest: "digest"})
					want = "NotFound"
					records := req["records"].([]map[string]any)
					var status orderpb.ExecutionRecoveryRecorded
					if e := proto.Unmarshal(records[0]["payload"].([]byte), &status); e != nil {
						t.Fatal(e)
					}
					status.CaseId = "missing"
					records[0]["payload"] = wire(t, &status)
					q = "SELECT 1"
				}
				if variant == "rls" {
					q = "ALTER TABLE execution_recovery_acks NO FORCE ROW LEVEL SECURITY"
					want = "UnsafeRole"
				}
				if _, e := f.pool.Exec(f.ctx, q); e != nil {
					t.Fatal(e)
				}
				before := f.ackSnapshot()
				expect(t, run(t, req), want)
				sameRecovery(t, before, f.ackSnapshot())
			})
		}
	})
}
