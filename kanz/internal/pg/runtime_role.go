package pg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// CheckRuntimePrivileges is an explicit startup gate for runtime consumers whose
// migration identity is separate. It checks the live catalog before those
// consumers start effects; it does not certify Vault delivery or continuously
// police later administrator changes. Migration pools deliberately do not use it.
// MEMBER is conservative: even non-inherited membership can permit SET ROLE.
// PostgreSQL's predefined pg_* roles confer capabilities outside this app's
// grant contract (including server-file/program and cross-database data access).
func CheckRuntimePrivileges(parent context.Context, pool *pgxpool.Pool, schema string) error {
	if pool == nil || schema == "" {
		return errors.New("pg: runtime privilege check requires a pool and schema")
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	var sameIdentity, dangerousRole, databaseOwner, databaseCreate, schemaDDL, objectDDL, writableBookkeeping bool
	var tables int64
	err := pool.QueryRow(ctx, `
WITH runtime_roles AS (
  SELECT r.oid FROM pg_roles r WHERE pg_has_role(session_user, r.oid, 'MEMBER')
)
SELECT
  current_user = session_user,
  EXISTS (SELECT 1 FROM pg_roles r
    WHERE pg_has_role(current_user, r.oid, 'MEMBER')
      AND (r.rolsuper OR r.rolbypassrls OR r.rolcreaterole OR r.rolcreatedb OR r.rolreplication OR left(r.rolname,3)='pg_')),
  pg_has_role(current_user, (SELECT datdba FROM pg_database WHERE datname=current_database()), 'MEMBER'),
  EXISTS (SELECT 1 FROM runtime_roles r WHERE has_database_privilege(r.oid, current_database(), 'CREATE')),
  EXISTS (SELECT 1 FROM pg_namespace n WHERE n.nspname=$1
    AND (pg_has_role(current_user, n.nspowner, 'MEMBER') OR
      EXISTS (SELECT 1 FROM runtime_roles r WHERE has_schema_privilege(r.oid, n.oid, 'CREATE')))),
  (SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
    WHERE n.nspname=$1 AND c.relkind IN ('r','p') AND c.relname <> 'schema_migrations'),
  EXISTS (
    SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
    WHERE n.nspname=$1 AND (
      pg_has_role(current_user, c.relowner, 'MEMBER') OR
      CASE WHEN c.relkind IN ('r','p','v','m','f')
        THEN EXISTS (SELECT 1 FROM runtime_roles r WHERE has_table_privilege(r.oid, c.oid, 'TRUNCATE,TRIGGER')) ELSE false END)
    UNION ALL
    SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
      WHERE n.nspname=$1 AND (pg_has_role(current_user, p.proowner, 'MEMBER') OR
        (p.prosecdef AND EXISTS (SELECT 1 FROM runtime_roles r WHERE has_function_privilege(r.oid, p.oid, 'EXECUTE'))))
    UNION ALL
    SELECT 1 FROM pg_type t JOIN pg_namespace n ON n.oid=t.typnamespace
      WHERE n.nspname=$1 AND pg_has_role(current_user, t.typowner, 'MEMBER')
  ),
  EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
    CROSS JOIN runtime_roles r WHERE n.nspname=$1 AND c.relname='schema_migrations' AND c.relkind IN ('r','p')
      AND (has_table_privilege(r.oid, c.oid, 'INSERT,UPDATE,DELETE') OR
        has_any_column_privilege(r.oid, c.oid, 'INSERT,UPDATE')))
`, schema).Scan(&sameIdentity, &dangerousRole, &databaseOwner, &databaseCreate, &schemaDDL, &tables, &objectDDL, &writableBookkeeping)
	if err != nil {
		return fmt.Errorf("pg: runtime privilege evidence unavailable: %w", err)
	}
	if tables == 0 {
		return errors.New("pg: runtime schema has no application tables; migration/privilege state is unknown")
	}
	if !sameIdentity {
		return errors.New("pg: runtime current_user differs from session_user; role-switching runtime connections are not supported")
	}
	if writableBookkeeping {
		return errors.New("pg: runtime identity can write migration bookkeeping; its owner must revoke effective table and column writes before startup")
	}
	if dangerousRole || databaseOwner || databaseCreate || schemaDDL || objectDDL {
		return errors.New("pg: runtime identity has privileged role, owner, CREATE, TRUNCATE, trigger or security-definer authority; use the restricted application credential")
	}
	return nil
}
