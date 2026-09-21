package order

import (
	"context"
	"errors"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

func (p *Postgres) ReadFeeCorrection(ctx context.Context, portfolio, caseID string) (*orderpb.GetExecutionFeeCorrectionResponse, error) {
	var status, reason string
	var approval []byte
	err := p.pool.QueryRow(ctx, `SELECT c.status,c.reason,a.approval FROM execution_fee_proposals p JOIN execution_recovery_cases c USING(tenant_id,case_id) JOIN orders o ON o.tenant_id=p.tenant_id AND o.order_id=p.order_id LEFT JOIN execution_fee_approvals a ON a.tenant_id=p.tenant_id AND a.case_id=p.case_id WHERE p.case_id=$1 AND o.portfolio_id=$2`, caseID, portfolio).Scan(&status, &reason, &approval)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	proposal, err := p.FeeProposal(ctx, caseID)
	if err != nil {
		return nil, err
	}
	if proposal.PortfolioId != portfolio {
		return nil, ErrNotFound
	}
	result := &orderpb.GetExecutionFeeCorrectionResponse{Proposal: proposal, Status: status, Reason: reason, OwnerTenant: bus.TenantIDFromContext(ctx)}
	if len(approval) != 0 {
		result.Approval = &orderpb.ExecutionFeeCorrectionApproved{}
		if err := proto.Unmarshal(approval, result.Approval); err != nil {
			return nil, err
		}
	}
	return result, nil
}
