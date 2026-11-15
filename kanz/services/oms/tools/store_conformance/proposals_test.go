package storeconformance

import (
	"context"
	"encoding/base64"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dualcontrol"
	"github.com/eighred/kanz/internal/dualcontrol/proposalstore"
	"github.com/eighred/kanz/internal/dualcontrol/proposalstore/proposalstoretest"
	"github.com/eighred/kanz/internal/outbox"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/oms/internal/approval"
	"github.com/eighred/kanz/services/oms/internal/order"
	"google.golang.org/protobuf/proto"
)

type proposalClock struct {
	Seconds int64 `json:"seconds"`
	Nanos   int   `json:"nanos"`
}

func clock(t time.Time) proposalClock   { return proposalClock{t.Unix(), t.Nanosecond()} }
func (c proposalClock) time() time.Time { return time.Unix(c.Seconds, int64(c.Nanos)).UTC() }
func optionalClock(t time.Time) *proposalClock {
	if t.IsZero() {
		return nil
	}
	c := clock(t)
	return &c
}
func optionalTime(c *proposalClock) time.Time {
	if c == nil {
		return time.Time{}
	}
	return c.time()
}

type proposalWire struct {
	ID        string         `json:"id"`
	Subject   string         `json:"subject"`
	Portfolio string         `json:"portfolio_id"`
	Act       string         `json:"act"`
	Proposer  string         `json:"proposer"`
	Digest    string         `json:"digest"`
	Command   string         `json:"command"`
	Created   proposalClock  `json:"created_at"`
	Expires   proposalClock  `json:"expires_at"`
	Approver  string         `json:"approver"`
	Decided   *proposalClock `json:"decided_at"`
	Reason    string         `json:"refusal_reason"`
	RefusedBy string         `json:"refused_by"`
	RefusedAt *proposalClock `json:"refused_at"`
	Announced *proposalClock `json:"expiry_announced_at"`
}

func proposalJSON(t *testing.T, p order.OrderProposal) proposalWire {
	t.Helper()
	return proposalWire{p.ID, p.Subject, p.PortfolioID, string(p.Act), p.Proposer, p.Digest, base64.StdEncoding.EncodeToString(wire(t, p.Command)), clock(p.CreatedAt), clock(p.ExpiresAt), p.Approver, optionalClock(p.DecidedAt), p.RefusalReason, p.RefusedBy, optionalClock(p.RefusedAt), optionalClock(p.ExpiryAnnouncedAt)}
}
func (p proposalWire) proposal(t *testing.T) order.OrderProposal {
	t.Helper()
	raw, e := base64.StdEncoding.DecodeString(p.Command)
	if e != nil {
		t.Fatal("command encoding")
	}
	cmd := &orderpb.SubmitOrder{}
	if proto.Unmarshal(raw, cmd) != nil {
		t.Fatal("command wire")
	}
	return order.OrderProposal{Proposal: dualcontrol.Proposal{ID: p.ID, Subject: p.Subject, Act: dualcontrol.Act(p.Act), Proposer: p.Proposer, Digest: p.Digest, CreatedAt: p.Created.time(), ExpiresAt: p.Expires.time()}, Command: cmd, PortfolioID: p.Portfolio, Approver: p.Approver, DecidedAt: optionalTime(p.Decided), RefusalReason: p.Reason, RefusedBy: p.RefusedBy, RefusedAt: optionalTime(p.RefusedAt), ExpiryAnnouncedAt: optionalTime(p.Announced)}
}
func proposal(t *testing.T, id, actor string, at time.Time) order.OrderProposal {
	t.Helper()
	cmd := &orderpb.SubmitOrder{OrderId: id, PortfolioId: "portfolio"}
	p, e := approval.Propose(id, approval.TermsOfSubmit(cmd), actor, at, dualcontrol.DefaultTTL)
	if e != nil {
		t.Fatal(e)
	}
	return order.OrderProposal{Proposal: p, Command: cmd, PortfolioID: cmd.PortfolioId}
}

type rustProposals struct{ f *fixture }

func (r rustProposals) request(mode, id string, at time.Time) map[string]any {
	return map[string]any{"mode": "proposal-" + mode, "schema": r.f.schema, "tenant": r.f.tenant, "order_id": id, "at": clock(at)}
}
func proposalError(status string) error {
	switch status {
	case "committed", "proposal", "missing", "proposals", "decision":
		return nil
	case "Exists":
		return proposalstore.ErrExists
	case "InvalidInput":
		return dualcontrol.ErrMalformed
	default:
		return errors.New(status)
	}
}
func (r rustProposals) Put(_ context.Context, p order.OrderProposal) error {
	req := r.request("put", p.ID, p.CreatedAt)
	req["proposal"] = proposalJSON(r.f.t, p)
	return proposalError(run(r.f.t, req))
}
func (r rustProposals) Get(_ context.Context, id string) (order.OrderProposal, bool, error) {
	got := runResult(r.f.t, r.request("get", id, at))
	if e := proposalError(got.Status); e != nil {
		return order.OrderProposal{}, false, e
	}
	if got.Status == "missing" {
		return order.OrderProposal{}, false, nil
	}
	if got.Proposal == nil {
		r.f.t.Fatal("missing proposal")
	}
	return got.Proposal.proposal(r.f.t), true, nil
}
func (r rustProposals) decision(mode, id, actor, reason string, now time.Time, records []outbox.Record) (bool, error) {
	req := r.request(mode, id, now)
	req["other"] = actor
	req["fill"] = reason
	facts := []map[string]any{}
	for _, v := range records {
		facts = append(facts, factJSON(v))
	}
	req["records"] = facts
	got := runResult(r.f.t, req)
	if e := proposalError(got.Status); e != nil {
		return false, e
	}
	if got.Won == nil {
		r.f.t.Fatal("missing decision")
	}
	return *got.Won, nil
}

// Like the existing shared contract adapter, this fixture supplies a valid FACT
// independently of the actor under test so invalid actors reach the repository.
func proposalFact(t *testing.T, id string, now time.Time) outbox.Record {
	t.Helper()
	p, e := dualcontrol.Propose(id, dualcontrol.ActOrderSubmission, id, "fixture-maker", "fixture-digest", now.Add(-time.Minute), time.Hour)
	if e != nil {
		t.Fatal(e)
	}
	a, e := p.Approve("fixture-checker", p.Digest, now)
	if e != nil {
		t.Fatal(e)
	}
	r, e := order.NewEmitter(nil).ApprovedFact(bus.WithTenantID(context.Background(), "envelope-tenant"), a)
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func (r rustProposals) Claim(_ context.Context, id, actor string, now time.Time) (bool, error) {
	return r.decision("claim", id, actor, "", now, []outbox.Record{proposalFact(r.f.t, id, now)})
}
func (r rustProposals) list(mode string, now time.Time) ([]order.OrderProposal, error) {
	got := runResult(r.f.t, r.request(mode, "", now))
	if e := proposalError(got.Status); e != nil {
		return nil, e
	}
	out := make([]order.OrderProposal, len(got.Proposals))
	for i, p := range got.Proposals {
		out[i] = p.proposal(r.f.t)
	}
	return out, nil
}
func (r rustProposals) Pending(_ context.Context, now time.Time) ([]order.OrderProposal, error) {
	return r.list("pending", now)
}
func (r rustProposals) Expired(_ context.Context, now time.Time) ([]order.OrderProposal, error) {
	return r.list("expired", now)
}

func TestRustProposalSharedContract(t *testing.T) {
	fixtureBinary(t)
	proposalstoretest.Run(t, proposalstoretest.Harness[order.OrderProposal]{New: func(t *testing.T) proposalstoretest.Store[order.OrderProposal] { return rustProposals{newFixture(t)} }, Build: proposal, RefusesSelfApprovalAtTheStore: true})
}

func TestRustProposalTransactions(t *testing.T) {
	t.Run("whole-result-row-and-aggregate-bounds", func(t *testing.T) {
		f := newFixture(t)
		r := rustProposals{f}
		p := proposal(t, "seed", "maker", at)
		if e := r.Put(f.ctx, p); e != nil {
			t.Fatal(e)
		}
		if _, e := f.pool.Exec(f.ctx, `INSERT INTO order_proposals (tenant_id,order_id,portfolio_id,act,proposer,digest,command,created_at,expires_at)
		SELECT tenant_id,'extra-'||lpad(i::text,5,'0'),portfolio_id,act,proposer,digest,command,created_at,expires_at FROM order_proposals CROSS JOIN generate_series(1,4096) AS i WHERE order_id='seed'`); e != nil {
			t.Fatal(e)
		}
		expect(t, run(t, r.request("pending", "", at)), "Capacity")
		if _, e := f.pool.Exec(f.ctx, "DELETE FROM order_proposals WHERE order_id LIKE 'extra-%'"); e != nil {
			t.Fatal(e)
		}
		if _, e := f.pool.Exec(f.ctx, `INSERT INTO order_proposals (tenant_id,order_id,portfolio_id,act,proposer,digest,command,created_at,expires_at)
		SELECT tenant_id,'extra-'||lpad(i::text,5,'0'),portfolio_id,act,proposer,repeat('x',4096),command,created_at,expires_at FROM order_proposals CROSS JOIN generate_series(1,300) AS i WHERE order_id='seed'`); e != nil {
			t.Fatal(e)
		}
		expect(t, run(t, r.request("pending", "", at)), "Capacity")
		req := r.request("expired", "", p.ExpiresAt)
		req["limit"] = 301
		expect(t, run(t, req), "Capacity")
	})
	t.Run("concurrent-claim-expiry-one-terminal", func(t *testing.T) {
		f := newFixture(t)
		r := rustProposals{f}
		p := proposal(t, "race-expiry", "maker", at)
		if e := r.Put(f.ctx, p); e != nil {
			t.Fatal(e)
		}
		after := p.ExpiresAt.Add(time.Second)
		goStore := order.NewPostgres(f.pool).Proposals()
		start := make(chan struct{})
		results := make(chan bool, 2)
		failures := make(chan error, 2)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			won, e := r.Claim(f.ctx, p.ID, "checker", after)
			results <- won
			failures <- e
		}()
		go func() {
			defer wg.Done()
			<-start
			won, e := goStore.AnnounceExpiry(f.ctx, p.ID, after, []outbox.Record{proposalFact(t, p.ID, after)})
			results <- won
			failures <- e
		}()
		close(start)
		wg.Wait()
		close(results)
		close(failures)
		wins := 0
		for v := range results {
			if v {
				wins++
			}
		}
		for e := range failures {
			if e != nil {
				t.Fatal(e)
			}
		}
		if wins != 1 {
			t.Fatal("two terminal winners")
		}
		got, found, e := r.Get(f.ctx, p.ID)
		if e != nil || !found || (got.Approver != "") == (!got.ExpiryAnnouncedAt.IsZero()) {
			t.Fatal("terminal markers disagree")
		}
		var n int
		if e = f.pool.QueryRow(f.ctx, "SELECT count(*) FROM outbox").Scan(&n); e != nil || n != 1 {
			t.Fatal("terminal outbox count")
		}
	})
	t.Run("command-and-required-fact-refusal", func(t *testing.T) {
		f := newFixture(t)
		r := rustProposals{f}
		for _, bad := range []string{"nil", "different-order", "different-subject"} {
			p := proposal(t, bad, "maker", at)
			switch bad {
			case "nil":
				p.Command = nil
			case "different-order":
				p.Command.OrderId = "other"
			case "different-subject":
				p.Subject = "other"
			}
			if e := r.Put(f.ctx, p); e == nil {
				t.Fatal("malformed held command accepted")
			}
		}
		p := proposal(t, "fact-required", "maker", at)
		if e := r.Put(f.ctx, p); e != nil {
			t.Fatal(e)
		}
		if _, e := r.decision("claim", p.ID, "checker", "", at, nil); e == nil {
			t.Fatal("signature without mandatory FACT")
		}
		if _, e := r.decision("refusal", p.ID, "checker", "", at, nil); e == nil {
			t.Fatal("half refusal")
		}
		got, found, e := r.Get(f.ctx, p.ID)
		if e != nil || !found || got.Approver != "" {
			t.Fatal("refusal consumed record")
		}
	})
	t.Run("inclusive-expiry-and-default-page", func(t *testing.T) {
		f := newFixture(t)
		r := rustProposals{f}
		p := proposal(t, "a", "maker", at)
		if e := r.Put(f.ctx, p); e != nil {
			t.Fatal(e)
		}
		if _, e := f.pool.Exec(f.ctx, `INSERT INTO order_proposals (tenant_id,order_id,portfolio_id,act,proposer,digest,command,created_at,expires_at)
		SELECT tenant_id,'extra-'||lpad(i::text,3,'0'),portfolio_id,act,proposer,digest,command,created_at,expires_at FROM order_proposals CROSS JOIN generate_series(1,100) AS i WHERE order_id='a'`); e != nil {
			t.Fatal(e)
		}
		// Canonical readers decode commands but do not re-validate stored orderID;
		// this fixture retains that distinction while qualifying queue pagination.
		goStore := order.NewPostgres(f.pool).Proposals()
		want, e := goStore.ExpiredUnannounced(f.ctx, p.ExpiresAt, 0)
		if e != nil || len(want) != 100 {
			t.Fatal("Go default expiry page")
		}
		got, e := r.Expired(f.ctx, p.ExpiresAt)
		if e != nil || len(got) != 100 {
			t.Fatal("Rust default expiry page")
		}
		for i := range want {
			if want[i].ID != got[i].ID {
				t.Fatal("expiry total order")
			}
		}
		pending, e := r.Pending(f.ctx, p.ExpiresAt)
		if e != nil || len(pending) != 0 {
			t.Fatal("inclusive expiry")
		}
		req := r.request("expired", "", p.ExpiresAt)
		req["limit"] = 4097
		expect(t, run(t, req), "Capacity")
	})
	t.Run("hold-fact-duplicate-and-rollback", func(t *testing.T) {
		f := newFixture(t)
		r := rustProposals{f}
		p := proposal(t, "held", "maker", at)
		req := r.request("put", p.ID, at)
		req["proposal"] = proposalJSON(t, p)
		req["records"] = []map[string]any{factJSON(proposalFact(t, p.ID, at))}
		expect(t, run(t, req), "committed")
		expect(t, run(t, req), "Exists")
		var held, orders, facts int
		if e := f.pool.QueryRow(f.ctx, "SELECT (SELECT count(*) FROM order_proposals),(SELECT count(*) FROM orders),(SELECT count(*) FROM outbox)").Scan(&held, &orders, &facts); e != nil || held != 1 || orders != 0 || facts != 1 {
			t.Fatal("held proposal is not an admitted order; duplicate wrote facts")
		}
		p = proposal(t, "bad-fact", "maker", at)
		req["proposal"] = proposalJSON(t, p)
		bad := proposalFact(t, p.ID, at)
		bad.TenantID = ""
		req["records"] = []map[string]any{factJSON(bad)}
		expect(t, run(t, req), "InvalidInput")
		if _, found, e := r.Get(f.ctx, p.ID); e != nil || found {
			t.Fatal("failed outbox retained proposal")
		}
	})
	t.Run("concurrent-go-rust-claim-one-fact", func(t *testing.T) {
		f := newFixture(t)
		r := rustProposals{f}
		p := proposal(t, "race", "maker", at)
		goStore := order.NewPostgres(f.pool).Proposals()
		if e := goStore.Put(f.ctx, p, nil); e != nil {
			t.Fatal(e)
		}
		var wg sync.WaitGroup
		start := make(chan struct{})
		results := make(chan bool, 2)
		failures := make(chan error, 2)
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			won, e := r.Claim(f.ctx, p.ID, "rust-checker", at.Add(time.Minute))
			results <- won
			failures <- e
		}()
		go func() {
			defer wg.Done()
			<-start
			won, e := goStore.Claim(f.ctx, p.ID, "go-checker", at.Add(time.Minute), []outbox.Record{proposalFact(t, p.ID, at.Add(time.Minute))})
			results <- won
			failures <- e
		}()
		close(start)
		wg.Wait()
		close(results)
		close(failures)
		wins := 0
		for won := range results {
			if won {
				wins++
			}
		}
		for e := range failures {
			if e != nil {
				t.Fatal(e)
			}
		}
		if wins != 1 {
			t.Fatal("claim arbitration")
		}
		var facts int
		if e := f.pool.QueryRow(f.ctx, "SELECT count(*) FROM outbox").Scan(&facts); e != nil || facts != 1 {
			t.Fatal("one second-signature fact required")
		}
		got, found, e := r.Get(f.ctx, p.ID)
		if e != nil || !found || got.Approver == "" || got.DecidedAt.IsZero() {
			t.Fatal("durable decision missing")
		}
	})
	t.Run("claim-expiry-exclusion-and-caller-clock", func(t *testing.T) {
		f := newFixture(t)
		r := rustProposals{f}
		goStore := order.NewPostgres(f.pool).Proposals()
		p := proposal(t, "expired", "maker", at)
		if e := r.Put(f.ctx, p); e != nil {
			t.Fatal(e)
		}
		after := p.ExpiresAt.Add(time.Minute)
		won, e := r.decision("announce", p.ID, "", "", after, []outbox.Record{proposalFact(t, p.ID, after)})
		if e != nil || !won {
			t.Fatal("expiry announce")
		}
		if won, e = r.Claim(f.ctx, p.ID, "checker", after); e != nil || won {
			t.Fatal("announced expiry resurrected")
		}
		p = proposal(t, "signed", "maker", at)
		if e = r.Put(f.ctx, p); e != nil {
			t.Fatal(e)
		}
		// Repository Claim intentionally has no independent clock rule. The
		// production caller must first enforce canonical dualcontrol.Approve.
		if won, e = r.Claim(f.ctx, p.ID, "checker", after); e != nil || !won {
			t.Fatal("changed caller-time repository contract")
		}
		if won, e = goStore.AnnounceExpiry(f.ctx, p.ID, after, []outbox.Record{proposalFact(t, p.ID, after)}); e != nil || won {
			t.Fatal("signed proposal expired")
		}
		p = proposal(t, "go-clock", "maker", at)
		if e = goStore.Put(f.ctx, p, nil); e != nil {
			t.Fatal(e)
		}
		if won, e = goStore.Claim(f.ctx, p.ID, "checker", after, []outbox.Record{proposalFact(t, p.ID, after)}); e != nil || !won {
			t.Fatal("Go clock oracle differs")
		}
		if _, e = p.Approve("checker", p.Digest, after); !errors.Is(e, dualcontrol.ErrExpired) {
			t.Fatal("mandatory caller expiry guard absent")
		}
	})
	t.Run("mutation-outbox-and-commit-rollback", func(t *testing.T) {
		f := newFixture(t)
		r := rustProposals{f}
		p := proposal(t, "rollback", "maker", at)
		if e := r.Put(f.ctx, p); e != nil {
			t.Fatal(e)
		}
		bad := proposalFact(t, p.ID, at)
		bad.TenantID = ""
		for _, mode := range []string{"claim", "announce"} {
			now := p.ExpiresAt.Add(time.Second)
			req := r.request(mode, p.ID, now)
			req["other"] = "checker"
			req["records"] = []map[string]any{factJSON(bad)}
			expect(t, run(t, req), "InvalidInput")
			got, found, e := r.Get(f.ctx, p.ID)
			if e != nil || !found || got.Approver != "" || !got.ExpiryAnnouncedAt.IsZero() {
				t.Fatal("failed outbox retained marker")
			}
		}
		if _, e := f.pool.Exec(f.ctx, `CREATE FUNCTION refuse_proposal_commit() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'fixture'; END$$;CREATE CONSTRAINT TRIGGER refuse_proposal_commit AFTER UPDATE ON order_proposals DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION refuse_proposal_commit()`); e != nil {
			t.Fatal(e)
		}
		for _, mode := range []string{"claim", "announce", "refusal"} {
			req := r.request(mode, p.ID, p.ExpiresAt.Add(time.Second))
			req["other"] = "checker"
			req["fill"] = "reason"
			req["records"] = []map[string]any{factJSON(proposalFact(t, p.ID, p.ExpiresAt.Add(time.Second)))}
			expect(t, run(t, req), "CommitUnconfirmed")
		}
		got, found, e := r.Get(f.ctx, p.ID)
		if e != nil || !found || got.Approver != "" || got.RefusalReason != "" || !got.ExpiryAnnouncedAt.IsZero() {
			t.Fatal("injected commit rejection retained mutations")
		}
	})
	t.Run("tenant-rls-and-bounded-storage", func(t *testing.T) {
		f := newFixture(t)
		r := rustProposals{f}
		p := proposal(t, "same", "maker", at)
		if e := r.Put(f.ctx, p); e != nil {
			t.Fatal(e)
		}
		req := r.request("get", p.ID, at)
		req["tenant"] = "tenant-b"
		expect(t, run(t, req), "missing")
		req["mode"] = "proposal-put"
		req["proposal"] = proposalJSON(t, p)
		expect(t, run(t, req), "committed")
		b := order.NewPostgres(f.open("tenant-b")).Proposals()
		if _, found, e := b.Get(f.ctx, p.ID); e != nil || !found {
			t.Fatal("tenant B positive")
		}
		if _, e := f.pool.Exec(f.ctx, "UPDATE order_proposals SET command=decode('ff','hex')"); e != nil {
			t.Fatal(e)
		}
		expect(t, run(t, r.request("get", p.ID, at)), "InvalidInput")
		if _, e := f.pool.Exec(f.ctx, "UPDATE order_proposals SET command=repeat('x',1048577)::bytea"); e != nil {
			t.Fatal(e)
		}
		expect(t, run(t, r.request("get", p.ID, at)), "Capacity")
		expect(t, run(t, r.request("pending", "", at)), "Capacity")
		if _, e := f.pool.Exec(f.ctx, "ALTER TABLE order_proposals NO FORCE ROW LEVEL SECURITY"); e != nil {
			t.Fatal(e)
		}
		expect(t, run(t, r.request("get", p.ID, at)), "UnsafeRole")
	})
	t.Run("go-roundtrip-raw-command-refusal", func(t *testing.T) {
		f := newFixture(t)
		r := rustProposals{f}
		p := proposal(t, "held", "maker", at)
		p.Command.ProtoReflect().SetUnknown([]byte{0xf8, 0x07, 0x01})
		if e := r.Put(f.ctx, p); e != nil {
			t.Fatal(e)
		}
		goStore := order.NewPostgres(f.pool).Proposals()
		got, found, e := goStore.Get(f.ctx, p.ID)
		if e != nil || !found || !proto.Equal(p.Command, got.Command) {
			t.Fatal("canonical Go readback")
		}
		won, e := r.decision("refusal", p.ID, "maker", "self_approval", at.Add(time.Minute), nil)
		if e != nil || !won {
			t.Fatal("refusal missing")
		}
		got, found, e = goStore.Get(f.ctx, p.ID)
		if e != nil || !found || got.Approver != "" || !got.DecidedAt.IsZero() || got.RefusalReason != "self_approval" {
			t.Fatal("refusal consumed proposal")
		}
		other, e := r.Pending(f.ctx, at.Add(time.Minute))
		if e != nil || len(other) != 1 || !reflect.DeepEqual(proposalJSON(t, got), proposalJSON(t, other[0])) {
			t.Fatal("metadata parity")
		}
	})
}
