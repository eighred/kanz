package pg

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/migrate"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// This deliberately uses a separate disposable server setup identity. Ordinary
// TEST_POSTGRES_URL owns test schemas and cannot prove runtime/owner separation.
// The qualification lane supplies only a dummy, isolated PostgreSQL endpoint.
type runtimeRoleFixture struct {
	app, owner           *pgxpool.Pool
	appURL, migrationURL string
	appRole, ownerRole   string
	admin                *pgx.Conn
}

func hardenRuntimeBookkeeping(t *testing.T, ctx context.Context, owner *pgxpool.Pool, runtimeRole string) {
	applyStagedRuntimeSQL(t, ctx, owner, runtimeRole, "harden-migration-bookkeeping.sql")
}

type runtimeSQLBeginner interface {
	Begin(context.Context) (pgx.Tx, error)
}

func applyStagedRuntimeSQL(t *testing.T, ctx context.Context, authority runtimeSQLBeginner, runtimeRole, filename string) {
	t.Helper()
	source, err := os.ReadFile(filepath.Join("..", "..", "..", "tools", "runtime_credentials", filename))
	if err != nil {
		t.Fatal("read staged owner-side hardening SQL")
	}
	if strings.Count(string(source), "DO $$") != 1 || strings.Count(string(source), "END $$;") != 1 {
		t.Fatal("staged hardening SQL no longer has one explicit owner transaction body")
	}
	_, body, _ := strings.Cut(string(source), "DO $$")
	body, _, _ = strings.Cut(body, "END $$;")
	tx, err := authority.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SET LOCAL search_path=pg_catalog"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('kanz.hardening_runtime_role',$1,true)", runtimeRole); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "DO $$"+body+"END $$;"); err != nil {
		t.Fatal("staged owner hardening refused:", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func newRuntimeRoleFixture(t *testing.T, ctx context.Context, service string) *runtimeRoleFixture {
	t.Helper()
	adminURL := os.Getenv("TEST_RUNTIME_ROLE_ADMIN_URL")
	if adminURL == "" {
		t.Skip("isolated runtime-role PostgreSQL fixture is not configured")
	}
	adminConfig, err := pgx.ParseConfig(adminURL)
	if err != nil || adminConfig.Host != "127.0.0.1" || adminConfig.User != "fixture_admin" || adminConfig.Database != "fixture" {
		t.Fatal("runtime-role proof accepts only its owned loopback fixture identity")
	}
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatal("connect disposable setup identity:", err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	name := "runtime_guard_" + hex.EncodeToString(random[:5])
	appRole, migrationRole := name+"_app", name+"_migrate"
	appPassword, migrationPassword := hex.EncodeToString(random[:16]), hex.EncodeToString(random[16:])
	quote := func(name string) string { return pgx.Identifier{name}.Sanitize() }
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		for _, statement := range []string{"DROP DATABASE IF EXISTS " + quote(name) + " WITH (FORCE)", "DROP ROLE IF EXISTS " + quote(appRole), "DROP ROLE IF EXISTS " + quote(migrationRole)} {
			if _, err := admin.Exec(cleanup, statement); err != nil {
				t.Error("disposable role/database cleanup:", err)
			}
		}
	})
	for role, password := range map[string]string{appRole: appPassword, migrationRole: migrationPassword} {
		if _, err := admin.Exec(ctx, "CREATE ROLE "+quote(role)+" LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE PASSWORD '"+password+"'"); err != nil {
			t.Fatal("create disposable role:", err)
		}
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+quote(name)+" OWNER "+quote(migrationRole)); err != nil {
		t.Fatal("create disposable database:", err)
	}
	connect := func(role, password string) (*pgxpool.Pool, string) {
		t.Helper()
		config, err := pgxpool.ParseConfig(adminURL)
		if err != nil {
			t.Fatal("parse disposable fixture configuration:", err)
		}
		config.ConnConfig.Database, config.ConnConfig.User, config.ConnConfig.Password = name, role, password
		config.MaxConns, config.MinConns = 2, 0
		pool, err := pgxpool.NewWithConfig(ctx, config)
		if err != nil {
			t.Fatal("open disposable role:", err)
		}
		t.Cleanup(pool.Close)
		var current, session string
		var superuser, bypass bool
		if err := pool.QueryRow(ctx, "SELECT current_user, session_user, rolsuper, rolbypassrls FROM pg_roles WHERE rolname=current_user").Scan(&current, &session, &superuser, &bypass); err != nil || current != role || session != role || superuser || bypass {
			t.Fatal("fixture did not connect as the actual NOSUPERUSER/NOBYPASSRLS identity:", err)
		}
		roleURL := &url.URL{Scheme: "postgresql", User: url.UserPassword(role, password), Host: net.JoinHostPort(config.ConnConfig.Host, strconv.Itoa(int(config.ConnConfig.Port))), Path: "/" + name, RawQuery: "sslmode=disable"}
		return pool, roleURL.String()
	}
	owner, migrationURL := connect(migrationRole, migrationPassword)
	app, appURL := connect(appRole, appPassword)
	for _, statement := range []string{
		"GRANT CONNECT ON DATABASE " + quote(name) + " TO " + quote(appRole),
		"GRANT USAGE ON SCHEMA public TO " + quote(appRole),
		"ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO " + quote(appRole),
	} {
		if _, err := owner.Exec(ctx, statement); err != nil {
			t.Fatal("prepare runtime grants:", err)
		}
	}
	migrations, err := migrate.Load(filepath.Join("..", "..", "services", service, "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.New(owner).Up(ctx, migrations); err != nil {
		t.Fatal("authorized migration failed:", err)
	}
	// The canonical broad default-DML grant also covers migration bookkeeping.
	// The staged owner-side prerequisite removes those writes, preserving reads.
	hardenRuntimeBookkeeping(t, ctx, owner, appRole)
	return &runtimeRoleFixture{app: app, owner: owner, appURL: appURL, migrationURL: migrationURL, appRole: appRole, ownerRole: migrationRole, admin: admin}
}

func TestRuntimePrivilegesWithSeparateRoles(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	fixture := newRuntimeRoleFixture(t, ctx, "audit")
	app, owner, admin := fixture.app, fixture.owner, fixture.admin
	appRole, migrationRole := fixture.appRole, fixture.ownerRole
	quote := func(name string) string { return pgx.Identifier{name}.Sanitize() }
	if err := CheckRuntimePrivileges(ctx, app, "public"); err != nil {
		t.Fatal("restricted runtime refused:", err)
	}
	if err := CheckRuntimePrivileges(ctx, owner, "public"); err == nil {
		t.Fatal("migration owner was accepted as runtime")
	}
	if err := CheckRuntimePrivileges(ctx, app, "missing_schema"); err == nil {
		t.Fatal("missing schema was presented as verified runtime authority")
	}
	t.Run("writable-migration-bookkeeping", func(t *testing.T) {
		for _, permission := range []string{"INSERT, UPDATE, DELETE", "UPDATE (checksum)"} {
			t.Run(map[bool]string{true: "column", false: "table"}[permission == "UPDATE (checksum)"], func(t *testing.T) {
				if _, err := owner.Exec(ctx, "GRANT "+permission+" ON schema_migrations TO "+quote(appRole)); err != nil {
					t.Fatal(err)
				}
				if err := CheckRuntimePrivileges(ctx, app, "public"); err == nil {
					t.Fatal("writable migration bookkeeping was certified")
				}
				if _, err := app.Exec(ctx, "UPDATE schema_migrations SET checksum=checksum"); err != nil {
					t.Fatal("affected writable-bookkeeping control failed")
				}
				hardenRuntimeBookkeeping(t, ctx, owner, appRole)
				if err := CheckRuntimePrivileges(ctx, app, "public"); err != nil {
					t.Fatal("owner-restricted migration bookkeeping refused")
				}
			})
		}
	})
	t.Run("assumable-truncate", func(t *testing.T) {
		writer := appRole + "_writer"
		if _, err := admin.Exec(ctx, "CREATE ROLE "+quote(writer)+" NOLOGIN NOSUPERUSER NOBYPASSRLS"); err != nil {
			t.Fatal(err)
		}
		defer func() {
			for _, statement := range []string{"REVOKE " + quote(writer) + " FROM " + quote(appRole), "ALTER ROLE " + quote(appRole) + " INHERIT", "DROP ROLE " + quote(writer)} {
				if _, err := admin.Exec(ctx, statement); err != nil {
					t.Error("disposable writer cleanup:", err)
				}
			}
		}()
		if _, err := owner.Exec(ctx, "GRANT TRUNCATE ON audit_log TO "+quote(writer)); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if _, err := owner.Exec(ctx, "REVOKE TRUNCATE ON audit_log FROM "+quote(writer)); err != nil {
				t.Error(err)
			}
		}()
		if _, err := admin.Exec(ctx, "ALTER ROLE "+quote(appRole)+" NOINHERIT; GRANT "+quote(writer)+" TO "+quote(appRole)); err != nil {
			t.Fatal(err)
		}
		if err := CheckRuntimePrivileges(ctx, app, "public"); err == nil {
			t.Fatal("NOINHERIT role with assumable TRUNCATE authority passed")
		}
		connection, err := app.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer connection.Release()
		defer func() {
			if _, err := connection.Exec(ctx, "RESET ROLE"); err != nil {
				t.Error(err)
			}
		}()
		if _, err := connection.Exec(ctx, "SET ROLE "+quote(writer)); err != nil {
			t.Fatal(err)
		}
		tx, err := connection.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, operationErr := tx.Exec(ctx, "TRUNCATE audit_log")
		rollbackErr := tx.Rollback(ctx)
		if operationErr != nil || rollbackErr != nil {
			t.Fatal("assumable writer authority control was not decisive")
		}
	})
	if _, err := app.Exec(ctx, `INSERT INTO audit_log
 (seq,event_id,correlation_id,domain,event_type,event_class,tenant_id,source,occurred_at,recorded_at,kind,summary,prev_hash,hash)
 VALUES (1,'dummy-event','dummy-correlation','test','test.event','FACT','dummy-tenant','fixture',now(),now(),'event','preserve this row','','dummy-hash')`); err != nil {
		t.Fatal("runtime INSERT failed:", err)
	}
	testRuntimeReplicationAuthority(t, ctx, fixture)
	for _, tc := range []struct{ operation, sql, code string }{
		{"UPDATE", "UPDATE audit_log SET summary='mutant'", "P0001"},
		{"DELETE", "DELETE FROM audit_log", "P0001"},
		{"TRUNCATE", "TRUNCATE audit_log", "42501"},
		{"ALTER", "ALTER TABLE audit_log DISABLE TRIGGER audit_log_no_mutate", "42501"},
	} {
		t.Run(tc.operation, func(t *testing.T) {
			_, err := app.Exec(ctx, tc.sql)
			pgError, ok := err.(*pgconn.PgError)
			if !ok || pgError.Code != tc.code {
				t.Fatalf("runtime %s did not fail with %s: %v", tc.operation, tc.code, err)
			}
		})
	}
	// An owner can bypass a DELETE row trigger through TRUNCATE/ALTER. This is
	// the affected control, rolled back before continuing; the startup gate must
	// refuse that identity rather than claiming the trigger confines its owner.
	for _, sql := range []string{"TRUNCATE audit_log", "ALTER TABLE audit_log DISABLE TRIGGER audit_log_no_mutate"} {
		tx, err := owner.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, operationErr := tx.Exec(ctx, sql)
		rollbackErr := tx.Rollback(ctx)
		if operationErr != nil || rollbackErr != nil {
			t.Fatal("affected owner control failed:", operationErr, rollbackErr)
		}
	}
	t.Run("downscoped-session-owner", func(t *testing.T) {
		if _, err := admin.Exec(ctx, "GRANT "+quote(appRole)+" TO "+quote(migrationRole)); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if _, err := admin.Exec(ctx, "REVOKE "+quote(appRole)+" FROM "+quote(migrationRole)); err != nil {
				t.Error(err)
			}
		}()
		configuration, err := pgxpool.ParseConfig(fixture.migrationURL)
		if err != nil {
			t.Fatal("parse disposable migration input")
		}
		configuration.MaxConns = 1
		configuration.AfterConnect = func(ctx context.Context, connection *pgx.Conn) error {
			_, err := connection.Exec(ctx, "SET ROLE "+quote(appRole))
			return err
		}
		downscoped, err := pgxpool.NewWithConfig(ctx, configuration)
		if err != nil {
			t.Fatal("open disposable downscoped pool")
		}
		defer downscoped.Close()
		if err := CheckRuntimePrivileges(ctx, downscoped, "public"); err == nil {
			t.Fatal("owner session hidden behind SET ROLE passed runtime check")
		}
		connection, err := downscoped.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer connection.Release()
		var current, session string
		if err := connection.QueryRow(ctx, "SELECT current_user, session_user").Scan(&current, &session); err != nil || current != appRole || session != migrationRole {
			t.Fatal("downscoped affected control did not resolve both identities")
		}
		if _, err := connection.Exec(ctx, "RESET ROLE"); err != nil {
			t.Fatal("owner session could not recover its actual authority")
		}
		tx, err := connection.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, operationErr := tx.Exec(ctx, "TRUNCATE audit_log")
		rollbackErr := tx.Rollback(ctx)
		if operationErr != nil || rollbackErr != nil {
			t.Fatal("RESET ROLE owner control was not decisive")
		}
	})
	for _, tc := range []struct {
		name, grant, revoke string
		setupIdentity       bool
	}{
		{"direct-truncate", "GRANT TRUNCATE ON audit_log TO " + quote(appRole), "REVOKE TRUNCATE ON audit_log FROM " + quote(appRole), false},
		{"public-truncate", "GRANT TRUNCATE ON audit_log TO PUBLIC", "REVOKE TRUNCATE ON audit_log FROM PUBLIC", false},
		{"trigger-authority", "GRANT TRIGGER ON audit_log TO " + quote(appRole), "REVOKE TRIGGER ON audit_log FROM " + quote(appRole), false},
		{"schema-create", "GRANT CREATE ON SCHEMA public TO " + quote(appRole), "REVOKE CREATE ON SCHEMA public FROM " + quote(appRole), false},
		{"database-create", "GRANT CREATE ON DATABASE " + quote(app.Config().ConnConfig.Database) + " TO " + quote(appRole), "REVOKE CREATE ON DATABASE " + quote(app.Config().ConnConfig.Database) + " FROM " + quote(appRole), false},
		{"security-definer", "CREATE FUNCTION fixture_definer() RETURNS integer LANGUAGE sql SECURITY DEFINER AS 'SELECT 1'", "DROP FUNCTION fixture_definer()", false},
		{"assumable-owner", "GRANT " + quote(migrationRole) + " TO " + quote(appRole), "REVOKE " + quote(migrationRole) + " FROM " + quote(appRole), true},
		{"predefined-capability", "GRANT pg_read_all_data TO " + quote(appRole), "REVOKE pg_read_all_data FROM " + quote(appRole), true},
		{"role-createrole", "ALTER ROLE " + quote(appRole) + " CREATEROLE", "ALTER ROLE " + quote(appRole) + " NOCREATEROLE", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			executor := owner
			if tc.setupIdentity {
				if _, err := admin.Exec(ctx, tc.grant); err != nil {
					t.Fatal(err)
				}
				defer func() {
					_, err := admin.Exec(ctx, tc.revoke)
					if err != nil {
						t.Error(err)
					}
				}()
			} else {
				if _, err := executor.Exec(ctx, tc.grant); err != nil {
					t.Fatal(err)
				}
				defer func() {
					_, err := executor.Exec(ctx, tc.revoke)
					if err != nil {
						t.Error(err)
					}
				}()
			}
			if err := CheckRuntimePrivileges(ctx, app, "public"); err == nil {
				t.Fatal("unsafe runtime authority mutation accepted")
			}
		})
	}
	if err := CheckRuntimePrivileges(ctx, app, "public"); err != nil {
		t.Fatal("restored restricted runtime refused:", err)
	}
	var count int
	var summary string
	if err := app.QueryRow(ctx, "SELECT count(*), min(summary) FROM audit_log").Scan(&count, &summary); err != nil || count != 1 || summary != "preserve this row" {
		t.Fatal("a refused or rolled-back operation changed the audit row:", err)
	}
}

func runtimeDeleteControl(t *testing.T, ctx context.Context, tx pgx.Tx, bypass bool) {
	t.Helper()
	result, operationErr := tx.Exec(ctx, "DELETE FROM public.audit_log")
	rollbackErr := tx.Rollback(ctx)
	if rollbackErr != nil {
		t.Fatal("rollback-only trigger control failed")
	}
	if bypass {
		if operationErr != nil || result.RowsAffected() != 1 {
			t.Fatal("replica trigger-bypass control was not decisive")
		}
	} else {
		pgError, ok := operationErr.(*pgconn.PgError)
		if !ok || pgError.Code != "P0001" {
			t.Fatal("safe trigger mode failed to enforce WORM")
		}
	}
}

func testRuntimeReplicationAuthority(t *testing.T, ctx context.Context, fixture *runtimeRoleFixture) {
	t.Helper()
	quote := func(name string) string { return pgx.Identifier{name}.Sanitize() }
	for _, privilege := range []string{"SET", "ALTER SYSTEM"} {
		t.Run("replication-parameter-"+strings.ToLower(strings.ReplaceAll(privilege, " ", "-")), func(t *testing.T) {
			if _, err := fixture.admin.Exec(ctx, "GRANT "+privilege+" ON PARAMETER session_replication_role TO "+quote(fixture.appRole)); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if _, err := fixture.admin.Exec(ctx, "REVOKE "+privilege+" ON PARAMETER session_replication_role FROM "+quote(fixture.appRole)); err != nil {
					t.Error(err)
				}
			}()
			if err := CheckRuntimePrivileges(ctx, fixture.app, "public"); err == nil {
				t.Fatal("replication-parameter authority was certified")
			}
			if privilege == "SET" {
				tx, err := fixture.app.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = tx.Rollback(ctx) }()
				if _, err := tx.Exec(ctx, "SET LOCAL session_replication_role=replica"); err != nil {
					t.Fatal("actual SET grant was not effective")
				}
				runtimeDeleteControl(t, ctx, tx, true)
			}
			applyStagedRuntimeSQL(t, ctx, fixture.admin, fixture.appRole, "harden-runtime-replication.sql")
			if err := CheckRuntimePrivileges(ctx, fixture.app, "public"); err != nil {
				t.Fatal("parameter-administrator-restricted runtime refused")
			}
		})
	}
	for _, mode := range []string{"replica", "local"} {
		t.Run("admin-default-"+mode, func(t *testing.T) {
			if _, err := fixture.admin.Exec(ctx, "ALTER ROLE "+quote(fixture.appRole)+" SET session_replication_role="+mode); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if _, err := fixture.admin.Exec(ctx, "ALTER ROLE "+quote(fixture.appRole)+" RESET session_replication_role"); err != nil {
					t.Error(err)
				}
			}()
			configuration, err := pgxpool.ParseConfig(fixture.appURL)
			if err != nil {
				t.Fatal("parse disposable runtime input")
			}
			configuration.MaxConns = 1
			pool, err := pgxpool.NewWithConfig(ctx, configuration)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			var actual string
			if err := pool.QueryRow(ctx, "SELECT pg_catalog.current_setting('session_replication_role')").Scan(&actual); err != nil || actual != mode {
				t.Fatal("administrator-supplied session default was not applied")
			}
			gateErr := CheckRuntimePrivileges(ctx, pool, "public")
			if (mode == "replica" && gateErr == nil) || (mode == "local" && gateErr != nil) {
				t.Fatal("actual trigger-mode gate returned the wrong decision")
			}
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			runtimeDeleteControl(t, ctx, tx, mode == "replica")
		})
	}
	t.Run("assumable-replication-set", func(t *testing.T) {
		writer := fixture.appRole + "_replication"
		if _, err := fixture.admin.Exec(ctx, "CREATE ROLE "+quote(writer)+" NOLOGIN NOSUPERUSER NOBYPASSRLS"); err != nil {
			t.Fatal(err)
		}
		defer func() {
			for _, statement := range []string{"REVOKE " + quote(writer) + " FROM " + quote(fixture.appRole), "ALTER ROLE " + quote(fixture.appRole) + " INHERIT", "REVOKE SET ON PARAMETER session_replication_role FROM " + quote(writer), "DROP ROLE " + quote(writer)} {
				if _, err := fixture.admin.Exec(ctx, statement); err != nil {
					t.Error("disposable parameter-role cleanup failed")
				}
			}
		}()
		if _, err := fixture.owner.Exec(ctx, "GRANT DELETE ON public.audit_log TO "+quote(writer)); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if _, err := fixture.owner.Exec(ctx, "REVOKE DELETE ON public.audit_log FROM "+quote(writer)); err != nil {
				t.Error(err)
			}
		}()
		if _, err := fixture.admin.Exec(ctx, "ALTER ROLE "+quote(fixture.appRole)+" NOINHERIT; GRANT "+quote(writer)+" TO "+quote(fixture.appRole)+"; GRANT SET ON PARAMETER session_replication_role TO "+quote(writer)); err != nil {
			t.Fatal(err)
		}
		if err := CheckRuntimePrivileges(ctx, fixture.app, "public"); err == nil {
			t.Fatal("assumable parameter-setting authority passed")
		}
		tx, err := fixture.app.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+quote(writer)+"; SET LOCAL session_replication_role=replica"); err != nil {
			t.Fatal("assumable SET control failed")
		}
		runtimeDeleteControl(t, ctx, tx, true)
	})
	t.Run("catalog-shadow-refused", func(t *testing.T) {
		if _, err := fixture.owner.Exec(ctx, "CREATE SCHEMA runtime_shadow; GRANT USAGE,CREATE ON SCHEMA runtime_shadow TO "+quote(fixture.appRole)); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if _, err := fixture.owner.Exec(ctx, "DROP SCHEMA runtime_shadow CASCADE"); err != nil {
				t.Error(err)
			}
		}()
		if _, err := fixture.app.Exec(ctx, "CREATE FUNCTION runtime_shadow.has_parameter_privilege(oid,text,text) RETURNS boolean LANGUAGE sql AS 'SELECT false'"); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.admin.Exec(ctx, "GRANT SET ON PARAMETER session_replication_role TO "+quote(fixture.appRole)); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if _, err := fixture.admin.Exec(ctx, "REVOKE SET ON PARAMETER session_replication_role FROM "+quote(fixture.appRole)); err != nil {
				t.Error(err)
			}
		}()
		configuration, err := pgxpool.ParseConfig(fixture.appURL)
		if err != nil {
			t.Fatal("parse disposable shadow input")
		}
		configuration.MaxConns = 1
		configuration.AfterConnect = func(ctx context.Context, connection *pgx.Conn) error {
			_, err := connection.Exec(ctx, "SET search_path=runtime_shadow,public,pg_catalog")
			return err
		}
		pool, err := pgxpool.NewWithConfig(ctx, configuration)
		if err != nil {
			t.Fatal(err)
		}
		defer pool.Close()
		var shadow, actual bool
		if err := pool.QueryRow(ctx, `SELECT has_parameter_privilege(r.oid,'session_replication_role','SET'),pg_catalog.has_parameter_privilege(r.oid,'session_replication_role','SET') FROM pg_catalog.pg_roles r WHERE r.rolname=current_user`).Scan(&shadow, &actual); err != nil || shadow || !actual {
			t.Fatal("catalog shadow control did not distinguish the real authority")
		}
		if err := CheckRuntimePrivileges(ctx, pool, "public"); err == nil {
			t.Fatal("caller-owned catalog-function shadow spoofed the gate")
		}
		applyStagedRuntimeSQL(t, ctx, fixture.admin, fixture.appRole, "harden-runtime-replication.sql")
		if err := CheckRuntimePrivileges(ctx, pool, "public"); err != nil {
			t.Fatal("restricted identity with harmless caller schema refused")
		}
		var schemas []string
		if err := pool.QueryRow(ctx, "SELECT pg_catalog.current_schemas(false)").Scan(&schemas); err != nil || strings.Join(schemas, ",") != "runtime_shadow,public,pg_catalog" {
			t.Fatal("catalog check changed the caller's session search path")
		}
	})
}
