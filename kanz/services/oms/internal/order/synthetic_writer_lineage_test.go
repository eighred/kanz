package order

import (
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/outbox"
	"github.com/eighred/kanz/internal/pg"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

// The coordinator invokes this on a fresh fenced database. Use the same Go
// aggregate, fact builder and atomic store entry point as ordinary admission.
func TestSyntheticGoRetainedAdmission(t *testing.T) {
	if os.Getenv("KANZ_SYNTHETIC_GO_LINEAGE") != "1" {
		t.Skip("requires the disposable Go writer handoff fixture")
	}
	const tenant, orderID = "synthetic-go-handoff", "0f36dca681444918a676761d47b61533"
	ctx := bus.WithCausationID(bus.WithTenantID(t.Context(), tenant), "go-retained-command")
	raw := os.Getenv("SYNTHETIC_OMS_RUNTIME_URL")
	cfg, err := pgxpool.ParseConfig(raw)
	if err != nil || cfg.ConnConfig.Host != "127.0.0.1" || cfg.ConnConfig.Database != "kanz_synthetic_oms" || cfg.ConnConfig.User != "kanz_synthetic_oms_runtime" || len(cfg.ConnConfig.Fallbacks) != 0 {
		t.Fatal("expected a local disposable writer connection")
	}
	pool, err := pg.NewTenantPool(ctx, raw, tenant)
	if err != nil {
		t.Fatal("could not open scoped Go writer")
	}
	defer pool.Close()
	var restricted bool
	err = pool.QueryRow(ctx, "SELECT NOT rolsuper AND NOT rolbypassrls AND synthetic_oms_assert_writer()=1 FROM pg_roles WHERE rolname=session_user").Scan(&restricted)
	if err != nil || !restricted {
		t.Fatal("requires the active restricted first-generation writer")
	}
	wire, err := io.ReadAll(io.LimitReader(os.Stdin, 65537))
	var cmd orderpb.SubmitOrder
	if err != nil || len(wire) > 65536 || proto.Unmarshal(wire, &cmd) != nil || cmd.OrderId != orderID {
		t.Fatal("invalid fixture order")
	}
	st, err := Accept(&cmd, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	st.VenueAccountId = "synthetic-spot"
	st.AcceptedAnnouncedAt = st.AsOf
	fact, err := NewEmitter(nil).AcceptedFact(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	store := NewPostgres(pool)
	if err = store.Create(ctx, st, []outbox.Record{fact}); err != nil {
		t.Fatal(err)
	}
	loaded, version, err := store.Load(ctx, orderID)
	if err != nil || version != 0 || !proto.Equal(st, loaded) {
		t.Fatalf("Go retained admission version %d: %v", version, err)
	}
	if err = store.Create(ctx, st, []outbox.Record{fact}); !errors.Is(err, ErrExists) {
		t.Fatalf("Go duplicate admission: %v", err)
	}
}
