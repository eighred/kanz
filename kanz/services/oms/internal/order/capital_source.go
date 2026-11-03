package order

import (
	"context"
	"errors"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/oms/internal/capital"
	"google.golang.org/protobuf/proto"
)

// CapitalCashHandler projects accounting's complete revision sequence into the
// same tenant database used by order admission. Queue-group delivery is correct
// here because replicas share durable state; the separate in-memory compliance
// view must continue to receive broadcast snapshots.
func (p *Postgres) CapitalCashHandler(tenant string, history capital.HistoryReader) bus.EventHandler {
	return func(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
		if err := bus.RequireTenantScope(env.GetTenantId(), tenant); err != nil {
			return err
		}
		err := capital.ApplyEnvelope(ctx, p.pool, tenant, env, payload)
		if !errors.Is(err, capital.ErrUnknown) || history == nil {
			return err
		}
		var msg accountingpb.PortfolioCashBalance
		if err := proto.Unmarshal(payload, &msg); err != nil {
			return err
		}
		if err := capital.RecoverCashPrefix(ctx, p.pool, history, msg.GetPortfolioId(), msg.GetBaseCurrency(), msg.GetCashCommit().GetRevision()); err != nil {
			return err
		}
		// Re-apply the broker payload after recovery. Its receipt must agree with
		// retained history; reaching the same revision is not evidence of identity.
		return capital.ApplyEnvelope(ctx, p.pool, tenant, env, payload)
	}
}
