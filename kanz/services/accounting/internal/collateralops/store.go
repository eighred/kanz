package collateralops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"time"

	"github.com/eighred/kanz/internal/outbox"
	pb "github.com/eighred/kanz/kanz-schemas-go/collateral/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const SubjectRecorded = "accounting.collateral.recorded"

type Store struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }
func digest(b []byte) string        { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func marshal(m proto.Message) ([]byte, error) {
	return proto.MarshalOptions{Deterministic: true}.Marshal(m)
}

// A tenant-scoped advisory transaction lock serializes this low-frequency
// control workflow across replicas. It is never held by the execution path.
// Solve deadlines and bounded LP dimensions bound lock occupancy.
func (s *Store) begin(ctx context.Context, tenant string) (pgx.Tx, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if err = lock(ctx, tx, tenant); err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	return tx, nil
}

func lock(ctx context.Context, tx pgx.Tx, tenant string) error {
	var actual string
	if err := tx.QueryRow(ctx, `SELECT app_current_tenant()`).Scan(&actual); err != nil {
		return err
	}
	if actual != tenant {
		return ErrNotFound
	}
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(app_current_tenant()||'/collateral-control',0))`)
	return err
}

// ImportSnapshot is only invoked by the authenticated source FACT consumer.
// No operator endpoint may invent authoritative balances or source provenance.
func (s *Store) ImportSnapshot(ctx context.Context, tenant string, input *pb.WorkflowSnapshot) error {
	if err := validateSnapshot(input); err != nil {
		return err
	}
	blob, err := marshal(input)
	if err != nil {
		return err
	}
	tx, err := s.begin(ctx, tenant)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var prior string
	err = tx.QueryRow(ctx, `SELECT digest FROM collateral_snapshots WHERE snapshot_id=$1`, input.SnapshotId).Scan(&prior)
	if err == nil {
		if prior != digest(blob) {
			return ErrConflict
		}
		return tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	for _, lot := range input.Inventory {
		identity, _ := json.Marshal([]string{lot.AssetId, lot.IssuerId, lot.CustodianId, lot.AccountId, input.CurrencyCode})
		lotBlob, err := marshal(lot)
		if err != nil {
			return err
		}
		var oldIdentity string
		var observed time.Time
		var oldBlob []byte
		err = tx.QueryRow(ctx, `SELECT identity_digest,observed_at,payload FROM collateral_lots WHERE lot_id=$1`, lot.LotId).Scan(&oldIdentity, &observed, &oldBlob)
		if err == nil {
			if oldIdentity != digest(identity) {
				return ErrConflict
			}
			if observed.Equal(input.AsOf.AsTime().Truncate(time.Microsecond)) && digest(oldBlob) != digest(lotBlob) {
				return ErrConflict
			}
			if observed.Before(input.AsOf.AsTime().Truncate(time.Microsecond)) {
				if _, err = tx.Exec(ctx, `UPDATE collateral_lots SET observed_at=$2,payload=$3 WHERE lot_id=$1`, lot.LotId, input.AsOf.AsTime().Truncate(time.Microsecond), lotBlob); err != nil {
					return err
				}
			}
		} else if errors.Is(err, pgx.ErrNoRows) {
			if _, err = tx.Exec(ctx, `INSERT INTO collateral_lots(lot_id,identity_digest,observed_at,payload) VALUES($1,$2,$3,$4)`, lot.LotId, digest(identity), input.AsOf.AsTime().Truncate(time.Microsecond), lotBlob); err != nil {
				return err
			}
		} else {
			return err
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO collateral_snapshots(snapshot_id,portfolio_id,as_of,digest,payload) VALUES($1,$2,$3,$4,$5)`, input.SnapshotId, input.PortfolioId, input.AsOf.AsTime().Truncate(time.Microsecond), digest(blob), blob)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type Action struct {
	Tenant, Actor, RequestID, WorkflowID, SnapshotID, Kind, Explanation string
	ExpectedRevision                                                    int64
}

func (s *Store) Load(ctx context.Context, tenant, id string) (*pb.WorkflowRecorded, error) {
	tx, err := s.begin(ctx, tenant)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	result, err := loadWorkflow(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	return result, tx.Commit(ctx)
}
func loadWorkflow(ctx context.Context, tx pgx.Tx, id string) (*pb.WorkflowRecorded, error) {
	var blob []byte
	err := tx.QueryRow(ctx, `SELECT payload FROM collateral_workflows WHERE workflow_id=$1`, id).Scan(&blob)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	state := new(pb.WorkflowRecorded)
	if err = proto.Unmarshal(blob, state); err != nil {
		return nil, err
	}
	return state, nil
}
func loadSnapshot(ctx context.Context, tx pgx.Tx, id string) (*pb.WorkflowSnapshot, error) {
	var blob []byte
	err := tx.QueryRow(ctx, `SELECT payload FROM collateral_snapshots WHERE snapshot_id=$1`, id).Scan(&blob)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	snapshot := new(pb.WorkflowSnapshot)
	if err = proto.Unmarshal(blob, snapshot); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (s *Store) Apply(ctx context.Context, a Action) (*pb.WorkflowRecorded, error) {
	if !identifier(a.Tenant) || !identifier(a.Actor) || !identifier(a.RequestID) || !identifier(a.WorkflowID) || len(a.Explanation) > 4096 || a.ExpectedRevision < 0 || a.ExpectedRevision >= 1<<62 {
		return nil, ErrInput
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	request, _ := json.Marshal(a)
	if err = lock(ctx, tx, a.Tenant); err != nil {
		return nil, err
	}
	hash := digest(request)
	var prior string
	var blob []byte
	err = tx.QueryRow(ctx, `SELECT digest,evidence FROM collateral_requests WHERE request_id=$1`, a.RequestID).Scan(&prior, &blob)
	if err == nil {
		if prior != hash {
			return nil, ErrConflict
		}
		result := new(pb.WorkflowRecorded)
		if err = proto.Unmarshal(blob, result); err != nil {
			return nil, err
		}
		return result, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	var state *pb.WorkflowRecorded
	if a.Kind == "propose" {
		if a.ExpectedRevision != 0 || !identifier(a.SnapshotID) {
			return nil, ErrInput
		}
		input, err := loadSnapshot(ctx, tx, a.SnapshotID)
		if err != nil {
			return nil, err
		}
		if now.Before(input.AsOf.AsTime().Truncate(time.Microsecond)) || !now.Before(input.ValidUntil.AsTime()) {
			return nil, ErrStale
		}
		if err = checkCurrent(ctx, tx, input); err != nil {
			return nil, err
		}
		reserved := map[string]*big.Rat{}
		lotIDs := make([]string, 0, len(input.Inventory))
		for _, lot := range input.Inventory {
			lotIDs = append(lotIDs, lot.LotId)
		}
		rows, err := tx.Query(ctx, `SELECT lot_id,quantity FROM collateral_reservations WHERE lot_id=ANY($1)`, lotIDs)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id, value string
			if err = rows.Scan(&id, &value); err != nil {
				break
			}
			v, ok := new(big.Rat).SetString(value)
			if !ok || v.Sign() < 0 {
				err = ErrInput
				break
			}
			if reserved[id] == nil {
				reserved[id] = new(big.Rat)
			}
			reserved[id].Add(reserved[id], v)
		}
		rows.Close()
		if err != nil {
			return nil, err
		}
		if err = rows.Err(); err != nil {
			return nil, err
		}
		legs, result, err := plan(ctx, input, reserved)
		if err != nil {
			return nil, err
		}
		status := pb.WorkflowStatus_WORKFLOW_STATUS_PROPOSED
		if !result.Feasible {
			status = pb.WorkflowStatus_WORKFLOW_STATUS_INFEASIBLE
		} else if len(legs) == 0 {
			status = pb.WorkflowStatus_WORKFLOW_STATUS_NO_MOVEMENT
		}
		state = &pb.WorkflowRecorded{WorkflowId: a.WorkflowID, SnapshotId: a.SnapshotID, Revision: 1, Status: status, Maker: a.Actor, CurrencyCode: input.CurrencyCode, Legs: legs}
		state.AllocationModelVersion = "exact-lp-csa-v1"
		// Certificate is immutable request evidence, alongside the emitted state.
		if !result.Feasible {
			certificate, _ := json.Marshal(result.Certificate)
			state.Explanation = "insufficient eligible collateral; certificate=" + string(certificate)
		}
		blob, err = marshal(state)
		if err != nil {
			return nil, err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO collateral_workflows(workflow_id,snapshot_id,revision,payload) VALUES($1,$2,1,$3)`, a.WorkflowID, a.SnapshotID, blob); err != nil {
			return nil, err
		}
		for _, leg := range legs {
			_, err = tx.Exec(ctx, `INSERT INTO collateral_reservations(workflow_id,agreement_id,lot_id,quantity) VALUES($1,$2,$3,$4)`, a.WorkflowID, leg.AgreementId, leg.LotId, rat(leg.Quantity).RatString())
			if err != nil {
				return nil, err
			}
		}
		if result.Feasible && len(legs) > 0 {
			for _, agreement := range input.Agreements {
				if _, err = tx.Exec(ctx, `INSERT INTO collateral_active_agreements(agreement_id,workflow_id) VALUES($1,$2)`, agreement.AgreementId, a.WorkflowID); err != nil {
					return nil, ErrConflict
				}
			}
		}
	} else {
		state, err = loadWorkflow(ctx, tx, a.WorkflowID)
		if err != nil {
			return nil, err
		}
		if state.Revision != a.ExpectedRevision || a.SnapshotID != "" {
			return nil, ErrConflict
		}
		switch a.Kind {
		case "cancel":
			// A never-instructed proposal can be withdrawn without asserting any
			// custody movement. Instructed/settled workflows cannot take this path.
			if state.Status != pb.WorkflowStatus_WORKFLOW_STATUS_PROPOSED || a.Explanation == "" {
				return nil, ErrTransition
			}
			state.Status = pb.WorkflowStatus_WORKFLOW_STATUS_CANCELLED
			if _, err = tx.Exec(ctx, `DELETE FROM collateral_reservations WHERE workflow_id=$1`, state.WorkflowId); err != nil {
				return nil, err
			}
			if _, err = tx.Exec(ctx, `DELETE FROM collateral_active_agreements WHERE workflow_id=$1`, state.WorkflowId); err != nil {
				return nil, err
			}
		case "approve":
			if state.Status != pb.WorkflowStatus_WORKFLOW_STATUS_PROPOSED || a.Actor == state.Maker {
				return nil, ErrTransition
			}
			input, err := loadSnapshot(ctx, tx, state.SnapshotId)
			if err != nil {
				return nil, err
			}
			if !now.Before(input.ValidUntil.AsTime()) {
				return nil, ErrStale
			}
			if err = checkCurrent(ctx, tx, input); err != nil {
				return nil, err
			}
			state.Status = pb.WorkflowStatus_WORKFLOW_STATUS_INSTRUCTED
			state.PostingRevision = state.Revision + 1
			state.PostingInstructedAt = timestamppb.New(now)
		case "dispute":
			if (state.Status != pb.WorkflowStatus_WORKFLOW_STATUS_INSTRUCTED && state.Status != pb.WorkflowStatus_WORKFLOW_STATUS_SETTLED) || a.Explanation == "" {
				return nil, ErrTransition
			}
			state.DisputedStatus = state.Status
			state.DisputeActor = a.Actor
			state.Status = pb.WorkflowStatus_WORKFLOW_STATUS_DISPUTED
		case "resolve":
			if state.Status != pb.WorkflowStatus_WORKFLOW_STATUS_DISPUTED || state.DisputeActor == a.Actor || a.Explanation == "" {
				return nil, ErrTransition
			}
			state.Status = state.DisputedStatus
			state.DisputedStatus = pb.WorkflowStatus_WORKFLOW_STATUS_UNSPECIFIED
		case "return":
			if state.Status != pb.WorkflowStatus_WORKFLOW_STATUS_SETTLED || a.Explanation == "" {
				return nil, ErrTransition
			}
			state.ReturnMaker = a.Actor
			state.Status = pb.WorkflowStatus_WORKFLOW_STATUS_RETURN_PROPOSED
		case "approve_return":
			if state.Status != pb.WorkflowStatus_WORKFLOW_STATUS_RETURN_PROPOSED || state.ReturnMaker == a.Actor {
				return nil, ErrTransition
			}
			state.Status = pb.WorkflowStatus_WORKFLOW_STATUS_RETURN_INSTRUCTED
			state.ReturnRevision = state.Revision + 1
			state.ReturnInstructedAt = timestamppb.New(now)
		default:
			return nil, ErrTransition
		}
		state.Revision++
		state.Explanation = a.Explanation
	}
	state.Actor = a.Actor
	state.RequestId = a.RequestID
	state.RecordedAt = timestamppb.New(now)
	fact, err := record(ctx, tx, a.Tenant, state)
	if err != nil {
		return nil, err
	}
	if err = outbox.Enqueue(ctx, tx, fact); err != nil {
		return nil, err
	}
	blob, err = marshal(state)
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO collateral_requests(request_id,digest,evidence) VALUES($1,$2,$3)`, a.RequestID, hash, blob); err != nil {
		return nil, err
	}
	return state, tx.Commit(ctx)
}

func checkCurrent(ctx context.Context, tx pgx.Tx, input *pb.WorkflowSnapshot) error {
	for _, agreement := range input.Agreements {
		if time.Now().UTC().After(agreement.SettlementDeadline.AsTime()) {
			return ErrStale
		}
	}
	var latest time.Time
	if err := tx.QueryRow(ctx, `SELECT max(as_of) FROM collateral_snapshots WHERE portfolio_id=$1`, input.PortfolioId).Scan(&latest); err != nil {
		return err
	}
	if latest.After(input.AsOf.AsTime().Truncate(time.Microsecond)) {
		return ErrStale
	}
	for _, lot := range input.Inventory {
		if err := tx.QueryRow(ctx, `SELECT observed_at FROM collateral_lots WHERE lot_id=$1`, lot.LotId).Scan(&latest); err != nil {
			return err
		}
		if latest.After(input.AsOf.AsTime().Truncate(time.Microsecond)) {
			return ErrStale
		}
	}
	return nil
}

func record(ctx context.Context, tx pgx.Tx, tenant string, state *pb.WorkflowRecorded) (outbox.Record, error) {
	blob, err := marshal(state)
	if err != nil {
		return outbox.Record{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE collateral_workflows SET revision=$2,payload=$3 WHERE workflow_id=$1`, state.WorkflowId, state.Revision, blob); err != nil {
		return outbox.Record{}, err
	}
	fact, err := outbox.From(ctx, bus.Event{Subject: SubjectRecorded, EventType: SubjectRecorded, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, Domain: "accounting", TenantID: tenant, EventTime: state.RecordedAt.AsTime(), CorrelationID: state.RequestId, PartitionKey: state.WorkflowId, PayloadSchemaRef: "collateral.v1.WorkflowRecorded:1", Payload: state})
	return fact, err
}
