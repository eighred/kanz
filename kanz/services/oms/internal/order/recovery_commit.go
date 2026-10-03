package order

import (
	"context"
	"errors"
	"math/big"
	"sort"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/fillfact"
	"github.com/eighred/kanz/internal/outbox"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var ErrPostedExecutionUnknown = errors.New("oms: posted execution history is incomplete or contradicts the order aggregate")
var ErrRecoveryBlocked = errors.New("oms: execution recovery was durably blocked")

// CommitRecovery reconciles complete immutable execution evidence with the OMS
// journal. It never calls a venue operation. Claims, aggregate CAS, execution
// evidence, downstream targets and outbox facts share one transaction.
func (p *Postgres) CommitRecovery(ctx context.Context, c RecoveryCase, now time.Time) error {
	if c.Status != "investigating" || c.Checkpoint != 0 {
		return ErrConflict
	}
	history, digest, err := p.RecoveryHistory(ctx, c.ID)
	if err != nil {
		return err
	}
	st, version, err := p.Load(ctx, c.OrderID)
	if err != nil {
		return err
	}
	if history.GetOrderId() != st.GetOrderId() || history.GetMappingVersion() != c.MappingVersion {
		return ErrRecoveryEvidenceConflict
	}
	if _, err := execution.CompleteHistory(st, execution.OrderView{State: execution.OrderViewCancelled, ExecutedQuantity: history.ExecutedQuantity, Fills: history.Fills}); err != nil {
		return err
	}
	var mapped bool
	if err := p.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM execution_account_mappings WHERE version=$1 AND portfolio_id=$2 AND venue=$3 AND venue_account_id=$4)`, c.MappingVersion, st.GetPortfolioId(), st.GetVenue(), st.GetVenueAccountId()).Scan(&mapped); err != nil {
		return err
	}
	if !mapped {
		return ErrRecoveryEvidenceConflict
	}
	posted, err := p.postedRecoveryExecutions(ctx, st)
	if err != nil {
		_, latestVersion, loadErr := p.Load(ctx, c.OrderID)
		if loadErr == nil && latestVersion != version {
			return ErrConflict
		}
		return err
	}
	observed := make(map[string]*orderpb.Fill, len(history.Fills))
	for _, fill := range history.Fills {
		observed[fillfact.ExecutionKey(fill)] = fill
	}
	approvals, err := p.approvedFeeChanges(ctx, c, digest, posted)
	if err != nil {
		return err
	}
	for key, fill := range posted {
		if !fillfact.SameExecution(fill, observed[key]) {
			if approvals[key] == nil || !fillfact.SameExecutionExceptFee(fill, observed[key]) {
				return fillfact.ErrExecutionIdentityConflict
			}
		}
	}
	next := cloneState(st)
	var missing []*orderpb.Fill
	all := make([]*orderpb.Fill, 0, len(history.Fills))
	for _, original := range history.Fills {
		fill := proto.Clone(original).(*orderpb.Fill)
		executionDigest, err := fillfact.ExecutionDigest(fill)
		if err != nil {
			return err
		}
		fill.Recovery = &orderpb.ExecutionRecoveryProvenance{CaseId: c.ID, MappingVersion: c.MappingVersion,
			SourceCursor: c.SourceCursor, PayloadDigest: digest, ExecutionDigest: executionDigest, ObservedAt: history.ObservedAt}
		fill.Recovery.FeeApproval = approvals[fillfact.ExecutionKey(fill)]
		all = append(all, fill)
		if _, exists := posted[fillfact.ExecutionKey(fill)]; exists {
			continue
		}
		next, err = p.applyRecoveredExecution(ctx, c, next, fill, now)
		if err != nil {
			return err
		}
		missing = append(missing, fill)
	}
	if dec.Cmp(next.GetFilledQuantity(), history.GetExecutedQuantity()) != 0 {
		return ErrPostedExecutionUnknown
	}
	// Average price is a rounded projection, not evidence of an execution. A
	// sequence of rounded incremental averages need not equal the exact weighted
	// sum. Rebuild this derived field from the verified complete journal, rounding
	// once under the canonical Decimal policy; never compare rationals to a
	// previously rounded value to decide whether an execution really occurred.
	if dec.IsPositive(history.GetExecutedQuantity()) {
		cost := new(big.Rat)
		for _, fill := range history.Fills {
			cost.Add(cost, new(big.Rat).Mul(dec.FromProto(fill.Quantity), dec.FromProto(fill.Price)))
		}
		average, ok := dec.ToProtoScaled(new(big.Rat).Quo(cost, dec.FromProto(history.ExecutedQuantity)))
		if !ok {
			return ErrPostedExecutionUnknown
		}
		next.AverageFillPrice = average
	}
	var emitter Emitter
	records := make([]outbox.Record, 0, len(all)+1)
	byExecution := make(map[string]outbox.Record, len(all))
	for _, fill := range all {
		record, err := outbox.From(ctx, emitter.event(fillfact.SubjectRecovered, c.OrderID, now, &orderpb.ExecutionRecovered{Fill: fill, State: next}))
		if err != nil {
			return err
		}
		records = append(records, record)
		byExecution[fillfact.ExecutionKey(fill)] = record
	}
	// Global claim order prevents two overlapping recovery transactions from
	// taking their execution alias locks in opposite orders.
	sort.Slice(missing, func(i, j int) bool { return missing[i].GetFillId() < missing[j].GetFillId() })
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, fill := range missing {
		claimed, err := p.claimFill(ctx, tx, c.OrderID, fillfact.ExecutionKey(fill), fill.GetFillId())
		if err != nil {
			return err
		}
		if !claimed {
			if err := verifyDuplicateExecution(ctx, tx, fill); err != nil {
				return err
			}
			return ErrConflict
		}
		if err := journalExecution(ctx, tx, c.OrderID, fill.GetFillId(), []outbox.Record{byExecution[fillfact.ExecutionKey(fill)]}); err != nil {
			return err
		}
	}
	if err := p.observeFundedRecovery(ctx, tx, next, all, now); err != nil {
		return err
	}
	data, err := proto.Marshal(next)
	if err != nil {
		return err
	}
	if err := p.cas(ctx, tx, next, data, version); err != nil {
		return err
	}
	// The order CAS serializes fee heads with push fills and other recoveries.
	// The revision claim shares this transaction with the exact approved FACT.
	for _, fill := range all {
		if fill.GetRecovery().GetFeeApproval() != nil {
			if _, _, err := fillfact.RecordFeeRevision(ctx, tx, "oms", posted[fillfact.ExecutionKey(fill)], fill); err != nil {
				return err
			}
		}
	}
	c.Checkpoint = len(all)
	c.Reason = "OMS committed execution evidence; awaiting position and ledger acknowledgements"
	if len(all) == 0 {
		c.Status, c.Reason = "corrected", ""
	}
	tag, err := tx.Exec(ctx, `UPDATE execution_recovery_cases SET checkpoint=$1,expected_ack_count=$2,status=$3,reason=$4,version=version+1,updated_at=$5
		WHERE case_id=$6 AND version=$7 AND status='investigating' AND checkpoint=0`, c.Checkpoint, len(all)*2, c.Status, c.Reason, now.UTC(), c.ID, c.Version)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	for _, fill := range all {
		if _, err := tx.Exec(ctx, `INSERT INTO execution_recovery_targets (case_id,execution_key,payload_digest,execution_digest)
			VALUES ($1,$2,$3,$4)`, c.ID, fillfact.ExecutionKey(fill), digest, fill.GetRecovery().GetExecutionDigest()); err != nil {
			return err
		}
	}
	lifecycle, err := recoveryStatusRecord(ctx, c, next, now)
	if err != nil {
		return err
	}
	records = append(records, lifecycle)
	if err := outbox.Enqueue(ctx, tx, records...); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (p *Postgres) postedRecoveryExecutions(ctx context.Context, st *orderpb.OrderState) (map[string]*orderpb.Fill, error) {
	rows, err := p.pool.Query(ctx, `SELECT fill FROM order_executions WHERE order_id=$1 ORDER BY fill_id LIMIT 8001`, st.GetOrderId())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	posted := map[string]*orderpb.Fill{}
	quantity := new(big.Rat)
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var fill orderpb.Fill
		if err := proto.Unmarshal(data, &fill); err != nil {
			return nil, err
		}
		if _, ok := dec.InDomainDeep(&fill); !ok {
			return nil, ErrPostedExecutionUnknown
		}
		if fill.GetVenueAccountId() != st.GetVenueAccountId() || fill.GetVenue() != st.GetVenue() || fill.GetInstrumentId() != st.GetInstrumentId() || fill.GetOrderId() != st.GetOrderId() || fill.GetSide() != st.GetSide() || fill.GetVenueExecutionId() == "" || !dec.IsPositive(fill.GetQuantity()) {
			return nil, ErrPostedExecutionUnknown
		}
		key := fillfact.ExecutionKey(&fill)
		if _, duplicate := posted[key]; duplicate {
			return nil, ErrPostedExecutionUnknown
		}
		posted[key] = &fill
		quantity.Add(quantity, dec.FromProto(fill.GetQuantity()))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	var claims int
	if err := p.pool.QueryRow(ctx, `SELECT count(*) FROM order_fills WHERE order_id=$1`, st.GetOrderId()).Scan(&claims); err != nil {
		return nil, err
	}
	if claims != len(posted) || claims > 8000 || quantity.Cmp(dec.FromProto(st.GetFilledQuantity())) != 0 {
		return nil, ErrPostedExecutionUnknown
	}
	// One bounded indexed query overlays revision heads; recovery does not issue
	// a database round trip for every historical execution.
	heads, err := p.pool.Query(ctx, `SELECT DISTINCT ON(execution_key) execution_key,revised_fill FROM execution_fee_revisions WHERE book='oms' AND order_id=$1 ORDER BY execution_key,revision_sequence DESC LIMIT 8001`, st.OrderId)
	if err != nil {
		return nil, err
	}
	defer heads.Close()
	for heads.Next() {
		var key string
		var data []byte
		if err := heads.Scan(&key, &data); err != nil {
			return nil, err
		}
		var fill orderpb.Fill
		if err := proto.Unmarshal(data, &fill); err != nil {
			return nil, err
		}
		if fillfact.ExecutionKey(&fill) != key || !fillfact.SameExecutionExceptFee(posted[key], &fill) {
			return nil, fillfact.ErrFeeRevision
		}
		posted[key] = &fill
	}
	if err := heads.Err(); err != nil {
		return nil, err
	}
	return posted, nil
}

func (p *Postgres) applyRecoveredExecution(ctx context.Context, c RecoveryCase, st *orderpb.OrderState, fill *orderpb.Fill, now time.Time) (*orderpb.OrderState, error) {
	if fill.GetRecovery() == nil {
		return nil, ErrRecoveryEvidenceConflict
	}
	// ApplyFill's arithmetic remains the single fold. Only this verified history
	// path may correct terminal economics; the terminal disposition is retained.
	working := cloneState(st)
	terminal := IsTerminal(st)
	if terminal {
		working.Status = orderpb.OrderStatus_ORDER_STATUS_ROUTED
	}
	leaves, ok := dec.ToProtoScaled(new(big.Rat).Sub(dec.FromProto(st.GetOrderedQuantity()), dec.FromProto(st.GetFilledQuantity())))
	if !ok {
		return nil, ErrPostedExecutionUnknown
	}
	working.LeavesQuantity = leaves
	next, err := ApplyFill(working, fill, now)
	if err != nil {
		if blockErr := p.BlockRecovery(ctx, c, st, "verified execution cannot be represented in the order aggregate; operator investigation required", now); blockErr != nil {
			return nil, blockErr
		}
		return nil, ErrRecoveryBlocked
	}
	if terminal {
		next.Status = st.GetStatus()
	}
	next.AsOf = timestamppb.New(now.UTC())
	return next, nil
}
