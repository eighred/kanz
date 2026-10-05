// Package auditdelivery projects committed authority evidence without a
// sequence watermark: a lower allocated sequence may commit after a higher one.
package auditdelivery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	observationpb "github.com/eighred/kanz/kanz-schemas-go/observation/v1"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/encoding/protojson"
)

type Source string

const (
	// Subject has its own non-expiring, reject-new-on-capacity stream.
	Subject                 = "audit.authority.decision"
	Identity         Source = "identity_access_audit"
	Session          Source = "bff_session_audit"
	operationTimeout        = 5 * time.Second
)

type Publisher interface {
	Publish(context.Context, bus.Event) error
}

// Worker owns no pool or publisher. A transaction locks one pending record
// through publish/ack/delete; a crash rolls back the deletion, retaining its ID.
type Worker struct {
	pool                             *pgxpool.Pool
	publisher                        Publisher
	source                           Source
	connected                        func() bool
	health, pending, oldest, scanned prometheus.Gauge
	delivered, failures              prometheus.Counter
}

func New(ctx context.Context, pool *pgxpool.Pool, publisher Publisher, source Source, registry prometheus.Registerer) (*Worker, error) {
	if pool == nil || publisher == nil || (source != Identity && source != Session) {
		return nil, errors.New("invalid audit delivery dependencies")
	}
	var bypass bool
	if err := pool.QueryRow(ctx, `SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&bypass); err != nil || bypass {
		return nil, errors.New("audit delivery requires a restricted database role")
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_class WHERE oid IN (to_regclass($1),to_regclass($2)) AND relrowsecurity AND relforcerowsecurity`, string(source), string(source)+"_pending").Scan(&count); err != nil || count != 2 {
		return nil, errors.New("audit delivery requires migrated FORCE RLS journals and queues")
	}
	var queueReady bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass($2) IS NOT NULL AND EXISTS(SELECT 1 FROM pg_trigger WHERE tgrelid=to_regclass($1) AND tgname=$3 AND tgenabled IN ('O','A'))`, string(source), string(source)+"_tenants", string(source)+"_enqueue").Scan(&queueReady); err != nil || !queueReady {
		return nil, errors.New("audit delivery enqueue trigger or tenant directory missing")
	}
	if pool.Config().MaxConns < 2 {
		return nil, errors.New("audit delivery requires at least two pool connections")
	}
	w := &Worker{pool: pool, publisher: publisher, source: source, connected: func() bool { return true },
		health:    prometheus.NewGauge(prometheus.GaugeOpts{Name: "kanz_authority_audit_scan_healthy", Help: "One when the last complete scan and delivery attempts succeeded; zero before first scan or on failure."}),
		pending:   prometheus.NewGauge(prometheus.GaugeOpts{Name: "kanz_authority_audit_pending", Help: "Pending records observed across the last complete tenant scan."}),
		oldest:    prometheus.NewGauge(prometheus.GaugeOpts{Name: "kanz_authority_audit_oldest_pending_seconds", Help: "Maximum pending record age at the last complete scan."}),
		scanned:   prometheus.NewGauge(prometheus.GaugeOpts{Name: "kanz_authority_audit_last_scan_timestamp_seconds", Help: "Time of last complete tenant scan; check freshness before trusting backlog."}),
		delivered: prometheus.NewCounter(prometheus.CounterOpts{Name: "kanz_authority_audit_delivered_total", Help: "Acknowledged records removed from pending queue."}),
		failures:  prometheus.NewCounter(prometheus.CounterOpts{Name: "kanz_authority_audit_failures_total", Help: "Failed delivery or discovery attempts; evidence remains pending."}),
	}

	if registry != nil {
		for _, c := range []prometheus.Collector{w.health, w.pending, w.oldest, w.scanned, w.delivered, w.failures} {
			if err := registry.Register(c); err != nil {
				return nil, err
			}
		}
	}
	return w, nil
}

// DeliverOne is bounded even if a caller supplies no deadline. It never reads
// another tenant's payload, including when its queue reference is malformed.
func (w *Worker) DeliverOne(ctx context.Context, tenant string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id',$1,true)`, tenant); err != nil {
		return false, err
	}
	var seq int64
	var id string
	var occurred time.Time
	var payload []byte
	err = tx.QueryRow(ctx, fmt.Sprintf(`SELECT p.sequence,p.event_id::text,j.occurred_at,CASE WHEN octet_length(j.decision::text)<=1048576 THEN j.decision ELSE NULL END FROM %s_pending p JOIN %s j ON j.sequence=p.sequence AND j.tenant_id=p.tenant_id ORDER BY p.sequence LIMIT 1 FOR UPDATE OF p SKIP LOCKED`, w.source, w.source)).Scan(&seq, &id, &occurred, &payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var decision observationpb.DecisionLog
	if err = protojson.Unmarshal(payload, &decision); err != nil {
		return false, errors.New("invalid persisted audit decision")
	}
	if decision.GetAttributes()["principal.tenant"] != tenant || decision.GetAttributes()["principal.subject"] == "" || (decision.Attributes["resource.tenant"] != "" && decision.Attributes["resource.tenant"] != tenant) {
		return false, errors.New("audit decision attribution does not match journal scope")
	}
	err = w.publisher.Publish(ctx, eventFor(id, tenant, occurred, &decision))
	if err != nil {
		return false, err
	}
	if _, err = tx.Exec(ctx, fmt.Sprintf(`DELETE FROM %s_pending WHERE sequence=$1`, w.source), seq); err != nil {
		return false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	w.delivered.Inc()
	return true, nil
}

func (w *Worker) backlog(ctx context.Context, tenant string) (int64, float64, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id',$1,true)`, tenant); err != nil {
		return 0, 0, err
	}
	var count int64
	var age float64
	err = tx.QueryRow(ctx, fmt.Sprintf(`SELECT count(*),COALESCE(GREATEST(0,extract(epoch FROM clock_timestamp()-min(j.occurred_at))),0)::double precision FROM %s_pending p JOIN %s j ON j.sequence=p.sequence AND j.tenant_id=p.tenant_id`, w.source, w.source)).Scan(&count, &age)
	return count, age, err
}

// Run registers LISTEN before scanning, so committed wakeups cannot fall between
// the initial scan and subscription. Timed recovery handles lost notifications
// and broker outages. Tenant discovery and per-tenant work have bounded memory.
func (w *Worker) Run(ctx context.Context, logger *slog.Logger) error {
	conn, err := w.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err = conn.Exec(ctx, "LISTEN "+string(w.source)); err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if _, e := conn.Exec(cleanup, "UNLISTEN "+string(w.source)); e != nil {
			_ = conn.Conn().Close(cleanup)
		}
	}()
	for ctx.Err() == nil {
		cursor := ""
		var pending int64
		var oldest float64
		healthy := true
		scanOK := true
		busy := false
		for ctx.Err() == nil {
			var tenant string
			queryCtx, cancel := context.WithTimeout(ctx, operationTimeout)
			err = w.pool.QueryRow(queryCtx, fmt.Sprintf(`SELECT tenant_id FROM %s_tenants WHERE tenant_id>$1 ORDER BY tenant_id LIMIT 1`, w.source), cursor).Scan(&tenant)
			cancel()
			if errors.Is(err, pgx.ErrNoRows) {
				break
			}
			if err != nil {
				healthy = false
				scanOK = false
				w.failures.Inc()
				break
			}
			cursor = tenant
			for n := 0; n < 16; n++ {
				var sent bool
				sent, err = w.DeliverOne(ctx, tenant)
				if err != nil {
					healthy = false
					w.failures.Inc()
					break
				}
				if !sent {
					break
				}
				busy = true
			}
			count, age, e := w.backlog(ctx, tenant)
			if e != nil {
				healthy = false
				scanOK = false
				w.failures.Inc()
			} else {
				pending += count
				oldest = max(oldest, age)
			}
		}
		if ctx.Err() != nil {
			return nil
		}
		if scanOK {
			w.pending.Set(float64(pending))
			w.oldest.Set(oldest)
			w.scanned.SetToCurrentTime()
		}
		healthy = healthy && w.connected()
		if healthy {
			w.health.Set(1)
		} else {
			w.health.Set(0)
			logger.Warn("durable authority audit delivery incomplete; pending evidence retained")
		}
		if healthy && busy {
			continue
		}
		wait, cancel := context.WithTimeout(ctx, 30*time.Second)
		_, err = conn.Conn().WaitForNotification(wait)
		cancel()
		if err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			w.health.Set(0)
			return errors.New("audit notification connection lost")
		}
	}
	return nil
}

func eventFor(id, tenant string, occurred time.Time, decision *observationpb.DecisionLog) bus.Event {
	return bus.Event{EventID: id, Subject: Subject, EventType: auth.AuthzDecisionEventType, EventClass: envelopepb.EventClass_EVENT_CLASS_OBSERVATION, SchemaVersion: 1, Domain: auth.AuthzDecisionDomain, EventTime: occurred, IngestionTime: occurred, PartitionKey: tenant + "/" + decision.Attributes["principal.subject"], PayloadSchemaRef: "observation.v1.DecisionLog:1", TenantID: tenant, Payload: decision}
}
