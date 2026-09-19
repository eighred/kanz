package collateralops

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	pb "github.com/eighred/kanz/kanz-schemas-go/collateral/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func number(n int64) *commonpb.Decimal { return &commonpb.Decimal{Coefficient: n} }
func fixture() *pb.WorkflowSnapshot {
	now := time.Now().UTC().Add(-time.Minute)
	return &pb.WorkflowSnapshot{SnapshotId: "snap-1", PortfolioId: "PF1", CurrencyCode: "USD", PositionsVersion: "positions-1", ValuationsVersion: "valuation-1", CashForecastVersion: "cash-1", ExposureModelVersion: "model-1", AsOf: timestamppb.New(now), ValidUntil: timestamppb.New(now.Add(time.Hour)),
		Inventory:  []*pb.WorkflowInventory{{LotId: "lot-A", AssetId: "cash", IssuerId: "issuer", CustodianId: "custodian", AccountId: "account", Quantity: number(100), UnitValue: number(1), QuantityIncrement: number(1), OpportunityCost: number(1), LiquidityBudget: number(100), AvailableAt: timestamppb.New(now)}},
		Agreements: []*pb.WorkflowAgreement{{AgreementId: "CSA1", Version: "v1", CounterpartyId: "CP1", Exposure: number(60), Held: number(0), InitialMargin: number(0), Threshold: number(0), MinimumTransfer: number(0), IndependentAmount: number(0), Rounding: number(0), IssuerLimit: number(100), SettlementDeadline: timestamppb.New(now.Add(2 * time.Hour)), Schedule: []*pb.WorkflowEligibility{{AssetId: "cash", Eligible: true, WrongWayRiskCleared: true, Haircut: number(0)}}}},
	}
}

func database(t *testing.T) (*Store, func(string) *Store) {
	t.Helper()
	url := os.Getenv("TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("requires real PostgreSQL via TEST_POSTGRES_URL")
	}
	base, err := pgxpool.New(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	var privileged bool
	if err = base.QueryRow(t.Context(), `SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&privileged); err != nil || privileged {
		t.Fatalf("RLS role required: %v %v", privileged, err)
	}
	// Accounting migration history targets the shared test schema. Like the
	// custody/ledger suites, run with -p 1, never against a production database.
	if _, err = base.Exec(t.Context(), `DROP TABLE IF EXISTS collateral_allocation_proofs,collateral_confirmations,collateral_requests,collateral_reservations,collateral_active_agreements,collateral_workflows,collateral_snapshots,collateral_lots,custody_actions,custody_statements,custody_runs,custody_breaks,ledger_entries,ledger_snapshots,outbox CASCADE`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(base.Close)
	connect := func(tenant string) *Store {
		cfg, err := pgxpool.ParseConfig(url)
		if err != nil {
			t.Fatal(err)
		}
		cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
			_, err := c.Exec(ctx, `SELECT set_config('app.tenant_id',$1,false)`, tenant)
			return err
		}
		pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		return New(pool)
	}
	s := connect("tenant-A")
	files, err := filepath.Glob("../../migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	for _, file := range files {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.pool.Exec(t.Context(), string(b)); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
	}
	return s, connect
}

func propose(t *testing.T, s *Store) *pb.WorkflowRecorded {
	t.Helper()
	if err := s.ImportSnapshot(t.Context(), "tenant-A", fixture()); err != nil {
		t.Fatal(err)
	}
	state, err := s.Apply(t.Context(), Action{Tenant: "tenant-A", Actor: "maker", RequestID: "propose-1", WorkflowID: "workflow-1", SnapshotID: "snap-1", Kind: "propose"})
	if err != nil {
		t.Fatal(err)
	}
	return state
}
func act(t *testing.T, s *Store, state *pb.WorkflowRecorded, actor, kind string) *pb.WorkflowRecorded {
	t.Helper()
	result, err := s.Apply(t.Context(), Action{Tenant: "tenant-A", Actor: actor, RequestID: fmt.Sprintf("%s-%d", kind, state.Revision), WorkflowID: state.WorkflowId, ExpectedRevision: state.Revision, Kind: kind, Explanation: "reviewed source evidence"})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func confirmed(state *pb.WorkflowRecorded, id string, quantity int64, returned bool) *pb.PostingConfirmation {
	revision := state.PostingRevision
	if returned {
		revision = state.ReturnRevision
	}
	return &pb.PostingConfirmation{SourceId: id, WorkflowId: state.WorkflowId, AgreementId: "CSA1", LotId: "lot-A", CustodianId: "custodian", AccountId: "account", CumulativeQuantity: number(quantity), Returned: returned, InstructionRevision: revision, SettledAt: timestamppb.Now()}
}
func reload(t *testing.T, s *Store) *pb.WorkflowRecorded {
	t.Helper()
	state, err := s.Load(t.Context(), "tenant-A", "workflow-1")
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestPostgresCollateralCompleteLifecycle(t *testing.T) {
	s, other := database(t)
	state := propose(t, s)
	if state.Status != pb.WorkflowStatus_WORKFLOW_STATUS_PROPOSED || len(state.Legs) != 1 {
		t.Fatalf("%+v", state)
	}
	if _, err := other("tenant-B").Load(t.Context(), "tenant-B", state.WorkflowId); !errors.Is(err, ErrNotFound) {
		t.Fatalf("RLS leaked: %v", err)
	}
	if _, err := s.Load(t.Context(), "tenant-B", state.WorkflowId); !errors.Is(err, ErrNotFound) {
		t.Fatalf("tenant envelope mismatch: %v", err)
	}
	if _, err := s.Apply(t.Context(), Action{Tenant: "tenant-A", Actor: "maker", RequestID: "self-approve", WorkflowID: state.WorkflowId, Kind: "approve", ExpectedRevision: state.Revision}); !errors.Is(err, ErrTransition) {
		t.Fatalf("self approved: %v", err)
	}
	state = act(t, s, state, "checker", "approve")
	partial := confirmed(state, "settle-partial", 30, false)
	if err := s.Confirm(t.Context(), "tenant-A", partial); err != nil {
		t.Fatal(err)
	}
	state = reload(t, s)
	if state.Status != pb.WorkflowStatus_WORKFLOW_STATUS_INSTRUCTED {
		t.Fatal("partial marked settled")
	}
	older := confirmed(state, "settle-old", 20, false)
	older.SettledAt = state.PostingInstructedAt
	if err := s.Confirm(t.Context(), "tenant-A", older); err != nil {
		t.Fatal(err)
	}
	if err := s.Confirm(t.Context(), "tenant-A", confirmed(state, "decreasing-newer", 20, false)); !errors.Is(err, ErrConflict) {
		t.Fatalf("accepted contradictory newer cumulative report: %v", err)
	}
	if err := s.Confirm(t.Context(), "tenant-A", confirmed(state, "overfill", 61, false)); !errors.Is(err, ErrConflict) {
		t.Fatalf("overfill accepted: %v", err)
	}
	state = act(t, s, reload(t, s), "investigator", "dispute")
	full := confirmed(state, "settle-full", 60, false)
	if err := s.Confirm(t.Context(), "tenant-A", full); err != nil {
		t.Fatal(err)
	}
	state = reload(t, s)
	if state.Status != pb.WorkflowStatus_WORKFLOW_STATUS_DISPUTED || state.DisputedStatus != pb.WorkflowStatus_WORKFLOW_STATUS_SETTLED {
		t.Fatal("dispute lost custody evidence")
	}
	state = act(t, s, state, "reviewer", "resolve")
	state = act(t, s, state, "maker", "return")
	state = act(t, s, state, "checker", "approve_return")
	if err := s.Confirm(t.Context(), "tenant-A", confirmed(state, "return-partial", 30, true)); err != nil {
		t.Fatal(err)
	}
	var reservations int
	if err := s.pool.QueryRow(t.Context(), `SELECT count(*) FROM collateral_reservations`).Scan(&reservations); err != nil || reservations != 1 {
		t.Fatalf("premature release: %d %v", reservations, err)
	}
	returned := confirmed(state, "return-full", 60, true)
	if err := s.Confirm(t.Context(), "tenant-A", returned); err != nil {
		t.Fatal(err)
	}
	if err := New(s.pool).Confirm(t.Context(), "tenant-A", returned); err != nil {
		t.Fatalf("restart replay: %v", err)
	}
	state = reload(t, s)
	if state.Status != pb.WorkflowStatus_WORKFLOW_STATUS_RELEASED {
		t.Fatalf("not released: %+v", state)
	}
	if err := s.pool.QueryRow(t.Context(), `SELECT count(*) FROM collateral_reservations`).Scan(&reservations); err != nil || reservations != 0 {
		t.Fatalf("reservations: %d %v", reservations, err)
	}
	proof, err := New(s.pool).Proof(t.Context(), "tenant-A", state.WorkflowId)
	if err != nil || proof.Cost != "60" || !proof.Feasible {
		t.Fatalf("historical proof after release: %+v %v", proof, err)
	}
	for _, table := range []string{"collateral_snapshots", "collateral_requests", "collateral_confirmations", "collateral_allocation_proofs"} {
		if _, err := s.pool.Exec(t.Context(), `DELETE FROM `+table); err == nil {
			t.Fatalf("mutable evidence: %s", table)
		}
	}
}

func TestPostgresCollateralConcurrentReservationsAndRetries(t *testing.T) {
	s, _ := database(t)
	input := fixture()
	if err := s.ImportSnapshot(t.Context(), "tenant-A", input); err != nil {
		t.Fatal(err)
	}
	a := Action{Tenant: "tenant-A", Actor: "maker", RequestID: "request", WorkflowID: "workflow-1", SnapshotID: input.SnapshotId, Kind: "propose"}
	var wg sync.WaitGroup
	states := make([]*pb.WorkflowRecorded, 16)
	errs := make([]error, 16)
	for i := range states {
		wg.Add(1)
		go func(i int) { defer wg.Done(); states[i], errs[i] = New(s.pool).Apply(t.Context(), a) }(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil || !proto.Equal(states[0], states[i]) {
			t.Fatalf("retry %d: %v", i, err)
		}
	}
	input = proto.Clone(input).(*pb.WorkflowSnapshot)
	input.SnapshotId = "snapshot-2"
	input.Agreements[0].AgreementId = "CSA2"
	if err := s.ImportSnapshot(t.Context(), "tenant-A", input); err != nil {
		t.Fatal(err)
	}
	a.RequestID = "request2"
	a.WorkflowID = "workflow-2"
	a.SnapshotID = input.SnapshotId
	state, err := s.Apply(t.Context(), a)
	if err != nil || state.Status != pb.WorkflowStatus_WORKFLOW_STATUS_INFEASIBLE {
		t.Fatalf("double allocated: %+v %v", state, err)
	}
	proof, err := s.Proof(t.Context(), "tenant-A", state.WorkflowId)
	if err != nil || proof.Feasible || proof.Assets[0].Available != "40" {
		t.Fatalf("did not preserve residual capacity proof: %+v %v", proof, err)
	}
	var count int
	if err = s.pool.QueryRow(t.Context(), `SELECT count(*) FROM collateral_reservations`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("%d %v", count, err)
	}
	a.Actor = "forged"
	if _, err = s.Apply(t.Context(), a); !errors.Is(err, ErrConflict) {
		t.Fatalf("idempotency content changed: %v", err)
	}
}

func TestPostgresCollateralOutboxFailureRollsBack(t *testing.T) {
	s, _ := database(t)
	if err := s.ImportSnapshot(t.Context(), "tenant-A", fixture()); err != nil {
		t.Fatal(err)
	}
	_, err := s.pool.Exec(t.Context(), `CREATE OR REPLACE FUNCTION reject_collateral_outbox() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected failure'; END $$; CREATE TRIGGER reject_collateral_outbox BEFORE INSERT ON outbox FOR EACH ROW EXECUTE FUNCTION reject_collateral_outbox()`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Apply(t.Context(), Action{Tenant: "tenant-A", Actor: "maker", RequestID: "propose", WorkflowID: "workflow-1", SnapshotID: "snap-1", Kind: "propose"})
	if err == nil {
		t.Fatal("accepted without durable outbox")
	}
	for _, table := range []string{"collateral_workflows", "collateral_reservations", "collateral_requests", "collateral_active_agreements"} {
		var count int
		if err = s.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("partial %s: %d %v", table, count, err)
		}
	}
}

func TestPostgresCollateralRejectsSupersededAndChangedInventory(t *testing.T) {
	s, _ := database(t)
	state := propose(t, s)
	newer := fixture()
	newer.SnapshotId = "newer"
	newer.AsOf = timestamppb.New(time.Now().UTC())
	if err := s.ImportSnapshot(t.Context(), "tenant-A", newer); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(t.Context(), Action{Tenant: "tenant-A", Actor: "checker", RequestID: "stale", WorkflowID: state.WorkflowId, Kind: "approve", ExpectedRevision: 1}); !errors.Is(err, ErrStale) {
		t.Fatalf("approved superseded input: %v", err)
	}
	newer.SnapshotId = "changed-owner"
	newer.Inventory[0].AccountId = "another-account"
	if err := s.ImportSnapshot(t.Context(), "tenant-A", newer); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed lot identity: %v", err)
	}
}

func TestPostgresCollateralRejectsFractionalExecutableLot(t *testing.T) {
	s, _ := database(t)
	input := fixture()
	input.Inventory[0].UnitValue = number(3)
	input.Agreements[0].Exposure = number(4)
	if err := s.ImportSnapshot(t.Context(), "tenant-A", input); err != nil {
		t.Fatal(err)
	}
	_, err := s.Apply(t.Context(), Action{Tenant: "tenant-A", Actor: "maker", RequestID: "fractional", WorkflowID: "workflow-1", SnapshotID: input.SnapshotId, Kind: "propose"})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("silently rounded 4/3 lots: %v", err)
	}
	var count int
	if err = s.pool.QueryRow(t.Context(), `SELECT count(*) FROM collateral_reservations`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial reservation: %d %v", count, err)
	}
}

func TestCollateralModelUnknownWrongWayAndSettlementConstraints(t *testing.T) {
	for _, mutate := range []func(*pb.WorkflowSnapshot){
		func(s *pb.WorkflowSnapshot) { s.Agreements[0].Schedule[0].WrongWayRiskCleared = false },
		func(s *pb.WorkflowSnapshot) { s.Inventory[0].IssuerId = s.Agreements[0].CounterpartyId },
		func(s *pb.WorkflowSnapshot) {
			s.Inventory[0].AvailableAt = timestamppb.New(s.Agreements[0].SettlementDeadline.AsTime().Add(time.Second))
		},
		func(s *pb.WorkflowSnapshot) { s.Inventory[0].LiquidityBudget = number(59) },
		func(s *pb.WorkflowSnapshot) { s.Agreements[0].IssuerLimit = number(59) },
	} {
		s := fixture()
		mutate(s)
		legs, result, err := plan(t.Context(), s, nil)
		if err != nil || result.Feasible || len(legs) != 0 {
			t.Fatalf("unsafe model accepted: %+v %v", result, err)
		}
	}
	s := fixture()
	s.Agreements[0].Held = nil
	if validateSnapshot(s) == nil {
		t.Fatal("missing held substituted zero")
	}
}

func TestCollateralSharedLiquidityAndExistingIssuerHeadroom(t *testing.T) {
	s := fixture()
	s.Inventory[0].LiquidityBudget = number(75)
	s.Agreements[0].Exposure = number(20)
	_, result, err := plan(t.Context(), s, map[string]*big.Rat{"lot-A": big.NewRat(60, 1)})
	if err != nil || result.Feasible {
		t.Fatalf("spent reserved liquidity twice: %+v %v", result, err)
	}
	s = fixture()
	s.Agreements[0].Held = number(20)
	if _, _, err = plan(t.Context(), s, nil); !errors.Is(err, ErrInput) {
		t.Fatalf("unknown held issuer headroom: %v", err)
	}
	s.Agreements[0].IssuerHeadroom = []*pb.WorkflowIssuerHeadroom{{IssuerId: "issuer", Remaining: number(39)}}
	_, result, err = plan(t.Context(), s, nil)
	if err != nil || result.Feasible {
		t.Fatalf("ignored held issuer concentration: %+v %v", result, err)
	}
}

func TestPostgresCollateralDifferentCallsCompeteForOneLot(t *testing.T) {
	s, _ := database(t)
	input := fixture()
	if err := s.ImportSnapshot(t.Context(), "tenant-A", input); err != nil {
		t.Fatal(err)
	}
	second := proto.Clone(input).(*pb.WorkflowSnapshot)
	second.SnapshotId = "snap-2"
	second.Agreements[0].AgreementId = "CSA2"
	if err := s.ImportSnapshot(t.Context(), "tenant-A", second); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	states := make([]*pb.WorkflowRecorded, 2)
	errs := make([]error, 2)
	for i, snapshot := range []string{input.SnapshotId, second.SnapshotId} {
		wg.Add(1)
		go func(i int, snapshot string) {
			defer wg.Done()
			<-start
			states[i], errs[i] = New(s.pool).Apply(t.Context(), Action{Tenant: "tenant-A", Actor: "maker", RequestID: fmt.Sprintf("race-%d", i), WorkflowID: fmt.Sprintf("race-workflow-%d", i), SnapshotID: snapshot, Kind: "propose"})
		}(i, snapshot)
	}
	close(start)
	wg.Wait()
	proposed, infeasible := 0, 0
	for i, state := range states {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		switch state.Status {
		case pb.WorkflowStatus_WORKFLOW_STATUS_PROPOSED:
			proposed++
		case pb.WorkflowStatus_WORKFLOW_STATUS_INFEASIBLE:
			infeasible++
		default:
			t.Fatalf("unexpected state: %+v", state)
		}
	}
	if proposed != 1 || infeasible != 1 {
		t.Fatalf("double allocated: proposed=%d infeasible=%d", proposed, infeasible)
	}
}

func TestPostgresCollateralReturnOutboxFailureRestoresReservation(t *testing.T) {
	s, _ := database(t)
	state := act(t, s, propose(t, s), "checker", "approve")
	if err := s.Confirm(t.Context(), "tenant-A", confirmed(state, "settle", 60, false)); err != nil {
		t.Fatal(err)
	}
	state = act(t, s, reload(t, s), "maker", "return")
	state = act(t, s, state, "checker", "approve_return")
	_, err := s.pool.Exec(t.Context(), `CREATE OR REPLACE FUNCTION reject_collateral_outbox() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected failure'; END $$; CREATE TRIGGER reject_collateral_outbox BEFORE INSERT ON outbox FOR EACH ROW EXECUTE FUNCTION reject_collateral_outbox()`)
	if err != nil {
		t.Fatal(err)
	}
	confirmation := confirmed(state, "return-final", 60, true)
	if err = s.Confirm(t.Context(), "tenant-A", confirmation); err == nil {
		t.Fatal("released without durable fact")
	}
	if got := reload(t, s); !proto.Equal(got, state) {
		t.Fatalf("rollback changed state: %+v", got)
	}
	var count int
	if err = s.pool.QueryRow(t.Context(), `SELECT count(*) FROM collateral_reservations`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("lost reservation: %d %v", count, err)
	}
	if err = s.pool.QueryRow(t.Context(), `SELECT count(*) FROM collateral_confirmations WHERE source_id='return-final'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("retained rolled-back confirmation: %d %v", count, err)
	}
	if _, err = s.pool.Exec(t.Context(), `DROP TRIGGER reject_collateral_outbox ON outbox`); err != nil {
		t.Fatal(err)
	}
	if err = New(s.pool).Confirm(t.Context(), "tenant-A", confirmation); err != nil {
		t.Fatalf("release retry: %v", err)
	}
	if got := reload(t, s); got.Status != pb.WorkflowStatus_WORKFLOW_STATUS_RELEASED {
		t.Fatal("return failed after retry")
	}
}
