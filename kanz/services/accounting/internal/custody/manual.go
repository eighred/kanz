package custody

import (
	"context"
	"errors"
	"strings"
	"unicode"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var ErrInvalidManualRequest = errors.New("custody: invalid dated reconciliation request")
var ErrManualRequestConflict = errors.New("custody: request identity is already bound to different inputs")
var ErrBookBasisChanged = errors.New("custody: the compared book is no longer current for this date")

// ReconcileManual receives its tenant and actor from the authenticated service
// boundary. Its date is a completed UTC business day. A fresh request for the
// same day may compare a new statement revision; a retry returns the actual
// previous result, even if the inputs or the operator's clock have advanced.
func (r *Reconciler) ReconcileManual(ctx context.Context, subject Subject, requestID, actor string) (Run, error) {
	id, err := uuid.Parse(requestID)
	if err != nil || id.String() != requestID || id.Version() != 4 || id.Variant() != uuid.RFC4122 ||
		actor == "" || len(actor) > 256 || strings.TrimSpace(actor) != actor || strings.IndexFunc(actor, unicode.IsControl) >= 0 || strings.TrimSpace(bus.TenantIDFromContext(ctx)) == "" ||
		subject.Validate() != nil || !subject.BusinessDate.Equal(BusinessDay(subject.BusinessDate)) ||
		!subject.BusinessDate.Before(BusinessDay(r.now())) {
		return Run{}, ErrInvalidManualRequest
	}
	request := Run{RunID: "manual:" + requestID, RequestID: requestID, Actor: actor, Subject: subject}
	prior, err := r.store.ManualReceipt(ctx, request)
	switch {
	case errors.Is(err, ErrNoRun):
		prior, err = r.reconcile(ctx, request, subject)
	case err != nil:
		return Run{}, err
	default:
		err = r.publish(ctx, prior)
	}
	if prior.RunID == "" || errors.Is(err, ErrPublicationPending) {
		return prior, err
	}
	if prior.Outcome == OutcomeFailed {
		switch prior.FailureReason {
		case ErrHistoricalLifecycle.Error():
			return prior, ErrHistoricalLifecycle
		case errBatchReceipt.Error():
			return prior, errBatchReceipt
		case bus.ErrPublishFrameTooLarge.Error():
			return prior, bus.ErrPublishFrameTooLarge
		}
		return prior, errors.New(prior.FailureReason)
	}
	return prior, nil
}

func (*MemoryStore) ManualReceipt(context.Context, Run) (Run, error) {
	return Run{}, ErrDurableActionStore
}

func (p *Postgres) ManualReceipt(ctx context.Context, request Run) (Run, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return Run{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var tenant string
	if err := tx.QueryRow(ctx, "SELECT app_current_tenant()").Scan(&tenant); err != nil {
		return Run{}, err
	}
	if bus.TenantIDFromContext(ctx) != tenant {
		return Run{}, ErrInvalidManualRequest
	}
	prior, err := storedManualReceipt(ctx, tx, request)
	if err != nil {
		return Run{}, err
	}
	return prior, tx.Commit(ctx)
}

func storedManualReceipt(ctx context.Context, tx pgx.Tx, request Run) (Run, error) {
	var payload []byte
	var basis *RunBasis
	err := tx.QueryRow(ctx, `SELECT manual_receipt,book_basis FROM custody_runs WHERE tenant_id=app_current_tenant() AND run_id=$1`, request.RunID).Scan(&payload, &basis)
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, ErrNoRun
	}
	if err != nil {
		return Run{}, err
	}
	if len(payload) == 0 {
		return Run{}, ErrRunCompletionMissing
	}
	prior, err := runFromRecord(payload)
	if err != nil {
		return Run{}, err
	}
	if prior.RunID != request.RunID || prior.RequestID != request.RequestID || prior.Actor != request.Actor || prior.Subject.Key() != request.Subject.Key() {
		return Run{}, ErrManualRequestConflict
	}
	prior.Basis = basis
	return prior, nil
}
