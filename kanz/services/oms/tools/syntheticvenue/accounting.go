package syntheticvenue

import (
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/oms/internal/order"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CapitalCashHandler consumes the accounting stream through the normal OMS
// projector. The isolated fixture supplies a tenant-scoped database pool and the
// original accounting frames from its private JetStream.
func CapitalCashHandler(pool *pgxpool.Pool, tenant string) bus.EventHandler {
	return order.NewPostgres(pool).CapitalCashHandler(tenant, nil)
}
