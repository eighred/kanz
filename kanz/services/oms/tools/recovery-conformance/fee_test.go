package recoveryconformance

import (
	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/fillfact"
	"github.com/eighred/kanz/internal/outbox"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/oms/internal/order"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"strings"
	"testing"
	"time"
)

type feeFixture struct {
	*fixture
	state             *orderpb.OrderState
	env               *envelopepb.Envelope
	payload           []byte
	previous, revised *orderpb.Fill
	proposal          *orderpb.ExecutionFeeCorrectionProposed
}

// Seed exclusively through existing Go state/journal/history/proposal owners.
// Synthetic maker/checker subjects qualify durable revalidation, not the live
// authentication or human-approval boundary, which remains Go-owned.
func prepareFee(t *testing.T, persistProposal bool) *feeFixture {
	t.Helper()
	f := newFixture(t)
	ctx := bus.WithTenantID(f.ctx, f.tenant)
	store := order.NewPostgres(f.pool)
	st := initial()
	st.Status = orderpb.OrderStatus_ORDER_STATUS_ROUTED
	st.LeavesQuantity = proto.Clone(st.OrderedQuantity).(*commonpb.Decimal)
	if store.Create(ctx, st, nil) != nil {
		t.Fatal("seed order failed")
	}
	old := history(st).Fills[0]
	next, err := order.ApplyFill(st, old, instant)
	if err != nil {
		t.Fatal(err)
	}
	fact, err := order.NewEmitter(nil).FillFact(ctx, old, next)
	if err != nil {
		t.Fatal(err)
	}
	if store.Save(ctx, next, 0, []outbox.Record{fact}, old.FillId) != nil {
		t.Fatal("seed execution failed")
	}
	st, _, err = order.Cancel(next, instant)
	if err != nil {
		t.Fatal(err)
	}
	if store.Save(ctx, st, 1, nil, "") != nil {
		t.Fatal("seed terminal state failed")
	}
	revised := proto.Clone(old).(*orderpb.Fill)
	revised.Fee.Amount.Coefficient = 2
	env := &envelopepb.Envelope{EventId: "observation", TenantId: f.tenant, EventType: execution.SubjectStateHealed, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, CorrelationId: "fixture-correlation"}
	payload := wire(t, &orderpb.StateHealed{OrderId: st.OrderId, Venue: st.Venue, State: st, Reason: "synthetic fee correction", DetectedAt: timestamppb.New(instant)})
	c, err := store.ObserveRecovery(ctx, env.EventId, st.OrderId, env.EventId, payload)
	if err != nil {
		t.Fatal(err)
	}
	mapping, err := store.RecordRecoveryMapping(ctx, order.RecoveryMapping{Tenant: f.tenant, Portfolio: st.PortfolioId, Venue: st.Venue, Account: st.VenueAccountId, ExchangeAccount: "fixture-exchange-account"}, execution.AccountProof{Verified: true, ExchangeAccountID: "fixture-exchange-account"})
	if err != nil {
		t.Fatal(err)
	}
	if store.FreezeRecoveryHistory(ctx, c, mapping, st, execution.OrderView{State: execution.OrderViewCancelled, ExecutedQuantity: old.Quantity, Fills: []*orderpb.Fill{revised}}, instant) != nil {
		t.Fatal("freeze authoritative venue history failed")
	}
	c, err = store.RecoveryCase(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !persistProposal {
		return &feeFixture{f, st, env, payload, old, revised, nil}
	}
	if store.ProposeFeeCorrection(ctx, c, "user:maker", "synthetic statement verification", instant.Add(time.Minute)) != nil {
		t.Fatal("canonical proposal failed")
	}
	proposal, err := store.FeeProposal(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	return &feeFixture{f, st, env, payload, old, revised, proposal}
}

func TestRustRecoveryPersistedFeeApproval(t *testing.T) {
	for _, name := range []string{"approved", "stale-approved-baseline", "missing-approval", "same-actor-approval", "expired-approval", "exact-expiry-approval", "immediately-before-expiry", "proposal-unknown-bound", "proposal-unknown-unbound", "invalid-change-before-valid", "duplicate-valid-change", "proposal-wrong-history", "proposal-wrong-mapping"} {
		t.Run(name, func(t *testing.T) {
			for _, owner := range []string{"go", "rust"} {
				f := prepareFee(t, true)
				injectedProposal := strings.HasPrefix(name, "proposal-") || strings.Contains(name, "change")
				if injectedProposal {
					original := f.proposal
					f = prepareFee(t, false)
					f.proposal = proto.Clone(original).(*orderpb.ExecutionFeeCorrectionProposed)
					switch name {
					case "proposal-unknown-bound", "proposal-unknown-unbound":
						f.proposal.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 1000, protowire.VarintType), 17))
					case "invalid-change-before-valid":
						bad := proto.Clone(f.proposal.Changes[0]).(*orderpb.ExecutionFeeChange)
						bad.Previous.Fee.Amount.Coefficient = 3
						f.proposal.Changes = append([]*orderpb.ExecutionFeeChange{bad}, f.proposal.Changes...)
					case "duplicate-valid-change":
						f.proposal.Changes = append(f.proposal.Changes, proto.Clone(f.proposal.Changes[0]).(*orderpb.ExecutionFeeChange))
					case "proposal-wrong-history":
						f.proposal.HistoryDigest = strings.Repeat("a", 64)
					case "proposal-wrong-mapping":
						f.proposal.MappingVersion = strings.Repeat("b", 64)
					}
					if name != "proposal-unknown-unbound" {
						digest, err := fillfact.FeeProposalDigest(f.tenant, f.proposal)
						if err != nil {
							t.Fatal("canonical synthetic proposal hash failed")
						}
						f.proposal.Digest = digest
					}
					if _, err := f.pool.Exec(f.ctx, "INSERT INTO execution_fee_proposals(case_id,order_id,digest,proposal) VALUES($1,$2,$3,$4)", "observation", f.state.OrderId, f.proposal.Digest, wire(t, f.proposal)); err != nil {
						t.Fatal("insert immutable synthetic proposal failed")
					}
				}
				ctx := bus.WithTenantID(f.ctx, f.tenant)
				store := order.NewPostgres(f.pool)
				c, err := store.RecoveryCase(ctx, "observation")
				if err != nil {
					t.Fatal(err)
				}
				if name == "approved" || name == "stale-approved-baseline" {
					if store.ApproveFeeCorrection(ctx, c, "user:checker", f.proposal.Digest, instant.Add(2*time.Minute)) != nil {
						t.Fatal("canonical second actor approval failed")
					}
				} else if name != "missing-approval" {
					approver := "user:checker"
					approvedAt := f.proposal.ExpiresAt.AsTime()
					if injectedProposal {
						approvedAt = instant.Add(2 * time.Minute)
					}
					if name == "same-actor-approval" {
						approver = "  USER:MAKER  "
						approvedAt = instant.Add(2 * time.Minute)
					}
					if name == "expired-approval" {
						approvedAt = approvedAt.Add(time.Nanosecond)
					}
					if name == "immediately-before-expiry" {
						approvedAt = approvedAt.Add(-time.Nanosecond)
					}
					injected := &orderpb.ExecutionFeeCorrectionApproved{CaseId: c.ID, OrderId: c.OrderID, PortfolioId: f.state.PortfolioId, Digest: f.proposal.Digest, Proposer: f.proposal.Proposer, Approver: approver, ApprovedAt: timestamppb.New(approvedAt)}
					// Insert a corrupt persisted witness without disabling immutable triggers.
					if _, err = f.pool.Exec(ctx, "INSERT INTO execution_fee_approvals(case_id,approval) VALUES($1,$2)", c.ID, wire(t, injected)); err != nil {
						t.Fatal("insert synthetic approval witness failed")
					}
				}
				if name == "stale-approved-baseline" {
					previousDigest, err := fillfact.EconomicDigest(f.previous)
					if err != nil {
						t.Fatal(err)
					}
					head := proto.Clone(f.previous).(*orderpb.Fill)
					head.Fee.Amount = &commonpb.Decimal{Coefficient: 15, Exponent: -2}
					head.Recovery = &orderpb.ExecutionRecoveryProvenance{CaseId: "intervening-case", FeeApproval: &orderpb.ExecutionFeeApproval{ProposalId: "intervening-case", Digest: strings.Repeat("a", 64), Proposer: "maker", Approver: "checker", ApprovedAt: timestamppb.New(instant), PreviousExecutionDigest: previousDigest, PreviousFee: proto.Clone(f.previous.Fee).(*commonpb.Money)}}
					tx, err := f.pool.Begin(ctx)
					if err != nil {
						t.Fatal("begin canonical intervening revision failed")
					}
					if _, fresh, err := fillfact.RecordFeeRevision(ctx, tx, "oms", f.previous, head); err != nil || !fresh {
						_ = tx.Rollback(ctx)
						t.Fatal("canonical intervening fee revision failed")
					}
					if tx.Commit(ctx) != nil {
						t.Fatal("intervening revision commit failed")
					}
				}
				v := &venue{tenant: f.tenant, orderID: f.state.OrderId, description: &venuepb.DescribeResponse{Mic: "MOCK", Account: "fixture-account", AccountVerified: true, ExchangeAccountId: "fixture-exchange-account", SupportedOrderTypes: []orderpb.OrderType{orderpb.OrderType_ORDER_TYPE_LIMIT}}, history: &venuepb.QueryOrderResponse{}}
				tr := startTransport(t, v)
				admitted := name == "approved" || name == "immediately-before-expiry" || name == "proposal-unknown-bound" || name == "duplicate-valid-change"
				var receive func() []*orderpb.ExecutionRecovered
				if admitted {
					receive = f.recovered(t, 1)
				}
				if !f.invoke(t, owner, tr, "Observation", f.env, f.payload) {
					t.Fatal("durable fee lifecycle did not acknowledge handler")
				}
				after, err := store.RecoveryCase(ctx, c.ID)
				if err != nil {
					t.Fatal(err)
				}
				var revisions, targets int
				if f.pool.QueryRow(ctx, "SELECT count(*) FROM execution_fee_revisions").Scan(&revisions) != nil || f.pool.QueryRow(ctx, "SELECT count(*) FROM execution_recovery_targets").Scan(&targets) != nil {
					t.Fatal("fee durable read failed")
				}
				expectedRevisions := 0
				if name == "stale-approved-baseline" {
					expectedRevisions = 1
				}
				if admitted {
					if after.Status != "investigating" || after.Checkpoint != 1 || revisions != 1 || targets != 1 {
						t.Fatal("approved fee was not applied exactly once")
					}
					records := receive()
					approval := records[0].Fill.GetRecovery().GetFeeApproval()
					if approval == nil || approval.PreviousFee.Amount.Coefficient != 1 || records[0].Fill.Fee.Amount.Coefficient != 2 {
						t.Fatal("approved previous/new fee evidence differs")
					}
					var previous, revised []byte
					if f.pool.QueryRow(ctx, "SELECT previous_fill,revised_fill FROM execution_fee_revisions").Scan(&previous, &revised) != nil {
						t.Fatal("fee journal missing")
					}
					var a, b orderpb.Fill
					if proto.Unmarshal(previous, &a) != nil || proto.Unmarshal(revised, &b) != nil || !fillfact.SameExecution(&a, f.previous) || !fillfact.SameExecution(&b, f.revised) {
						t.Fatal("fee journal economics differ")
					}
				} else if after.Status != "blocked" || after.Checkpoint != 0 || revisions != expectedRevisions || targets != 0 {
					t.Fatal("invalid/missing approval altered financial owners")
				}
				if v.queries.Load() != 0 || v.writes.Load() != 0 {
					t.Fatal("frozen evidence was re-queried or traded")
				}
			}
		})
	}
}
