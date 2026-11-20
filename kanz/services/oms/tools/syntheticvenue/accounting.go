package syntheticvenue

import (
	"bytes"
	"context"
	"errors"
	"net/http"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/oms/internal/capital"
	"github.com/eighred/kanz/services/oms/internal/order"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CapitalCashHandler consumes the accounting stream through the normal OMS
// projector. The isolated fixture supplies a tenant-scoped database pool and the
// original accounting frames from its private JetStream.
func CapitalCashHandler(pool *pgxpool.Pool, tenant string) bus.EventHandler {
	return order.NewPostgres(pool).CapitalCashHandler(tenant, nil)
}

// RecoverOpeningCash exercises the standard OMS history reader and prefix recovery
// against an empty disposable projection. The old envelope stays in JetStream.
func RecoverOpeningCash(ctx context.Context, pool *pgxpool.Pool, endpoint string, client *http.Client, bearer, signingKey, portfolio string, original []byte) error {
	history, err := capital.NewHistoryClient(endpoint, client, func(context.Context) (capital.HistoryCredentials, error) {
		return capital.HistoryCredentials{Bearer: bearer, SigningKey: signingKey}, nil
	})
	if err != nil {
		return err
	}
	defer history.CloseIdleConnections()
	page, err := history.ReadCashCommits(ctx, portfolio, "USD", 0, 1, 1)
	if err != nil {
		return err
	}
	if len(page.Records) != 1 || !bytes.Equal(page.Records[0].Payload, original) {
		return errors.New("retained opening cash differs from accounting publication")
	}
	return capital.RecoverCashPrefix(ctx, pool, history, portfolio, "USD", 1)
}
