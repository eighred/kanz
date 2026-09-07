// Package pg builds every Postgres pool this platform is allowed to have.
//
// # Why this exists
//
// Two properties, both of which were previously a convention repeated per service
// and therefore skippable.
//
// TENANT SCOPE. Every tenant-scoped table enforces isolation with row-level
// security keyed on the `app.tenant_id` GUC, so a connection that does not set it
// can read nothing. Eight services each carried their own copy of the
// pgxpool.Config + AfterConnect that sets it — which meant the platform's most
// important safety property was a convention, repeated eight times, and a ninth
// service could simply not repeat it.
//
// That is not hypothetical. accounting shipped with `pgxpool.New()` and no
// AfterConnect at all: its ledger reads returned nothing and its writes failed, and
// every test was green, because the tests set the GUC on their own pools. The service
// was silently unable to write a single row of the book of record.
//
// SIZE AND TIME BOUNDS (#228). Nothing in the estate set MaxConns or a
// statement_timeout — a repo-wide grep for either returned zero. pgx defaults
// MaxConns to max(4, runtime.NumCPU()), and runtime.NumCPU() in a container with no
// CPU quota reports the NODE's core count: on a 16-core node every pod silently took
// 16 connections against a server defaulting to max_connections=100. Pools are lazy,
// so nothing failed until traffic arrived, and then it surfaced as
// `FATAL: sorry, too many clients already` on a query in whichever service warmed
// last — not at startup, and not phrased as "the pool is too big". Meanwhile
// statement_timeout=0 meant one blocked query held its connection forever, which is
// how the pool got exhausted in the first place.
//
// So there is ONE place that builds a pool, it REFUSES an empty tenant, it REFUSES a
// pool it has not sized, and the database will not answer an unscoped connection
// anyway (MT-01e: app_current_tenant() RAISES rather than returning NULL, so an
// unscoped query ERRORS instead of quietly returning zero rows). Belt at the
// composition root, braces at the engine.
//
// test/arch/pool_budget_test.go is the guard: it re-derives the estate's connection
// demand from infra/deploy on every run, and it fails any composition root that
// calls pgxpool.New directly and goes around all of this.
package pg

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Unbounded is a timeout that is deliberately absent, as distinct from one nobody
// filled in. Configure REFUSES a Profile whose Name or MaxConns is zero, so the
// zero value of a Profile can never reach a pool; Unbounded is how a profile says
// "this bound is off, and that was a decision" in a way a reader can tell apart
// from an omission.
const Unbounded time.Duration = 0

// ServiceMaxConns is every service pool's ceiling, and the term the estate's
// connection budget multiplies. The arithmetic that turns it into
// max_connections lives in infra/deploy/postgres-dev.yaml, next to the number it
// produces, and test/arch/pool_budget_test.go re-derives it from the manifests.
//
// WHY 4. pgx's own default is max(4, runtime.NumCPU()), so 4 is its floor: no
// service loses capacity relative to a 4-core box, and the only thing removed is
// the dependence on how many cores the NODE happens to have. Going lower would be
// a throughput change to services nobody has profiled. Going higher would fund
// concurrency the pods cannot execute — infra/deploy/availability.yaml's
// LimitRange gives every container 500m of CPU request and a 1 CPU limit, and a
// pod capped at one core cannot usefully run more than a handful of statements at
// once; the surplus would queue in Postgres, holding a backend each, which is
// strictly worse than queueing in the client pool.
const ServiceMaxConns int32 = 4

// OperatorReserve is the slice of max_connections that is NOT the estate's:
// Postgres's own superuser_reserved_connections (3 on the rig, and its default),
// plus room for a human with psql, a DR restore and the operator. Without it the
// first thing to fail when the estate is at its ceiling is the ability to log in
// and find out why.
const OperatorReserve = 13

// Profile is the sizing and the time bounds applied to one kind of pool. There
// are exactly two, below; a caller does not build one.
type Profile struct {
	// Name appears in the refusal when a pool is built unsized, and in
	// application_name so pg_stat_activity says which pool a backend belongs to.
	Name string

	MaxConns int32
	MinConns int32

	MaxConnLifetime   time.Duration
	MaxConnIdleTime   time.Duration
	HealthCheckPeriod time.Duration

	// StatementTimeout, LockTimeout and IdleInTxTimeout are set as startup-packet
	// runtime parameters, so they are in force before the first query — including
	// before AfterConnect's set_config. Unbounded means the GUC is set to 0
	// explicitly, which is not the same as leaving it unset: `0` in
	// pg_settings.setting with `client` as its source is a decision on the record.
	StatementTimeout time.Duration
	LockTimeout      time.Duration
	IdleInTxTimeout  time.Duration
}

// serviceStatementTimeout is how long any one service statement may hold a
// Postgres backend.
//
// IT IS A CONNECTION-HOLDING BOUND, NOT A LATENCY SLO. It is far above any healthy
// query here — the request paths are indexed point reads — and that is deliberate:
// the number exists so that no single statement can hold a backend indefinitely,
// not to express what a caller will wait for.
//
// IT MUST BE STRICTLY BELOW httpserver.standardWriteTimeout (120s), WHICH IS THE
// WHOLE POINT OF THE VALUE. Every service behind the gateway takes
// httpserver.Standard(), including audit — the one serving the unbounded full-log
// export. Timeouts.Write bounds the entire response and fires by SEVERING the
// connection: no status, no body, nothing naming what was slow. So the two bounds
// race, and whichever fires first decides what the caller is told:
//
//	statement_timeout first → the handler gets SQLSTATE 57014 and renders a real
//	                          response (audit's handleReport routes the query error
//	                          through Server.fail, so it is a status, not a hang).
//	WriteTimeout first      → an anonymous EOF, indistinguishable from a broken
//	                          database, and the handler's diagnosis is never
//	                          written.
//
// The named error must win. They were EQUAL at 120s when #228 first landed, which
// is a coin toss between those two outcomes — the exact inversion #303's own guard
// (test/arch/http_server_timeouts_test.go) exists to prevent one layer out.
//
// 90s, leaving 30s under the write bound for the handler to RENDER after the query
// returns — full-log json.MarshalIndents the whole result set, which is not free
// on a result set large enough to have taken this long to fetch. 30s is the same
// headroom proxy.copilotForwardTimeout takes under the same 120s bound, so the
// estate has one answer to "how much room does a budget nested under
// standardWriteTimeout need" rather than two.
//
// LOWERING IT FROM 120s LOSES NO REQUEST THAT WOULD HAVE SUCCEEDED. A query
// finishing between 90s and 120s left no time to marshal and flush before the
// write deadline severed the socket, so its only possible outcome was already the
// anonymous EOF. This converts that outcome into a named one; it does not take a
// result away from anybody.
//
// Three queries in this estate were unbounded by construction and were checked
// against this number before it was chosen. TWO REMAIN:
//
//   - ledger.Journal — a portfolio's whole journal, no LIMIT (accounting's
//     Snapshotter, and MaterializeCurrent's no-checkpoint fallback).
//   - ledger.StalePortfolios — an aggregate over the journal index, on a
//     background ticker.
//
// The third was audit's `full-log` report — audit.Filter{} with no window and no
// cap, rendered whole in memory. IT IS NO LONGER UNBOUNDED (#304): every report
// template now carries a page size, the route pages on a Seq cursor, and a
// partial export declares itself so a truncation cannot be read as a complete
// log. It is listed here only so this paragraph is not read as still describing
// it — the audit report is no longer one of the queries this timeout is sized to
// survive.
//
// Neither remaining query approaches 90s at this estate's data volume, and both
// are OFF the request path — they are re-run on the next tick, so a 57014 there
// costs a cycle, not a result. That is a materially safer position than when
// this number was chosen, because the one request-path query in the list is the
// one that got fixed. If one of the two legitimately needs longer, the answer is
// WithStatementTimeout on that query — NOT raising this number until nothing
// complains, which would give both back the unbounded hold this bound exists to
// remove, and would walk it back into the write bound.
//
// A literal `N * time.Unit` product, and a const rather than an inline field, so
// that test/arch/http_server_timeouts_test.go can read it out of this source and
// keep it under standardWriteTimeout. See standardWriteMustExceed there.
const serviceStatementTimeout = 90 * time.Second

// Service is the profile every long-lived service pool gets.
//
// LOCK TIMEOUT is much shorter on purpose. A service statement that cannot take
// its lock in 5s is queued behind DDL or a leaked transaction, and Postgres's lock
// queue is FIFO: joining it puts every later reader behind this statement too. A
// fast refusal keeps the failure to the one caller.
//
// IDLE-IN-TRANSACTION is 60s. A transaction left open holds a backend AND pins the
// vacuum horizon; every transaction on this estate is a single statement or one
// short fold, so 60s can only be reached by a bug, and killing it is how the bug
// becomes visible.
var Service = Profile{
	Name:     "service",
	MaxConns: ServiceMaxConns,
	// One warm connection, not zero. A lazy pool means a DSN that cannot connect
	// is discovered by the first event rather than at startup, and it means an
	// over-subscribed estate stays invisible until load arrives — which is exactly
	// how #228 was going to be found.
	MinConns: 1,
	// Recycle rather than pin. A pod that lives for weeks otherwise holds the same
	// backends for weeks, so a failover, a role change or a bloated backend is
	// never shed.
	MaxConnLifetime:   30 * time.Minute,
	MaxConnIdleTime:   5 * time.Minute,
	HealthCheckPeriod: time.Minute,

	StatementTimeout: serviceStatementTimeout,
	LockTimeout:      5 * time.Second,
	IdleInTxTimeout:  60 * time.Second,
}

// Migration is kanz-migrate's profile, and it is deliberately NOT the service one.
//
// MaxConns 1: a migration run is serial and session-scoped — it takes one
// connection, holds a session advisory lock on it, and applies each migration in a
// transaction on that same connection. A second connection would be a second
// migrator.
//
// StatementTimeout Unbounded: DDL legitimately runs long (an index build, a table
// rewrite), and the run ALREADY has exactly one bound — kanz-migrate's -timeout
// flag, which is the context deadline pgx cancels on. A second, smaller bound here
// would make a migration fail at a duration nobody granted while the operator
// believes they granted two minutes.
//
// AND IT IS UNAFFECTED BY httpserver.standardWriteTimeout, which is why this stays
// Unbounded while Service does not. serviceStatementTimeout must sit under the
// write bound because a service statement runs inside an HTTP handler whose
// response is being raced. kanz-migrate serves nothing: it imports no net/http, not
// even transitively (`go list -deps ./cmd/kanz-migrate` names no httpserver), and
// it is an initContainer that exits. There is no response to sever, so there is no
// ordering to respect.
//
// LockTimeout 10s, and this is the one that matters: a queued ALTER TABLE takes
// every later reader down with it, because Postgres's lock queue is FIFO. Failing
// fast leaves the database serving and the initContainer retrying — the rollout
// stalls, the platform does not.
//
// The advisory lock is the deliberate exception and internal/migrate lifts the
// bound for exactly that statement: lock_timeout aborts pg_advisory_lock too
// (measured against a real Postgres 16 — 702ms, SQLSTATE 55P03, on a 700ms
// lock_timeout), so leaving it in force would turn the documented "second replica
// waits its turn" into a crash-looping initContainer.
//
// IdleInTxTimeout Unbounded for the same reason as the statement timeout: a
// migration's transaction is legitimately long, and the run's deadline bounds it.
var Migration = Profile{
	Name:     "migration",
	MaxConns: 1,
	MinConns: 1,
	// A migration run is seconds to minutes and then the process exits; recycling
	// a connection mid-run would drop the session advisory lock the whole design
	// depends on.
	MaxConnLifetime:   time.Hour,
	MaxConnIdleTime:   time.Hour,
	HealthCheckPeriod: time.Minute,

	StatementTimeout: Unbounded,
	LockTimeout:      10 * time.Second,
	IdleInTxTimeout:  Unbounded,
}

// NewTenantPool opens a pool whose every connection is scoped to tenant.
//
// The tenant is set with set_config(..., false) — session-level, not
// transaction-level — so it survives for the life of the connection and applies to
// every query the pool hands out, including ones outside a transaction.
//
// An empty tenant is refused. It would be indistinguishable, at the database, from a
// service that forgot to scope itself at all — and that is the failure this whole
// mechanism exists to make impossible.
func NewTenantPool(ctx context.Context, dsn, tenant string) (*pgxpool.Pool, error) {
	if tenant == "" {
		return nil, errors.New("pg: empty tenant — a pool with no tenant can read nothing (RLS) and would be " +
			"indistinguishable from a service that forgot to scope itself")
	}
	cfg, err := prepare(dsn, Service)
	if err != nil {
		return nil, err
	}
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		// If this fails, pgx discards the connection and the caller gets the error.
		// A connection that could not be scoped must never be handed to a query.
		if _, err := conn.Exec(ctx, "SELECT set_config('app.tenant_id', $1, false)", tenant); err != nil {
			return fmt.Errorf("pg: could not scope the connection to tenant %q: %w", tenant, err)
		}
		// AND THEN ASK WHETHER THAT SCOPE MEANS ANYTHING (#634).
		//
		// Every tenant-isolation policy on this platform is FORCE ROW LEVEL
		// SECURITY plus USING (tenant_id = app_current_tenant()). Postgres exempts
		// a SUPERUSER and a BYPASSRLS role from RLS UNCONDITIONALLY — FORCE does
		// not reach either. So the whole guarantee rested on one property of the
		// DSN, asserted in fifteen comments, set by a single CREATE ROLE line in a
		// DEV-ONLY manifest, and checked by nothing that runs. A repo-wide grep for
		// the only ways to ask — rolsuper, rolbypassrls, pg_roles, pg_has_role —
		// returned zero hits in any .go file.
		//
		// The failure it prevents is the one AGENTS.md's standards forbid by name:
		// a Vault entry written with the wrong role produces a service that starts
		// cleanly, passes readiness, sets app.tenant_id on every connection, logs
		// nothing unusual, and serves every tenant's rows to every tenant.
		// "Nothing configured" and "checked, and fine" are the same observable
		// event.
		//
		// pg_has_role RATHER THAN A LOOKUP ON current_user ALONE, because BYPASSRLS
		// is INHERITED through role membership. A role that is not itself
		// rolbypassrls but is a member of one that is bypasses RLS just the same,
		// and a check on the login role's own attributes would report that estate
		// as safe. bool_or over every role this user holds is the question actually
		// being asked: can this connection see past a policy.
		//
		// REFUSED HERE RATHER THAN LOGGED, and in AfterConnect rather than once at
		// startup: pgx discards a connection whose AfterConnect fails, so a pool
		// that cannot prove it is subject to RLS hands out nothing at all. A
		// warning would leave the service running and serving.
		var exempt bool
		if err := conn.QueryRow(ctx,
			`SELECT COALESCE(bool_or(r.rolsuper OR r.rolbypassrls), false)
			   FROM pg_roles r
			  WHERE pg_has_role(current_user, r.oid, 'USAGE')`,
		).Scan(&exempt); err != nil {
			return fmt.Errorf("pg: could not determine whether this role is exempt from row-level "+
				"security, so tenant isolation for %q cannot be established: %w", tenant, err)
		}
		if exempt {
			return fmt.Errorf("pg: refusing a tenant pool for %q — this role is SUPERUSER or holds "+
				"BYPASSRLS (directly or through role membership), and Postgres exempts both from row-level "+
				"security unconditionally. FORCE ROW LEVEL SECURITY does not reach them, so every "+
				"tenant-isolation policy on this connection is a no-op and this pool would serve every "+
				"tenant's rows to %[1]q. Point this service at its NOSUPERUSER application role "+
				"(see infra/security/secrets/secretproviderclass.yaml), not the DDL or admin role", tenant)
		}
		return nil
	}
	pool, err := open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	// FORCE ONE CONNECTION NOW, so the checks above run at STARTUP (#634).
	//
	// pgxpool.NewWithConfig is lazy: without this, AfterConnect first runs on the
	// first Acquire, so a service pointed at an RLS-exempt role would start
	// cleanly, report ready, and only fail once traffic arrived. For an isolation
	// property that is the wrong moment to find out — a pod that cannot prove it
	// is subject to RLS should never reach the load balancer.
	//
	// THE COST IS DELIBERATE AND IS SCOPED TO TENANT POOLS. A tenant pool now
	// requires the database to be reachable at construction; an unreachable one
	// fails the process rather than starting it. That is the correct posture
	// here — the alternative is a service that is Ready while unable to
	// demonstrate the single property every tenant's data rests on. NewGlobalPool
	// is untouched and stays lazy: its stores have no RLS by design, and its
	// whyNoTenant argument already says so.
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pg: tenant pool for %q could not establish a scoped connection: %w", tenant, err)
	}
	return pool, nil
}

// NewGlobalPool opens a service pool for a store that deliberately has NO
// row-level security — reference and market data that is the same fact for every
// tenant (market-data's price_observations, the audit log, the regulatory link
// chain, the schema registry).
//
// whyNoTenant is required and must be non-empty. Before #228 all five of those
// pools were a bare pgxpool.New, which made "this store has no RLS on purpose"
// and "this service forgot to scope itself" the same line of code — the second
// being precisely the accounting defect described in this package's header. The
// reason string is what makes them different at the call site, and an empty one is
// refused rather than defaulted.
func NewGlobalPool(ctx context.Context, dsn, whyNoTenant string) (*pgxpool.Pool, error) {
	if strings.TrimSpace(whyNoTenant) == "" {
		return nil, errors.New("pg: NewGlobalPool needs a written reason this store has no tenant scope — " +
			"an unscoped pool with no stated reason is indistinguishable from a service that forgot to scope " +
			"itself, which is the failure internal/pg exists to make impossible")
	}
	cfg, err := prepare(dsn, Service)
	if err != nil {
		return nil, err
	}
	return open(ctx, cfg)
}

// NewMigrationPool opens kanz-migrate's pool. See Migration for why its bounds are
// not the service ones.
func NewMigrationPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := prepare(dsn, Migration)
	if err != nil {
		return nil, err
	}
	return open(ctx, cfg)
}

// WithStatementTimeout runs fn inside a transaction whose statement_timeout is d,
// and is the ONLY sanctioned way past Service.StatementTimeout.
//
// It exists so that a query which legitimately runs longer than the platform bound
// says so at its own call site, in a transaction, for the duration of that one
// statement — instead of the bound being raised globally and every other query
// getting the unbounded backend hold back. SET LOCAL is scoped to the transaction,
// so the connection returns to the pool with the platform bound restored even if
// fn panics or the transaction rolls back.
//
// d must be positive: this is an escape hatch to a LONGER bound, not a way to spell
// Unbounded from a service. Nothing in the estate needs it today — see
// serviceStatementTimeout for the three unbounded-by-construction queries and why
// each fits inside 90s — and its Postgres-gated test is what keeps it working until
// one does.
//
// ON AN HTTP REQUEST PATH IT IS NOT ENOUGH ON ITS OWN. serviceStatementTimeout sits
// under httpserver.standardWriteTimeout on purpose; raising it past that for a
// request handler swaps a named 57014 for a severed connection with no status. A
// handler that genuinely needs longer needs its server's Write bound raised in the
// same change, and test/arch/http_server_timeouts_test.go will say so.
func WithStatementTimeout(ctx context.Context, pool *pgxpool.Pool, d time.Duration, fn func(pgx.Tx) error) error {
	if d <= 0 {
		return fmt.Errorf("pg: WithStatementTimeout needs a positive bound, got %s — a service may raise the "+
			"platform's %s statement_timeout for one query, never remove it", d, Service.StatementTimeout)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("pg: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// SET does not take bind parameters; the value is an integer this function
	// computed, never caller text.
	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = "+millis(d)); err != nil {
		return fmt.Errorf("pg: raise statement_timeout to %s: %w", d, err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// prepare parses dsn and applies p. Exposed to tests (and to the arch guard)
// through Configure below, which is what makes "the constructor sizes the pool" a
// behavioural assertion rather than a grep.
func prepare(dsn string, p Profile) (*pgxpool.Config, error) {
	if dsn == "" {
		return nil, errors.New("pg: empty DSN")
	}
	if err := refuseSelfSizedDSN(dsn, p); err != nil {
		return nil, err
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("pg: parse DSN: %w", err)
	}
	if err := p.Configure(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Configure applies a profile to a parsed pool config.
//
// It REFUSES a Profile it was not given a size for. pgxpool.ParseConfig has
// already filled cfg.MaxConns with max(4, runtime.NumCPU()) by the time this runs,
// so a Configure that silently skipped an unset MaxConns would leave the node's
// core count in place and look identical, on inspection, to a pool that had been
// sized on purpose. That is the shape #228 was.
func (p Profile) Configure(cfg *pgxpool.Config) error {
	if p.Name == "" || p.MaxConns <= 0 {
		return fmt.Errorf("pg: refusing to build an unsized pool (profile %q, MaxConns %d). Without an "+
			"explicit MaxConns pgx uses max(4, runtime.NumCPU()), and in a container with no CPU quota "+
			"runtime.NumCPU() is the NODE's core count — every pod would take a share of max_connections "+
			"nobody chose, and the estate would only find out under load, as `sorry, too many clients "+
			"already` on a query in whichever service warmed last", p.Name, p.MaxConns)
	}
	cfg.MaxConns = p.MaxConns
	cfg.MinConns = p.MinConns
	cfg.MaxConnLifetime = p.MaxConnLifetime
	cfg.MaxConnIdleTime = p.MaxConnIdleTime
	cfg.HealthCheckPeriod = p.HealthCheckPeriod

	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	// Startup-packet parameters, so they bind before the first statement — which
	// includes AfterConnect's set_config, and includes anything pgx itself runs.
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = millis(p.StatementTimeout)
	cfg.ConnConfig.RuntimeParams["lock_timeout"] = millis(p.LockTimeout)
	cfg.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] = millis(p.IdleInTxTimeout)
	if cfg.ConnConfig.RuntimeParams["application_name"] == "" {
		cfg.ConnConfig.RuntimeParams["application_name"] = appName() + "/" + p.Name
	}
	return nil
}

func open(ctx context.Context, cfg *pgxpool.Config) (*pgxpool.Pool, error) {
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("pg: open pool: %w", err)
	}
	return pool, nil
}

// refuseSelfSizedDSN rejects a DSN carrying pgxpool's own pool_* parameters.
//
// Configure overwrites them, so such a DSN is not dangerous — it is WORSE than
// dangerous in the way this repository cares about: an operator sets
// pool_max_conns in a Vault secret, the value is silently discarded, and the
// estate's real ceiling is somewhere neither of them is looking. A pool is sized
// in one place or the budget is not a budget.
func refuseSelfSizedDSN(dsn string, p Profile) error {
	var found []string
	for _, k := range dsnKeys(dsn) {
		if strings.HasPrefix(k, "pool_") {
			found = append(found, k)
		}
	}
	if len(found) == 0 {
		return nil
	}
	return fmt.Errorf("pg: this DSN sets %s, and internal/pg would silently overwrite them with the %q "+
		"profile (MaxConns %d). The estate's connection budget is derived in ONE place — see ServiceMaxConns "+
		"and infra/deploy/postgres-dev.yaml — so a pool sized in a DSN is a second answer that nothing "+
		"re-derives. Remove them, or change the profile", strings.Join(found, ", "), p.Name, p.MaxConns)
}

// dsnKeys returns the parameter names a DSN sets, for both shapes pgx accepts: a
// URL (postgres://…?k=v) and a keyword/value string (host=… k=v).
func dsnKeys(dsn string) []string {
	var out []string
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return nil
		}
		for k := range u.Query() {
			out = append(out, k)
		}
		return out
	}
	for _, field := range strings.Fields(dsn) {
		if k, _, ok := strings.Cut(field, "="); ok {
			out = append(out, k)
		}
	}
	return out
}

// millis renders a duration the way Postgres reads a bare integer timeout: in
// milliseconds. Unbounded renders "0", which is the GUC's own spelling for off —
// set explicitly, so pg_settings shows it came from the client rather than from
// nobody.
func millis(d time.Duration) string {
	return strconv.FormatInt(int64(d/time.Millisecond), 10)
}

// appName is the running binary, so pg_stat_activity attributes a backend to a
// service without every call site having to pass its own name. Diagnostic only —
// nothing branches on it.
func appName() string {
	if len(os.Args) == 0 {
		return "kanz"
	}
	return strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
}
