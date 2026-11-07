package recoveryconformance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/fillfact"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	commandpb "github.com/eighred/kanz/kanz-schemas-go/command/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/oms/internal/order"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestRustFeeCommandPublicationRestart(t *testing.T) {
	for _, owner := range []string{"go", "rust"} {
		t.Run(owner, func(t *testing.T) {
			f := prepareFee(t, false)
			tr := f.feeTransport(t)
			ctx := bus.WithTenantID(f.ctx, f.tenant)
			store := order.NewPostgres(f.pool)
			c, err := store.RecoveryCase(ctx, f.env.EventId)
			at := time.Now().UTC()
			if err != nil || store.ProposeFeeCorrection(ctx, c, "user:maker", "synthetic review", at.Add(-time.Hour)) != nil {
				t.Fatal("seed pending review")
			}
			p, err := store.FeeProposal(ctx, c.ID)
			if err != nil {
				t.Fatal("read pending review")
			}
			nc, err := nats.Connect(brokerURL(t))
			if err != nil {
				t.Fatal("connect independent approval observer")
			}
			defer nc.Close()
			received := make(chan *nats.Msg, 4)
			sub, err := nc.ChanSubscribe(order.SubjectFeeApproved, received)
			if err != nil {
				t.Fatal("subscribe approval observer")
			}
			defer func() { _ = sub.Unsubscribe() }()
			if nc.FlushTimeout(5*time.Second) != nil {
				t.Fatal("arm approval observer")
			}
			ddl := `CREATE SEQUENCE fee_mark_witness;
CREATE FUNCTION refuse_fee_mark() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
IF NEW.event_type='order.order.fee_correction_approved' AND NEW.published_at IS NOT NULL THEN
PERFORM nextval('fee_mark_witness'); RAISE EXCEPTION 'synthetic mark refusal'; END IF; RETURN NEW; END $$;
CREATE TRIGGER refuse_fee_mark BEFORE UPDATE ON outbox FOR EACH ROW EXECUTE FUNCTION refuse_fee_mark();`
			if _, err := f.pool.Exec(ctx, ddl); err != nil {
				t.Fatal("install isolated SQL mark refusal")
			}
			env := f.feeEnvelope(order.SubjectFeeApprove)
			cmd := &orderpb.ApproveExecutionFeeCorrection{Metadata: f.feeMetadata("user:checker"), OrderId: f.state.OrderId, CaseId: c.ID, Digest: p.Digest}
			if f.feeCommand(t, owner, tr, env, cmd, at) {
				t.Fatal("unconfirmed publication reported success")
			}
			var witness bool
			if f.pool.QueryRow(ctx, "SELECT is_called FROM fee_mark_witness").Scan(&witness) != nil || !witness {
				t.Fatal("publication failure did not reach post-ACK SQL mark")
			}
			select {
			case msg := <-received:
				e, payload, err := bus.Unframe(msg.Data)
				var approved orderpb.ExecutionFeeCorrectionApproved
				if err != nil || bus.Validate(e) != nil || e.TenantId != f.tenant || e.CausationId != env.EventId || proto.Unmarshal(payload, &approved) != nil || approved.Digest != p.Digest || approved.Approver != "user:checker" {
					t.Fatal("actual approval publication lost identity or terms")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("actual approved FACT not observed")
			}
			before, version, err := store.Load(ctx, f.state.OrderId)
			if err != nil {
				t.Fatal("read committed state after mark refusal")
			}
			c, err = store.RecoveryCase(ctx, c.ID)
			if err != nil || c.Checkpoint != 1 {
				t.Fatal("publication error lost financial commit")
			}
			var approvals, revisions, pending int
			for _, query := range []struct {
				sql string
				n   *int
			}{{"SELECT count(*) FROM execution_fee_approvals", &approvals}, {"SELECT count(*) FROM execution_fee_revisions", &revisions}, {"SELECT count(*) FROM outbox WHERE event_type='order.order.fee_correction_approved' AND published_at IS NULL", &pending}} {
				if f.pool.QueryRow(ctx, query.sql).Scan(query.n) != nil {
					t.Fatal("read unresolved committed evidence")
				}
			}
			if approvals != 1 || revisions != 1 || pending != 1 {
				t.Fatal("publication error misrepresented durable outcome")
			}
			if _, err := f.pool.Exec(ctx, "DROP TRIGGER refuse_fee_mark ON outbox"); err != nil {
				t.Fatal("remove isolated mark refusal")
			}
			if !f.feeCommand(t, owner, tr, env, cmd, at.Add(time.Minute)) {
				t.Fatal("fresh owner failed to finish pending publication")
			}
			after, nextVersion, err := store.Load(ctx, f.state.OrderId)
			if err != nil || nextVersion != version || !proto.Equal(before, after) {
				t.Fatal("publication restart repeated financial transition")
			}
			if f.pool.QueryRow(ctx, "SELECT count(*) FROM outbox WHERE published_at IS NULL").Scan(&pending) != nil || pending != 0 {
				t.Fatal("restart left unpublished committed FACTs")
			}
		})
	}
}

func TestRustFeeCommandConcurrentCommands(t *testing.T) {
	for _, operation := range []string{"proposal", "approval"} {
		t.Run(operation, func(t *testing.T) { concurrentFeeCommand(t, operation) })
	}
}
func concurrentFeeCommand(t *testing.T, operation string) {
	f := prepareFee(t, false)
	ctx := bus.WithTenantID(f.ctx, f.tenant)
	tr := f.feeTransport(t)
	goConfig := f.pool.Config().Copy()
	goName := "fee-go-" + f.schema
	goConfig.ConnConfig.RuntimeParams["application_name"] = goName
	goPool, err := pgxpool.NewWithConfig(f.ctx, goConfig)
	if err != nil {
		t.Fatal("named Go oracle pool unavailable")
	}
	t.Cleanup(goPool.Close)
	goFixture := *f.fixture
	goFixture.pool = goPool
	svc := goFixture.service(t, tr)
	w := f.start(t, tr)
	rustName := fmt.Sprintf("oms-recovery-%d", w.cmd.Process.Pid)
	store := order.NewPostgres(f.pool)
	subject, fact, table := order.SubjectFeePropose, order.SubjectFeeProposed, "execution_fee_proposals"
	var cmd proto.Message = &orderpb.ProposeExecutionFeeCorrection{Metadata: f.feeMetadata("user:maker"), OrderId: f.state.OrderId, CaseId: f.env.EventId, Reason: "synthetic concurrent review"}
	barrierQuery, barrierKey := "SELECT pg_backend_pid() FROM orders WHERE order_id=$1 FOR UPDATE", f.state.OrderId
	if operation == "approval" {
		c, err := store.RecoveryCase(ctx, f.env.EventId)
		if err != nil || store.ProposeFeeCorrection(ctx, c, "user:maker", "synthetic review", time.Now().UTC().Add(-time.Hour)) != nil {
			t.Fatal("seed concurrent approval proposal")
		}
		p, err := store.FeeProposal(ctx, c.ID)
		if err != nil {
			t.Fatal("read concurrent proposal")
		}
		cmd = &orderpb.ApproveExecutionFeeCorrection{Metadata: f.feeMetadata("user:checker"), OrderId: f.state.OrderId, CaseId: c.ID, Digest: p.Digest}
		subject, fact, table = order.SubjectFeeApprove, order.SubjectFeeApproved, "execution_fee_approvals"
		barrierQuery, barrierKey = "SELECT pg_backend_pid() FROM execution_recovery_cases WHERE case_id=$1 FOR UPDATE", c.ID
	}
	_, version, err := store.Load(ctx, f.state.OrderId)
	if err != nil {
		t.Fatal("read concurrent baseline")
	}
	gate, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal("begin isolated row barrier")
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = gate.Rollback(cleanup)
	}()
	var barrierPID int32
	if gate.QueryRow(ctx, barrierQuery, barrierKey).Scan(&barrierPID) != nil {
		t.Fatal("acquire actual order row barrier")
	}
	env := f.feeEnvelope(subject)
	payload := wire(t, cmd)
	goDone := make(chan error, 1)
	go func() { goDone <- svc.HandleFeeCorrection(lineage(f.ctx, env), env, payload) }()
	overlapped := make(chan feeLockReport, 1)
	go func() {
		watch, cancel := context.WithTimeout(f.ctx, 10*time.Second)
		defer cancel()
		report := feeLockReport{queryError: "none", releaseError: "none"}
		for watch.Err() == nil {
			snapshot, err := observeFeeLocks(watch, f.pool, barrierPID, goName, rustName)
			report.polls++
			if err != nil {
				report.queryError = goErrorCategory(err)
				break
			}
			report.last = snapshot
			report.samples++
			if snapshot.score() > report.best.score() {
				report.best = snapshot
			}
			// Soft queue blockers are valid only when both named active Lock
			// waiters have an observed path to this still-held transaction.
			if snapshot.overlapped() {
				report.observed = true
				break
			}
			select {
			case <-watch.Done():
			case <-time.After(10 * time.Millisecond):
			}
		}
		// Release even when observation fails so both actual callers can join.
		release, cancelRelease := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelRelease()
		err := gate.Commit(release)
		report.releaseError = goErrorCategory(err)
		overlapped <- report
	}()
	at := time.Now().UTC()
	request := map[string]any{"method": "FeeCorrection", "envelope": wire(t, env), "payload": payload, "at": map[string]any{"seconds": at.Unix(), "nanos": at.Nanosecond()}, "timeout_ms": 30000}
	result := w.call(request)
	report := <-overlapped
	var goResult error
	joined := false
	select {
	case goResult = <-goDone:
		joined = true
	case <-time.After(35 * time.Second):
	}
	goOutcome := "not_joined"
	if joined {
		goOutcome = goErrorCategory(goResult)
	}
	t.Logf("fee contention polls=%d samples=%d observed=%t query=%s release=%s go_joined=%t go_outcome=%s rust_outcome=%s rust_sessions_closed=%t last={%s} best={%s}", report.polls, report.samples, report.observed, report.queryError, report.releaseError, joined, goOutcome, feeRustOutcome(result), result.SessionsClosed, report.last.text(), report.best.text())
	if !joined {
		t.Fatal("Go concurrent proposal did not join")
	}
	if !report.observed || report.queryError != "none" || report.releaseError != "none" {
		t.Fatal("both independent proposal CAS operations did not reach held row")
	}
	if !result.SessionsClosed || (result.Status != "ok" && (result.Status != "error" || result.Error != "Conflict")) {
		t.Fatal("Rust concurrent proposal outcome differs")
	}
	if goResult != nil && !errors.Is(goResult, order.ErrConflict) {
		t.Fatal("Go concurrent proposal outcome differs")
	}
	if retry := w.call(request); retry.Status != "ok" || !retry.SessionsClosed {
		t.Fatal("proposal loser cannot resolve committed evidence")
	}
	tr.closeGo()
	w.close(tr)
	if _, err := store.FeeProposal(ctx, f.env.EventId); err != nil {
		t.Fatal("Go rollback reader cannot validate winner")
	}
	_, after, err := store.Load(ctx, f.state.OrderId)
	if err != nil || after != version+1 {
		t.Fatal("concurrent proposals advanced order twice")
	}
	for _, sql := range []string{"SELECT count(*) FROM " + table, "SELECT count(*) FROM outbox WHERE event_type='" + fact + "'"} {
		var count int
		if f.pool.QueryRow(ctx, sql).Scan(&count) != nil || count != 1 {
			t.Fatal("concurrent proposals duplicated immutable decision")
		}
	}
}

func (f *feeFixture) feeTransport(t *testing.T) *transport {
	t.Helper()
	return startTransport(t, &venue{tenant: f.tenant, orderID: f.state.OrderId,
		description: &venuepb.DescribeResponse{Mic: "MOCK", Account: "fixture-account", AccountVerified: true, ExchangeAccountId: "fixture-exchange-account", SupportedOrderTypes: []orderpb.OrderType{orderpb.OrderType_ORDER_TYPE_LIMIT}}, history: &venuepb.QueryOrderResponse{}})
}

func TestRustFeeCommandApprovedEvidenceDrift(t *testing.T) {
	for _, name := range []string{"history-attribution-error", "posted-unknown-block", "stale-fee-block"} {
		t.Run(name, func(t *testing.T) {
			for _, owner := range []string{"go", "rust"} {
				f := prepareFee(t, false)
				tr := f.feeTransport(t)
				ctx := bus.WithTenantID(f.ctx, f.tenant)
				store := order.NewPostgres(f.pool)
				c, err := store.RecoveryCase(ctx, f.env.EventId)
				if err != nil {
					t.Fatal("read frozen case")
				}
				at := time.Now().UTC()
				if store.ProposeFeeCorrection(ctx, c, "user:maker", "synthetic review", at.Add(-time.Hour)) != nil {
					t.Fatal("seed proposed terms")
				}
				p, err := store.FeeProposal(ctx, c.ID)
				if err != nil {
					t.Fatal("read proposed terms")
				}
				revisions := 0
				if name == "stale-fee-block" {
					previousDigest, err := fillfact.EconomicDigest(f.previous)
					if err != nil {
						t.Fatal("derive canonical original digest")
					}
					head := proto.Clone(f.previous).(*orderpb.Fill)
					head.Fee.Amount = &commonpb.Decimal{Coefficient: 15, Exponent: -2}
					head.Recovery = &orderpb.ExecutionRecoveryProvenance{CaseId: "intervening-case", FeeApproval: &orderpb.ExecutionFeeApproval{ProposalId: "intervening-case", Digest: strings.Repeat("a", 64), Proposer: "maker", Approver: "checker", ApprovedAt: timestamppb.New(at), PreviousExecutionDigest: previousDigest, PreviousFee: proto.Clone(f.previous.Fee).(*commonpb.Money)}}
					tx, err := f.pool.Begin(ctx)
					if err != nil {
						t.Fatal("begin intervening canonical revision")
					}
					if _, fresh, err := fillfact.RecordFeeRevision(ctx, tx, "oms", f.previous, head); err != nil || !fresh {
						_ = tx.Rollback(ctx)
						t.Fatal("intervening revision refused")
					}
					if tx.Commit(ctx) != nil {
						t.Fatal("commit intervening revision")
					}
					revisions = 1
				} else if name == "posted-unknown-block" {
					if _, err := f.pool.Exec(ctx, "INSERT INTO order_fills(order_id,fill_id) VALUES($1,'synthetic-orphan-claim')", f.state.OrderId); err != nil {
						t.Fatal("seed synthetic orphan claim")
					}
				} else {
					st, version, err := store.Load(ctx, f.state.OrderId)
					if err != nil {
						t.Fatal("read drift baseline")
					}
					st.VenueAccountId = "unmapped-account"
					if store.Save(ctx, st, version, nil, "") != nil {
						t.Fatal("seed synthetic stored-state drift")
					}
				}
				cmd := &orderpb.ApproveExecutionFeeCorrection{Metadata: f.feeMetadata("user:checker"), OrderId: f.state.OrderId, CaseId: c.ID, Digest: p.Digest}
				accepted := f.feeCommand(t, owner, tr, f.feeEnvelope(order.SubjectFeeApprove), cmd, at)
				if accepted != (name != "history-attribution-error") {
					t.Fatal("approval command confused propagated error and durable block")
				}
				after, err := store.RecoveryCase(ctx, c.ID)
				expectedStatus := "blocked"
				if name == "history-attribution-error" {
					expectedStatus = "investigating"
				}
				if err != nil || after.Status != expectedStatus || after.Checkpoint != 0 {
					t.Fatal("postapproval recovery status differs")
				}
				var decisions, actualRevisions, targets int
				for _, q := range []struct {
					sql string
					n   *int
				}{{"SELECT count(*) FROM execution_fee_approvals", &decisions}, {"SELECT count(*) FROM execution_fee_revisions", &actualRevisions}, {"SELECT count(*) FROM execution_recovery_targets", &targets}} {
					if f.pool.QueryRow(ctx, q.sql).Scan(q.n) != nil {
						t.Fatal("read postapproval durable outcome")
					}
				}
				if decisions != 1 || actualRevisions != revisions || targets != 0 {
					t.Fatal("recovery failure lost prior signature or changed financial journal")
				}
			}
		})
	}
}
func (f *feeFixture) feeEnvelope(subject string) *envelopepb.Envelope {
	return &envelopepb.Envelope{TenantId: f.tenant, EventId: "fee-command", EventClass: envelopepb.EventClass_EVENT_CLASS_COMMAND, EventType: subject, CorrelationId: "fixture-correlation"}
}
func (f *feeFixture) feeMetadata(actor string) *commandpb.CommandMetadata {
	return &commandpb.CommandMetadata{Issuer: actor, TargetId: f.state.OrderId, PrincipalPortfolios: []string{f.state.PortfolioId}}
}
func (f *feeFixture) feeCommand(t *testing.T, owner string, tr *transport, env *envelopepb.Envelope, payload proto.Message, at time.Time) bool {
	t.Helper()
	if owner == "go" {
		return f.service(t, tr).HandleFeeCorrection(lineage(f.ctx, env), env, wire(t, payload)) == nil
	}
	w := f.start(t, tr)
	r := w.call(map[string]any{"method": "FeeCorrection", "envelope": wire(t, env), "payload": wire(t, payload), "at": map[string]any{"seconds": at.Unix(), "nanos": at.Nanosecond()}, "timeout_ms": 30000})
	if !r.SessionsClosed {
		t.Fatal("fee command retained SQL owner")
	}
	w.close(tr)
	return r.Status == "ok"
}

// The actual Go handler is the proposal oracle. Its persisted clock is supplied
// unchanged to Rust; no new production clock seam or cloned financial algorithm.
func TestRustFeeCommandProposalParity(t *testing.T) {
	for _, count := range []int{1, 2} {
		name := "one-change"
		if count == 2 {
			name = "sorted-two-changes"
		}
		t.Run(name, func(t *testing.T) { feeProposalParity(t, count) })
	}
}
func feeProposalParity(t *testing.T, count int) {
	var expected []byte
	var proposedAt time.Time
	for _, owner := range []string{"go", "rust"} {
		f := prepareFeeCount(t, false, count)
		tr := f.feeTransport(t)
		store := order.NewPostgres(f.pool)
		_, version, err := store.Load(f.ctx, f.state.OrderId)
		if err != nil {
			t.Fatal("read proposal baseline")
		}
		cmd := &orderpb.ProposeExecutionFeeCorrection{Metadata: f.feeMetadata("user:maker"), OrderId: f.state.OrderId, CaseId: f.env.EventId, Reason: "synthetic statement verification"}
		if !f.feeCommand(t, owner, tr, f.feeEnvelope(order.SubjectFeePropose), cmd, proposedAt) {
			t.Fatal("fee proposal command refused")
		}
		p, err := store.FeeProposal(bus.WithTenantID(f.ctx, f.tenant), cmd.CaseId)
		if err != nil || len(p.Changes) != count {
			t.Fatal("Go cannot read complete proposed terms")
		}
		if owner == "go" {
			expected, proposedAt = wire(t, p), p.ProposedAt.AsTime()
		} else if !bytes.Equal(expected, wire(t, p)) {
			t.Fatal("Go/Rust canonical proposal and digest differ")
		}
		foundOriginal := false
		previousKey := ""
		for _, change := range p.Changes {
			key := fillfact.ExecutionKey(change.Previous)
			if previousKey != "" && key <= previousKey {
				t.Fatal("fee changes not sorted by canonical identity")
			}
			previousKey = key
			if proto.Equal(change.Previous, f.previous) && proto.Equal(change.Revised, f.revised) {
				foundOriginal = true
			}
		}
		if !foundOriginal {
			t.Fatal("proposal lost original execution/unknown fields")
		}
		if p.ExpiresAt.AsTime().Sub(p.ProposedAt.AsTime()) != 24*time.Hour {
			t.Fatal("proposal expiry changed")
		}
		if !f.feeCommand(t, owner, tr, f.feeEnvelope(order.SubjectFeePropose), cmd, proposedAt.Add(48*time.Hour)) {
			t.Fatal("committed proposal redelivery refused")
		}
		cmd.Reason = "different immutable terms"
		if f.feeCommand(t, owner, tr, f.feeEnvelope(order.SubjectFeePropose), cmd, proposedAt) {
			t.Fatal("conflicting proposal accepted")
		}
		st, after, err := store.Load(f.ctx, f.state.OrderId)
		if err != nil || after != version+1 || !proto.Equal(st, f.state) {
			t.Fatal("proposal CAS changed economics or repeated")
		}
		var proposals, facts int
		if f.pool.QueryRow(f.ctx, "SELECT count(*) FROM execution_fee_proposals").Scan(&proposals) != nil || f.pool.QueryRow(f.ctx, "SELECT count(*) FROM outbox WHERE event_type='order.order.fee_correction_proposed'").Scan(&facts) != nil || proposals != 1 || facts != 1 {
			t.Fatal("proposal/outbox exactly-once boundary differs")
		}
	}
}

func TestRustFeeCommandHumanPortfolioBoundary(t *testing.T) {
	for _, name := range []string{"foreign-tenant", "fact-not-command", "missing-human", "empty-human", "wrong-target", "empty-portfolios", "wrong-portfolio", "wrong-case-order"} {
		t.Run(name, func(t *testing.T) {
			for _, owner := range []string{"go", "rust"} {
				f := prepareFee(t, false)
				tr := f.feeTransport(t)
				env := f.feeEnvelope(order.SubjectFeePropose)
				cmd := &orderpb.ProposeExecutionFeeCorrection{Metadata: f.feeMetadata("user:maker"), OrderId: f.state.OrderId, CaseId: f.env.EventId, Reason: "synthetic review"}
				switch name {
				case "foreign-tenant":
					env.TenantId = "foreign"
				case "fact-not-command":
					env.EventClass = envelopepb.EventClass_EVENT_CLASS_FACT
				case "missing-human":
					cmd.Metadata.Issuer = "strategy:maker"
				case "empty-human":
					cmd.Metadata.Issuer = "user:"
				case "wrong-target":
					cmd.Metadata.TargetId = "another-order"
				case "empty-portfolios":
					cmd.Metadata.PrincipalPortfolios = nil
				case "wrong-portfolio":
					cmd.Metadata.PrincipalPortfolios = []string{"another-portfolio"}
				case "wrong-case-order":
					other := proto.Clone(f.state).(*orderpb.OrderState)
					other.OrderId = "another-order"
					if order.NewPostgres(f.pool).Save(bus.WithTenantID(f.ctx, f.tenant), other, 0, nil, "") != nil {
						t.Fatal("seed independent order for mismatched case")
					}
					cmd.OrderId, cmd.Metadata.TargetId = other.OrderId, other.OrderId
				}
				if f.feeCommand(t, owner, tr, env, cmd, instant.Add(time.Minute)) {
					t.Fatal("unattributed fee command accepted")
				}
				var count int
				if f.pool.QueryRow(f.ctx, "SELECT count(*) FROM execution_fee_proposals").Scan(&count) != nil || count != 0 {
					t.Fatal("refused command created proposal")
				}
			}
		})
	}
}

func TestRustFeeCommandApprovalLifecycle(t *testing.T) {
	proposalAt := time.Now().UTC().Add(-time.Hour)
	var approvedAt time.Time
	var expected []byte
	for _, owner := range []string{"go", "rust"} {
		f := prepareFee(t, false)
		ctx := bus.WithTenantID(f.ctx, f.tenant)
		tr := f.feeTransport(t)
		store := order.NewPostgres(f.pool)
		c, err := store.RecoveryCase(ctx, f.env.EventId)
		if err != nil || store.ProposeFeeCorrection(ctx, c, "user:maker", "synthetic review", proposalAt) != nil {
			t.Fatal("seed canonical proposal")
		}
		p, err := store.FeeProposal(ctx, c.ID)
		if err != nil {
			t.Fatal("read canonical proposal")
		}
		cmd := &orderpb.ApproveExecutionFeeCorrection{Metadata: f.feeMetadata("user:checker"), OrderId: f.state.OrderId, CaseId: c.ID, Digest: p.Digest}
		receive := f.recoveredFrom(t, 1, 35*time.Second, "fee-command")
		if !f.feeCommand(t, owner, tr, f.feeEnvelope(order.SubjectFeeApprove), cmd, approvedAt) {
			t.Fatal("approved fee lifecycle refused")
		}
		var raw []byte
		if f.pool.QueryRow(ctx, "SELECT approval FROM execution_fee_approvals WHERE case_id=$1", c.ID).Scan(&raw) != nil {
			t.Fatal("approval missing")
		}
		var approval orderpb.ExecutionFeeCorrectionApproved
		if proto.Unmarshal(raw, &approval) != nil {
			t.Fatal("approval not canonical")
		}
		if owner == "go" {
			expected, approvedAt = wire(t, &approval), approval.ApprovedAt.AsTime()
		} else if !bytes.Equal(expected, wire(t, &approval)) {
			t.Fatal("Go/Rust approval provenance differs")
		}
		after, err := store.RecoveryCase(ctx, c.ID)
		if err != nil || after.Checkpoint != 1 || after.Status != "investigating" {
			t.Fatal("approved correction not committed")
		}
		records := receive()
		if len(records) != 1 || records[0].Fill.GetRecovery().GetFeeApproval().GetDigest() != p.Digest {
			t.Fatal("recovered FACT lacks approved digest")
		}
		// Original signing time remains authoritative after expiry and after
		// financial commit. Go's existing timestamped owner is the retry oracle.
		if owner == "go" {
			if store.ApproveFeeCorrection(ctx, after, "USER:CHECKER", p.Digest, p.ExpiresAt.AsTime().Add(time.Hour)) != nil {
				t.Fatal("Go committed signature expired on replay")
			}
		} else {
			cmd.Metadata.Issuer = "user:CHECKER"
			if !f.feeCommand(t, owner, tr, f.feeEnvelope(order.SubjectFeeApprove), cmd, p.ExpiresAt.AsTime().Add(time.Hour)) {
				t.Fatal("Rust committed signature expired on replay")
			}
		}
		cmd.Metadata.Issuer = "user:another-checker"
		if f.feeCommand(t, owner, tr, f.feeEnvelope(order.SubjectFeeApprove), cmd, approvedAt) {
			t.Fatal("different person replaced committed signature")
		}
		cmd.Metadata.Issuer = "user:checker"
		cmd.Digest = "different-digest"
		if f.feeCommand(t, owner, tr, f.feeEnvelope(order.SubjectFeeApprove), cmd, approvedAt) {
			t.Fatal("changed digest replaced committed signature")
		}
		var revisions, approvals, facts int
		for _, q := range []struct {
			sql    string
			target *int
		}{{"SELECT count(*) FROM execution_fee_revisions", &revisions}, {"SELECT count(*) FROM execution_fee_approvals", &approvals}, {"SELECT count(*) FROM outbox WHERE event_type='order.order.fee_correction_approved'", &facts}} {
			if f.pool.QueryRow(ctx, q.sql).Scan(q.target) != nil {
				t.Fatal("read durable replay counts")
			}
		}
		if revisions != 1 || approvals != 1 || facts != 1 {
			t.Fatal("approval replay duplicated durable financial effects")
		}
		f.acknowledgeWithQueries(t, owner, tr, records, 0)
	}
}

func TestRustFeeCommandApprovalRefusalBlocks(t *testing.T) {
	for _, name := range []string{"same-person", "wrong-digest", "expired"} {
		t.Run(name, func(t *testing.T) {
			for _, owner := range []string{"go", "rust"} {
				f := prepareFee(t, true)
				tr := f.feeTransport(t)
				actor, digest := "user:checker", f.proposal.Digest
				reason := "fee approval refused: proposal expired; start a new investigation"
				if name == "same-person" {
					actor = "user:MAKER"
					reason = "fee approval refused: proposer and approver must be different people"
				}
				if name == "wrong-digest" {
					digest = "wrong"
					reason = "fee approval refused: digest does not match the proposed changes"
				}
				cmd := &orderpb.ApproveExecutionFeeCorrection{Metadata: f.feeMetadata(actor), OrderId: f.state.OrderId, CaseId: f.env.EventId, Digest: digest}
				if !f.feeCommand(t, owner, tr, f.feeEnvelope(order.SubjectFeeApprove), cmd, f.proposal.ExpiresAt.AsTime()) {
					t.Fatal("durable approval refusal not acknowledged")
				}
				c, err := order.NewPostgres(f.pool).RecoveryCase(f.ctx, f.env.EventId)
				if err != nil || c.Status != "blocked" || c.Reason != reason || c.Checkpoint != 0 {
					t.Fatal("approval refusal status/reason differs")
				}
				for _, table := range []string{"execution_fee_approvals", "execution_fee_revisions", "execution_recovery_targets"} {
					var count int
					if f.pool.QueryRow(f.ctx, "SELECT count(*) FROM "+table).Scan(&count) != nil || count != 0 {
						t.Fatal("refused approval changed financial evidence")
					}
				}
			}
		})
	}
}

func TestRustFeeCommandAtomicRollback(t *testing.T) {
	for _, operation := range []string{"proposal", "approval"} {
		for _, phase := range []string{"outbox-insert", "deferred-commit"} {
			t.Run(operation+"/"+phase, func(t *testing.T) {
				for _, owner := range []string{"go", "rust"} {
					f := prepareFee(t, false)
					ctx := bus.WithTenantID(f.ctx, f.tenant)
					tr := f.feeTransport(t)
					store := order.NewPostgres(f.pool)
					c, err := store.RecoveryCase(ctx, f.env.EventId)
					if err != nil {
						t.Fatal("read frozen case")
					}
					at := time.Now().UTC()
					subject, fact := order.SubjectFeePropose, order.SubjectFeeProposed
					var cmd proto.Message = &orderpb.ProposeExecutionFeeCorrection{Metadata: f.feeMetadata("user:maker"), OrderId: f.state.OrderId, CaseId: c.ID, Reason: "synthetic review"}
					if operation == "approval" {
						if store.ProposeFeeCorrection(ctx, c, "user:maker", "synthetic review", at.Add(-time.Hour)) != nil {
							t.Fatal("seed pending proposal")
						}
						p, err := store.FeeProposal(ctx, c.ID)
						if err != nil {
							t.Fatal("read pending proposal")
						}
						subject, fact = order.SubjectFeeApprove, order.SubjectFeeApproved
						cmd = &orderpb.ApproveExecutionFeeCorrection{Metadata: f.feeMetadata("user:checker"), OrderId: f.state.OrderId, CaseId: c.ID, Digest: p.Digest}
					}
					baseline, version, err := store.Load(ctx, f.state.OrderId)
					if err != nil {
						t.Fatal("read unchanged financial baseline")
					}
					// Nontransactional sequence is a synthetic witness that the
					// actual required FACT reached the failing insert/commit.
					ddl := `CREATE SEQUENCE fee_refusal_witness;
CREATE FUNCTION refuse_fee_fact() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
IF NEW.event_type='` + fact + `' THEN PERFORM nextval('fee_refusal_witness'); RAISE EXCEPTION 'synthetic refusal'; END IF;
RETURN NEW; END $$;`
					if _, err := f.pool.Exec(ctx, ddl); err != nil {
						t.Fatal("install isolated fee refusal")
					}
					trigger := "CREATE TRIGGER refuse_fee_fact BEFORE INSERT ON outbox FOR EACH ROW EXECUTE FUNCTION refuse_fee_fact()"
					if phase == "deferred-commit" {
						trigger = "CREATE CONSTRAINT TRIGGER refuse_fee_fact AFTER INSERT ON outbox DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION refuse_fee_fact()"
					}
					if _, err := f.pool.Exec(ctx, trigger); err != nil {
						t.Fatal("install isolated refusal trigger")
					}
					if f.feeCommand(t, owner, tr, f.feeEnvelope(subject), cmd, at) {
						t.Fatal("failed fee transaction reported success")
					}
					var called bool
					if f.pool.QueryRow(ctx, "SELECT is_called FROM fee_refusal_witness").Scan(&called) != nil || !called {
						t.Fatal("transaction failure did not reach required FACT")
					}
					after, nextVersion, err := store.Load(ctx, f.state.OrderId)
					if err != nil || nextVersion != version || !proto.Equal(after, baseline) {
						t.Fatal("failed fee transaction changed order CAS/economics")
					}
					nextCase, err := store.RecoveryCase(ctx, c.ID)
					if err != nil || nextCase.Version != c.Version || nextCase.Status != c.Status || nextCase.Checkpoint != c.Checkpoint {
						t.Fatal("failed approval advanced case")
					}
					var count int
					if f.pool.QueryRow(ctx, "SELECT count(*) FROM outbox WHERE event_type=$1", fact).Scan(&count) != nil || count != 0 {
						t.Fatal("failed transaction retained FACT")
					}
					table := "execution_fee_proposals"
					if operation == "approval" {
						table = "execution_fee_approvals"
					}
					if f.pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count) != nil || count != 0 {
						t.Fatal("failed transaction retained fee decision")
					}
					if _, err := f.pool.Exec(ctx, "DROP TRIGGER refuse_fee_fact ON outbox"); err != nil {
						t.Fatal("remove owned trigger")
					}
					if !f.feeCommand(t, owner, tr, f.feeEnvelope(subject), cmd, at) {
						t.Fatal("clean restart could not apply rolled-back command")
					}
				}
			})
		}
	}
}
