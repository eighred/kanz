package snapshot

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eighred/kanz/internal/lake/snapshotprofile"
	"github.com/eighred/kanz/internal/outbox"
	"github.com/eighred/kanz/internal/pg"
	"github.com/eighred/kanz/internal/version"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/eighred/kanz/services/lake-sink/internal/config"
	"github.com/eighred/kanz/services/lake-sink/internal/sink"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
)

// Run owns the optional snapshot resources until all callbacks and children
// have actually joined. The caller must join Run before closing the FileSink or
// the shared workload source. It never migrates/provisions PG or broker state.
func Run(ctx context.Context, cfg config.Snapshot, source transport.Source, landing *sink.FileSink, landingRoot string, logger *slog.Logger, metrics *bus.BusMetrics, readiness func(bool)) error {
	if source == nil || readiness == nil || logger == nil {
		return ErrInvalid
	}
	parent := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	readiness(false)
	defer readiness(false)
	var failed atomic.Bool
	var readinessMu sync.Mutex
	latch := func() {
		readinessMu.Lock()
		defer readinessMu.Unlock()
		failed.Store(true)
		readiness(false)
		// Readiness alone cannot revoke an established gateway HTTP/2 channel.
		// The finite owner stops admission/work; restart must independently prove
		// account/profile identity and durable outbox catch-up before serving.
		cancel()
	}
	startup, stopStartup := context.WithTimeout(ctx, 30*time.Second)
	defer stopStartup()
	policy, err := auth.LoadPolicyFile(cfg.PolicyFile)
	if err != nil {
		return ErrInvalid
	}
	pool, err := pg.NewTenantPool(startup, cfg.DatabaseURL, cfg.Tenant)
	if err != nil {
		return errors.New("snapshot database unavailable")
	}
	defer pool.Close()
	var unsafe bool
	var forced int
	if pool.QueryRow(startup, `SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&unsafe) != nil || unsafe ||
		pool.QueryRow(startup, `SELECT count(*) FROM pg_class WHERE oid IN ('lake_snapshots'::regclass,'outbox'::regclass) AND relrowsecurity AND relforcerowsecurity`).Scan(&forced) != nil || forced != 2 {
		return errors.New("snapshot requires scoped role and FORCE RLS owner tables")
	}
	store, err := NewStore(pool, cfg.Tenant, auth.NewPolicyAuthorizer(policy))
	if err != nil {
		return err
	}
	worker, err := NewWorker(cfg.WorkerPath, cfg.WorkerSHA256, func() { latch(); cancel() })
	if err != nil {
		return err
	}
	// After stopping admission/callbacks, retain ownership until any exceptional
	// child/pipe reaper releases the slot. Five seconds is an observation grace,
	// not permission to close a lease/pool underneath a surviving process.
	defer func() { worker.failed.Store(true); worker.slot <- struct{}{} }()
	svid, err := source.GetX509SVID()
	if err != nil || svid == nil || svid.ID.Path() != "/ns/kanz-services/sa/lake-sink" || svid.ID.TrustDomain() != cfg.GatewayID.TrustDomain() {
		return ErrDenied
	}
	brokerID, err := spiffeid.FromPath(cfg.GatewayID.TrustDomain(), "/ns/kanz-messaging/sa/nats")
	if err != nil {
		return ErrInvalid
	}
	tuning := bus.RetainedWorkTuning()
	client, err := bus.DialNATS(startup, bus.NATSConfig{URL: cfg.NATSURL, Name: Service + "-snapshots", TLSConfig: transport.ClientTLSConfig(source, transport.AuthorizeServices(brokerID)), ConsumerTuning: &tuning, DisableReconnect: true, Metrics: metrics, Logger: logger, OnDisconnect: func(error) { latch(); cancel() }})
	if err != nil {
		return errors.New("snapshot broker unavailable")
	}
	defer func() { _ = client.Close() }()
	if err := client.RequireAccountIdentity(startup, cfg.NATSAccount, svid.ID.String()); err != nil {
		return err
	}
	if err := client.RequireDurableRetention(startup, snapshotprofile.Stream()); err != nil {
		return err
	}
	if err := client.RequireRegisteredDefaultDurable(startup, snapshotprofile.Name, "lake.snapshot.>", "archiver"); err != nil {
		return err
	}
	if err := client.RequireCanonicalParking(startup, SubjectRequested, int64(snapshotprofile.Stream().MaxMsgSize)); err != nil {
		return err
	}
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: Service, ProducerVersion: version.String(), Tenant: cfg.Tenant})
	if err != nil {
		return err
	}
	queue := outbox.NewPostgres(pool, outbox.ServiceLakeSink)
	relay, err := outbox.NewRelay(queue, &latchedPublisher{publisher: producer, fail: latch}, logger)
	if err != nil {
		return err
	}
	// A restart clears no failure merely by connecting. Before admitting RPCs,
	// flush every bounded known key and read back that its durable mark is done.
	keys, err := queue.PendingKeys(startup, 17)
	if err != nil || len(keys) > 16 {
		return errors.New("snapshot owned backlog unavailable")
	}
	for _, key := range keys {
		if _, err := relay.Flush(startup, key); err != nil {
			return errors.New("snapshot owned backlog unconfirmed")
		}
	}
	keys, err = queue.PendingKeys(startup, 1)
	if err != nil || len(keys) != 0 || failed.Load() {
		return errors.New("snapshot owned backlog unconfirmed")
	}
	server, err := NewServer(ctx, store, worker, landing, landingRoot, cfg.OutputDir)
	if err != nil {
		return err
	}
	grpcServer, err := NewGRPCServer(source, cfg.GatewayID, server)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return errors.New("snapshot listener unavailable")
	}
	defer func() { _ = listener.Close() }()
	consumer, err := bus.NewConsumer(&latchedSubscriber{NATSClient: client, fail: latch}, bus.WithRetainedRetries(SubjectRequested), bus.WithBusMetrics(metrics), bus.WithDLQ(client))
	if err != nil {
		return err
	}
	var wg sync.WaitGroup
	var first error
	var once sync.Once
	run := func(f func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := f(); err != nil && ctx.Err() == nil {
				once.Do(func() { first = errors.New("snapshot owner stopped"); latch(); cancel() })
			}
		}()
	}
	initialReady := make(chan struct{})
	var initialOnce sync.Once
	run(func() error { return relay.Run(ctx) })
	run(func() error {
		return consumer.SubscribeReady(ctx, SubjectRequested, snapshotprofile.Group, server.HandleRequested, func() {
			initialOnce.Do(func() { close(initialReady) })
		})
	})
	// Binding a listener is not admission. Do not start the authenticated RPC
	// server before the native durable/profile and initial replay have passed.
	select {
	case <-initialReady:
		readinessMu.Lock()
		if !failed.Load() && ctx.Err() == nil {
			run(func() error { return grpcServer.Serve(listener) })
			readiness(true)
		}
		readinessMu.Unlock()
	case <-ctx.Done():
	}
	<-ctx.Done()
	latch()
	grpcServer.Stop()
	wg.Wait()
	if first == nil && parent.Err() == nil {
		return errors.New("snapshot owner lost required dependency")
	}
	return first
}

type latchedPublisher struct {
	publisher outbox.Publisher
	fail      func()
}

func (p *latchedPublisher) Publish(ctx context.Context, event bus.Event) error {
	err := p.publisher.Publish(ctx, event)
	if err != nil && ctx.Err() == nil {
		p.fail()
	}
	return err
}

type latchedSubscriber struct {
	*bus.NATSClient
	fail func()
}

func (s *latchedSubscriber) guarded(h bus.Handler) bus.Handler {
	return func(ctx context.Context, msg bus.Message) error {
		err := h(ctx, msg)
		if err != nil && ctx.Err() == nil && !errors.Is(err, ErrBusy) {
			s.fail()
		}
		return err
	}
}
func (s *latchedSubscriber) Subscribe(ctx context.Context, subject, group string, h bus.Handler) error {
	return s.NATSClient.Subscribe(ctx, subject, group, s.guarded(h))
}
func (s *latchedSubscriber) SubscribeReady(ctx context.Context, subject, group string, h bus.Handler, ready func()) error {
	return s.NATSClient.SubscribeReady(ctx, subject, group, s.guarded(h), ready)
}
