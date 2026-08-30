package outbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Enqueue writes records into the outbox INSIDE AN EXISTING TRANSACTION.
//
// IT TAKES A pgx.Tx, NOT A POOL, AND THAT IS THE WHOLE DESIGN. An outbox row
// written on its own connection is just a second independent write with extra
// steps — the exact defect this package replaces, reintroduced by a signature.
// Requiring the transaction makes "enqueue outside the state change" not a rule
// somebody has to remember but a thing that does not compile.
//
// THE ROW'S STORAGE TENANT IS THE CONNECTION'S; THE RECORD'S IS THE ENVELOPE'S,
// AND THEY ARE DIFFERENT COLUMNS BECAUSE THEY ARE DIFFERENT FACTS.
//
// tenant_id is left to the column default (app_current_tenant()) for exactly the
// reason orders.tenant_id is written from current_setting: the row belongs to
// the OMS's own isolation domain, alongside the order it announces.
// envelope_tenant_id is written explicitly from the record, because that is what
// the FACT will be published under.
//
// This is not a distinction without a difference. On the deployment that exists
// today the two DIFFER on every real order — the shipped OMS runs
// OMS_TENANT="__system__" while the api-gateway stamps the caller's tenant on the
// command, which pkg/bus.RequireTenantScope documents as the accepted state of
// #223 until #97. Writing the envelope's tenant into tenant_id would put the row
// outside the connection's RLS scope and the WITH CHECK would refuse it: not a
// subtle bug, a total admission outage. See migrations/0006_outbox.sql.
func Enqueue(ctx context.Context, tx pgx.Tx, records ...Record) error {
	for _, r := range records {
		if r.TenantID == "" {
			// Belt for From's braces: a hand-built Record must not reach the
			// table without an envelope tenant. bus.Validate refuses an empty
			// one, so the record could never be published — it would sit at the
			// head of its key blocking every FACT behind it.
			return fmt.Errorf("%w: %s", ErrNoTenant, r.EventType)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO outbox (
				envelope_tenant_id, partition_key, subject, event_type, event_class,
				schema_version, domain, payload_schema_ref, event_time,
				correlation_id, causation_id, trace_context, payload
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
			r.TenantID, r.PartitionKey, r.Subject, r.EventType, int32(r.EventClass),
			int32(r.SchemaVersion), r.Domain, r.PayloadSchemaRef, r.EventTime.UTC(),
			r.CorrelationID, r.CausationID, r.TraceContext, r.Payload,
		); err != nil {
			return fmt.Errorf("outbox: enqueue %s for %s: %w", r.EventType, r.PartitionKey, err)
		}
	}
	return nil
}

// Postgres is the durable Queue over a service's outbox table.
//
// It reads through the SAME tenant-scoped pool the service's own store uses, and
// that is a decision, not a convenience — see the RLS argument on Relay.
type Postgres struct {
	pool *pgxpool.Pool
	// service namespaces the advisory lock. Relay's doc named this as the one
	// thing that changes when a second service adopts the outbox, and #410 is
	// that second service.
	//
	// WITHOUT IT, TWO SERVICES SHARING A DATABASE SERIALISE AGAINST EACH OTHER.
	// pg_advisory_lock keys are cluster-wide integers, not per-table: a namespace
	// of "<tenant>/outbox" would make the OMS's drain of order-id "X" block
	// datamaster's drain of exception-id "X" whenever the two collide in
	// hashtext. Nothing would break — the relays would just take turns for no
	// reason, intermittently, in a way no test reproduces.
	service string
}

// NewPostgres returns a Queue over an existing pool. The caller owns the pool.
//
// service is REQUIRED and is not defaulted: a default would put two services in
// one lock namespace, which is the failure described above and is invisible.
func NewPostgres(pool *pgxpool.Pool, service string) *Postgres {
	if strings.TrimSpace(service) == "" {
		panic("outbox: NewPostgres requires a service name for the advisory-lock namespace")
	}
	return &Postgres{pool: pool, service: service}
}

// lockNamespace is "<tenant>/<service>/outbox", built in ONE place so the three
// advisory-lock calls cannot drift — a lock taken in one namespace and released
// in another leaks the lock for the life of the connection.
func (p *Postgres) lockNamespace() string { return "/" + p.service + "/outbox" }

var _ Queue = (*Postgres)(nil)

// pendingColumns is the projection every read shares, so a column added to one
// read and forgotten in the other cannot make two relays publish different
// envelopes for one row.
// envelope_tenant_id, NOT tenant_id: the relay publishes under the tenant the
// FACT belongs to, which on today's deployment is not the tenant the row is
// stored under. See Enqueue.
//
// last_error IS ON THE PROJECTION (#817). It was written by MarkFailed, cleared
// by MarkPublished and selected by nothing, so the one field that says why the
// head of a key will not publish could only be reached by opening psql against
// production during the outage. Every column here must have a Scan target in
// Pending below, in the same order; test/arch/outbox_projection_test.go proves
// the counts match, because a mismatch is a pgx error that only appears on a
// box with TEST_POSTGRES_URL set — which a developer's is not.
const pendingColumns = `id, attempts, envelope_tenant_id, partition_key, subject, event_type,
	event_class, schema_version, domain, payload_schema_ref, event_time,
	correlation_id, causation_id, trace_context, payload, last_error`

// PendingKeys returns the partition keys with work waiting, ordered by their
// OLDEST pending record.
//
// Oldest-first, not newest-first, because a key whose head cannot publish is the
// one an operator needs to see and the one whose backlog grows. Newest-first
// would drain the healthy keys forever and let the stuck one age out of every
// batch.
func (p *Postgres) PendingKeys(ctx context.Context, limit int) ([]string, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := p.pool.Query(ctx, `
		SELECT partition_key
		FROM outbox
		WHERE published_at IS NULL
		GROUP BY partition_key
		ORDER BY MIN(id)
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("outbox: list pending keys: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, fmt.Errorf("outbox: scan pending key: %w", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// LockKey takes the cluster-wide drain lock for one partition key.
//
// # Why an advisory lock, and why TRY
//
// oms-deploy.yaml runs replicas: 2, so two relays drain one table. Ordering per
// key survives that only if a key belongs to exactly one drainer at a time. A
// row lock cannot express that — see the SKIP LOCKED argument on Queue — and a
// lease row would need a heartbeat, a reaper and an answer to "what if the
// holder is merely slow". A session advisory lock dies with the connection, so a
// pod that is OOM-killed mid-drain strands nothing.
//
// TRY, not wait: the loser has other keys to drain and no reason to queue behind
// this one. Same posture, and the same connection-pinning care, as
// datamaster's PostgresCycleLock.
//
// # Why the tenant is in the key
//
// An advisory lock is CLUSTER-GLOBAL and knows nothing about RLS. Two tenants
// with an order of the same id — and order ids are caller-supplied — would
// otherwise serialize against each other's drains for no reason. This is the
// same reasoning, and the same two-int form, as position.lockInstrument: the
// two-int lock space is distinct from the int8 space internal/migrate and
// linkstore use, so these can never collide with a migration runner's lock.
// app_current_tenant() RAISES on an unscoped session (MT-01e), so a connection
// that never set the GUC fails HERE rather than taking a lock silently shared by
// every tenant.
//
// A hash collision between two different keys over-locks — two unrelated orders
// serialize — which costs throughput and cannot cost correctness.
//
// # Why there are two modes
//
// See Queue.LockKey. The background pass TRIES and moves on; the inline flush a
// handler runs after committing a FACT WAITS, because it is about to publish the
// next FACT for the same order directly and must not do that while an earlier
// one is still in the table.
func (p *Postgres) LockKey(ctx context.Context, key string, wait bool) (func(), bool, error) {
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("outbox: acquire connection for the %s drain lock: %w", key, err)
	}
	if wait {
		// pg_advisory_lock has no boolean result; it returns void and blocks.
		// Cancellation is the caller's ctx — pgx cancels the query, so a
		// shutdown does not strand a handler inside the lock wait.
		if _, err := conn.Exec(ctx,
			`SELECT pg_advisory_lock(hashtext(app_current_tenant() || $2), hashtext($1))`,
			key, p.lockNamespace()); err != nil {
			conn.Release()
			return nil, false, fmt.Errorf("outbox: wait for the %s drain lock: %w", key, err)
		}
		return func() { p.unlock(conn, key) }, true, nil
	}
	var won bool
	if err := conn.QueryRow(ctx,
		`SELECT pg_try_advisory_lock(hashtext(app_current_tenant() || $2), hashtext($1))`,
		key, p.lockNamespace()).Scan(&won); err != nil {
		conn.Release()
		return nil, false, fmt.Errorf("outbox: try the %s drain lock: %w", key, err)
	}
	if !won {
		conn.Release()
		return nil, false, nil
	}
	return func() { p.unlock(conn, key) }, true, nil
}

// unlock releases the drain lock and returns the connection to the pool.
//
// If the unlock fails the connection is DESTROYED rather than returned. The
// session would still hold the lock, and handing it back to the pool would
// strand that key for the pool's lifetime — after which no relay, in this pod or
// any other, could ever drain it again and its FACTs would silently stop being
// published. Killing the session makes Postgres release the lock for us. Same
// stance, and the same reason, as datamaster's cycle lock.
func (p *Postgres) unlock(conn *pgxpool.Conn, key string) {
	// Not the caller's ctx: a shutdown has usually cancelled it by the time this
	// unwinds, and the unlock still has to happen.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := conn.Exec(ctx,
		`SELECT pg_advisory_unlock(hashtext(app_current_tenant() || $2), hashtext($1))`,
		key, p.lockNamespace()); err != nil {
		if c := conn.Hijack(); c != nil {
			_ = c.Close(ctx)
		}
		return
	}
	conn.Release()
}

// Pending returns one key's unpublished records in ID order — the order they
// were committed in, which for a single key is the order the lifecycle happened
// in (see Queue).
func (p *Postgres) Pending(ctx context.Context, key string, limit int) ([]Pending, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := p.pool.Query(ctx, `
		SELECT `+pendingColumns+`
		FROM outbox
		WHERE published_at IS NULL AND partition_key = $1
		ORDER BY id
		LIMIT $2`, key, limit)
	if err != nil {
		return nil, fmt.Errorf("outbox: read pending for %s: %w", key, err)
	}
	defer rows.Close()
	var out []Pending
	for rows.Next() {
		var (
			p         Pending
			class     int32
			schemaVer int32
			eventTime time.Time
		)
		if err := rows.Scan(&p.ID, &p.Attempts, &p.Record.TenantID, &p.Record.PartitionKey,
			&p.Record.Subject, &p.Record.EventType, &class, &schemaVer, &p.Record.Domain,
			&p.Record.PayloadSchemaRef, &eventTime, &p.Record.CorrelationID,
			&p.Record.CausationID, &p.Record.TraceContext, &p.Record.Payload,
			&p.LastError); err != nil {
			return nil, fmt.Errorf("outbox: scan pending for %s: %w", key, err)
		}
		p.Record.EventClass = envelopepb.EventClass(class)
		p.Record.SchemaVersion = uint32(schemaVer)
		p.Record.EventTime = eventTime.UTC()
		out = append(out, p)
	}
	return out, rows.Err()
}

// MarkPublished stamps published_at. A row whose publish succeeded and whose
// mark then failed is retried on the next pass and published twice — the
// at-least-once direction, and the one every consumer of these FACTs already
// tolerates (tv-sync appends an identical revision; the position book claims
// each fill_id before folding). The other direction — marking before publishing
// — would lose the FACT outright, which is the failure this whole package
// exists to remove.
func (p *Postgres) MarkPublished(ctx context.Context, id int64) error {
	tag, err := p.pool.Exec(ctx,
		`UPDATE outbox SET published_at = now(), last_error = '' WHERE id = $1 AND published_at IS NULL`, id)
	if err != nil {
		return fmt.Errorf("outbox: mark %d published: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		// Another relay published it between our read and this update, or RLS
		// hid it. Neither is an error for this caller: the record is out.
		return nil
	}
	return nil
}

// MarkFailed records an attempt that did not reach the broker.
//
// It deliberately does NOT set published_at and does NOT move the record aside.
// A dead-letter for the outbox would be a queue nobody drains holding FACTs the
// estate is missing, and skipping the record would publish the FACTs behind it
// out of order. The record stays at the head of its key, the attempt count
// climbs, and the age gauge makes it visible.
//
// THE CAUSE IS WHAT THE AGE GAUGE CANNOT SAY. It comes back on the next
// Pending as LastError and the relay logs it beside the live error, so the
// replica that inherits a stalled key reports the refusal that started it rather
// than only the symptom it just saw (#817).
func (p *Postgres) MarkFailed(ctx context.Context, id int64, cause error) error {
	msg := boundedCause(cause)
	if _, err := p.pool.Exec(ctx,
		`UPDATE outbox SET attempts = attempts + 1, last_error = $2 WHERE id = $1 AND published_at IS NULL`,
		id, msg); err != nil {
		return fmt.Errorf("outbox: mark %d failed: %w", id, err)
	}
	return nil
}

// OldestPendingAge is the relay's liveness signal. See Queue.
//
// MIN over no rows returns ONE ROW HOLDING NULL, not zero rows, so the scan
// target is a POINTER: a nil there is an empty queue, which is the healthy
// answer and must not read as a fault. Scanning into a bare time.Time would
// error on every healthy pass and the gauge would go stale while the log filled
// with a failure that was not one.
func (p *Postgres) OldestPendingAge(ctx context.Context, now time.Time) (time.Duration, bool, error) {
	var oldest *time.Time
	err := p.pool.QueryRow(ctx,
		`SELECT MIN(enqueued_at) FROM outbox WHERE published_at IS NULL`).Scan(&oldest)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("outbox: oldest pending: %w", err)
	}
	if oldest == nil {
		return 0, false, nil
	}
	age := now.UTC().Sub(oldest.UTC())
	if age < 0 {
		age = 0
	}
	return age, true, nil
}
