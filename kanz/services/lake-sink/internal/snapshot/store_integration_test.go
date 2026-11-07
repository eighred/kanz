package snapshot

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/pg"
	lakepb "github.com/eighred/kanz/kanz-schemas-go/lake/v1"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/lake-sink/internal/sink"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func snapshotStores(t *testing.T) (*Store, *Store) {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_URL is required for real snapshot PostgreSQL proof")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil || net.ParseIP(cfg.ConnConfig.Host) == nil || !net.ParseIP(cfg.ConnConfig.Host).IsLoopback() {
		t.Fatal("snapshot PostgreSQL fixture requires literal loopback")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal("fixture connection unavailable")
	}
	t.Cleanup(admin.Close)
	var unsafe bool
	if err := admin.QueryRow(ctx, `SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&unsafe); err != nil || unsafe {
		t.Fatal("fixture role must be NOSUPERUSER and NOBYPASSRLS")
	}
	schema := "lake_fixture_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal("fixture schema unavailable")
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := admin.Exec(cleanup, `DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`); err != nil {
			t.Error("owned fixture schema cleanup failed")
		}
	})
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	poolA, err := pg.NewTenantPool(ctx, cfg.ConnConfig.ConnString(), "tenant-a")
	if err != nil {
		t.Fatal("tenant A pool unavailable")
	}
	t.Cleanup(poolA.Close)
	ddl, err := os.ReadFile(filepath.Join("..", "..", "migrations", "0001_snapshots.sql"))
	if err != nil {
		t.Fatal("canonical migration unavailable")
	}
	if _, err := poolA.Exec(ctx, string(ddl)); err != nil {
		t.Fatal("canonical snapshot migration failed")
	}
	poolB, err := pg.NewTenantPool(ctx, cfg.ConnConfig.ConnString(), "tenant-b")
	if err != nil {
		t.Fatal("tenant B pool unavailable")
	}
	t.Cleanup(poolB.Close)
	var forced int
	if err := poolA.QueryRow(ctx, `SELECT count(*) FROM pg_class WHERE relnamespace=current_schema()::regnamespace AND relname IN ('lake_snapshots','outbox') AND relrowsecurity AND relforcerowsecurity`).Scan(&forced); err != nil || forced != 2 {
		t.Fatal("both owner tables must have actual FORCE RLS")
	}
	t.Log("actual snapshot PostgreSQL role: NOSUPERUSER NOBYPASSRLS; FORCE RLS tables: 2")
	authorizer := auth.NewPolicyAuthorizer(&auth.Policy{Roles: map[string][]auth.Action{"creator": {ActionCreate}, "reader": {ActionRead}}})
	a, err := NewStore(poolA, "tenant-a", authorizer)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewStore(poolB, "tenant-b", authorizer)
	if err != nil {
		t.Fatal(err)
	}
	return a, b
}

func snapshotInput(id string) *lakepb.CreateSnapshotInput {
	return &lakepb.CreateSnapshotInput{RequestId: id, Selection: &lakepb.SnapshotSelection{EventTypes: []string{"market.trade"}, EventTimeFrom: &timestamppb.Timestamp{Seconds: 1}, EventTimeUntil: &timestamppb.Timestamp{Seconds: 2}}}
}

func creator(tenant, subject string) *lakepb.GatewayCaller {
	return &lakepb.GatewayCaller{Tenant: tenant, Subject: subject, Roles: []string{"creator"}}
}

func TestSnapshotPostgresAuthorizationIdempotencyAndRLS(t *testing.T) {
	a, b := snapshotStores(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	input := snapshotInput("request-authorization")
	first, err := a.Create(ctx, creator("tenant-a", "alice"), input)
	if err != nil {
		t.Fatal("accepted resource failed")
	}
	if _, err := a.Get(ctx, creator("tenant-a", "alice"), first.SnapshotId); err == nil {
		t.Fatal("create granted read")
	}
	if _, err := a.Create(ctx, creator("tenant-a", "bob"), input); !errors.Is(err, ErrNotFound) {
		t.Fatal("second actor discovered existing request")
	}
	changed := proto.Clone(input).(*lakepb.CreateSnapshotInput)
	changed.Selection.EventTypes = []string{"market.quote"}
	if _, err := a.Create(ctx, creator("tenant-a", "alice"), changed); !errors.Is(err, ErrConflict) {
		t.Fatal("changed idempotency input accepted")
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := a.Create(ctx, creator("tenant-a", "alice"), input)
			if err == nil && !proto.Equal(result, first) {
				err = ErrConflict
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal("concurrent exact retry diverged")
		}
	}
	var rows, facts int
	if a.pool.QueryRow(ctx, `SELECT count(*) FROM lake_snapshots`).Scan(&rows) != nil || rows != 1 || a.pool.QueryRow(ctx, `SELECT count(*) FROM outbox`).Scan(&facts) != nil || facts != 1 {
		t.Fatal("duplicate resource or FACT committed")
	}
	if b.pool.QueryRow(ctx, `SELECT count(*) FROM lake_snapshots`).Scan(&rows) != nil || rows != 0 || b.pool.QueryRow(ctx, `SELECT count(*) FROM outbox`).Scan(&facts) != nil || facts != 0 {
		t.Fatal("cross-tenant rows visible")
	}
	foreignReader := &lakepb.GatewayCaller{Tenant: "tenant-b", Subject: "bob", Roles: []string{"reader"}}
	if _, err := a.Get(ctx, foreignReader, first.SnapshotId); !errors.Is(err, ErrNotFound) {
		t.Fatal("foreign metadata disclosed")
	}
	lease, err := a.Acquire(ctx, first.SnapshotId)
	if err != nil {
		t.Fatal("lease unavailable")
	}
	t.Cleanup(func() { _ = lease.Close() })
	if err := lease.Fail(ctx, lakepb.SnapshotFailure_SNAPSHOT_FAILURE_WORKER); err != nil {
		t.Fatal("failure FACT transaction failed")
	}
	if err := lease.Close(); err != nil {
		t.Fatal("lease close failed")
	}
	retry, err := a.Create(ctx, creator("tenant-a", "alice"), input)
	if err != nil || !proto.Equal(first, retry) {
		t.Fatal("Create retry exposed failed progress")
	}
}

func TestSnapshotPostgresAtomicOutboxRefusal(t *testing.T) {
	a, _ := snapshotStores(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := a.pool.Exec(ctx, `CREATE FUNCTION fixture_refuse_outbox() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'fixture refusal'; END; $$;
 CREATE TRIGGER fixture_refuse BEFORE INSERT ON outbox FOR EACH ROW EXECUTE FUNCTION fixture_refuse_outbox()`)
	if err != nil {
		t.Fatal("refusal fixture unavailable")
	}
	if _, err := a.Create(ctx, creator("tenant-a", "alice"), snapshotInput("request-rollback")); err == nil {
		t.Fatal("outbox refusal accepted")
	}
	var rows int
	if a.pool.QueryRow(ctx, `SELECT count(*) FROM lake_snapshots`).Scan(&rows) != nil || rows != 0 {
		t.Fatal("resource survived failed mandatory FACT")
	}
}

func TestSnapshotPostgresQuotasAndPersistedCut(t *testing.T) {
	a, _ := snapshotStores(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var ids []string
	for range 8 {
		response, err := a.Create(ctx, creator("tenant-a", "alice"), snapshotInput(uuid.NewString()))
		if err != nil {
			t.Fatal("initial quota admission failed")
		}
		ids = append(ids, response.SnapshotId)
	}
	if _, err := a.Create(ctx, creator("tenant-a", "alice"), snapshotInput(uuid.NewString())); !errors.Is(err, ErrCapacity) {
		t.Fatal("pending quota bypassed")
	}
	for _, id := range ids {
		lease, err := a.Acquire(ctx, id)
		if err != nil {
			t.Fatal("lease unavailable")
		}
		t.Cleanup(func() { _ = lease.Close() })
		if err := lease.PersistCut(ctx, sink.LandingCut{RootDevice: 1, RootInode: 2}); err != nil {
			t.Fatal("cut persist failed")
		}
		if err := lease.PersistCut(ctx, sink.LandingCut{RootDevice: 1, RootInode: 3}); !errors.Is(err, ErrConflict) {
			t.Fatal("cut recaptured")
		}
		if err := lease.Fail(ctx, lakepb.SnapshotFailure_SNAPSHOT_FAILURE_INPUT_UNAVAILABLE); err != nil {
			t.Fatal("failure transaction failed")
		}
		if err := lease.Close(); err != nil {
			t.Fatal("lease cleanup failed")
		}
	}
	for range 8 {
		response, err := a.Create(ctx, creator("tenant-a", "alice"), snapshotInput(uuid.NewString()))
		if err != nil {
			t.Fatal("retained quota admission failed")
		}
		lease, err := a.Acquire(ctx, response.SnapshotId)
		if err != nil {
			t.Fatal("lease unavailable")
		}
		t.Cleanup(func() { _ = lease.Close() })
		if err := lease.Fail(ctx, lakepb.SnapshotFailure_SNAPSHOT_FAILURE_INPUT_UNAVAILABLE); err != nil {
			t.Fatal("failure transaction failed")
		}
		if err := lease.Close(); err != nil {
			t.Fatal("lease cleanup failed")
		}
	}
	if _, err := a.Create(ctx, creator("tenant-a", "alice"), snapshotInput(uuid.NewString())); !errors.Is(err, ErrCapacity) {
		t.Fatal("FAILED resources released retained quota")
	}
}

func TestSnapshotPostgresGenerationExhaustionKeepsOriginalCutAndFailureFact(t *testing.T) {
	a, _ := snapshotStores(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	created, err := a.Create(ctx, creator("tenant-a", "alice"), snapshotInput("request-generations"))
	if err != nil {
		t.Fatal("resource creation failed")
	}
	cut := sink.LandingCut{RootDevice: 19, RootInode: 23}
	for generation := int64(1); generation <= 3; generation++ {
		lease, err := a.Acquire(ctx, created.SnapshotId)
		if err != nil {
			t.Fatal("bounded generation unavailable")
		}
		t.Cleanup(func() { _ = lease.Close() })
		if lease.row.generation != generation {
			t.Fatal("generation fence did not advance exactly once")
		}
		if generation == 1 {
			if lease.PersistCut(ctx, cut) != nil {
				t.Fatal("first cut did not persist")
			}
		} else {
			stored, err := lease.Cut()
			if err != nil || stored == nil || stored.RootDevice != cut.RootDevice || stored.RootInode != cut.RootInode {
				t.Fatal("restart recaptured the source cut")
			}
		}
		// A lost worker leaves this generation unpublished. Releasing its actual
		// session permits a new owner, without deleting or accepting its files.
		if lease.Close() != nil {
			t.Fatal("prior session did not close")
		}
	}
	if lease, err := a.Acquire(ctx, created.SnapshotId); !errors.Is(err, ErrCapacity) || lease != nil {
		t.Fatal("fourth publication generation admitted")
	}
	var state, generation, failure, facts int
	if a.pool.QueryRow(ctx, `SELECT state,generation,failure FROM lake_snapshots WHERE snapshot_id=$1`, created.SnapshotId).Scan(&state, &generation, &failure) != nil || state != 4 || generation != 3 || failure != int(lakepb.SnapshotFailure_SNAPSHOT_FAILURE_CAPACITY) {
		t.Fatal("generation exhaustion did not persist closed failure")
	}
	if a.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE event_type=$1`, "lake.snapshot.failed").Scan(&facts) != nil || facts != 1 {
		t.Fatal("failure FACT not atomically retained")
	}
	retry, err := a.Create(ctx, creator("tenant-a", "alice"), snapshotInput("request-generations"))
	if err != nil || !proto.Equal(created, retry) {
		t.Fatal("creation retry disclosed progress or replaced accepted resource")
	}
}
