//go:build synthetic_handoff

// Command syntheticadmission is the one-shot Go writer for the isolated
// forwarding drill. Its sole output is a bounded admission result.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/eighred/kanz/internal/pg"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/oms/internal/order"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

func main() {
	wire, err := io.ReadAll(io.LimitReader(os.Stdin, 131073))
	var frame envelopepb.EventFrame
	if err != nil || len(wire) > 131072 || proto.Unmarshal(wire, &frame) != nil || bus.Validate(frame.GetEnvelope()) != nil {
		fmt.Println("REFUSED")
		return
	}
	dsn := os.Getenv("SYNTHETIC_OMS_DATABASE_URL")
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil || cfg.ConnConfig.Host != "127.0.0.1" || cfg.ConnConfig.User != "kanz_synthetic_oms_runtime" ||
		cfg.ConnConfig.Database != "kanz_synthetic_oms" || len(cfg.ConnConfig.Fallbacks) != 0 {
		fmt.Println("REFUSED")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	tenant := frame.Envelope.TenantId
	pool, err := pg.NewTenantPool(ctx, dsn, tenant)
	if err != nil {
		fmt.Println("REFUSED")
		return
	}
	defer pool.Close()
	admit := order.NewPostgres(pool).CreateSyntheticHandoff
	if len(os.Args) != 1 {
		if len(os.Args) != 2 || os.Args[1] != "--funded" {
			fmt.Println("REFUSED")
			return
		}
		admit = order.NewPostgres(pool).CreateFundedSyntheticHandoff
	}
	switch err := admit(bus.WithTenantID(ctx, tenant), wire); {
	case err == nil:
		fmt.Println("ACCEPTED")
	case errors.Is(err, order.ErrExists):
		fmt.Println("DUPLICATE")
	default:
		fmt.Println("REFUSED")
	}
}
