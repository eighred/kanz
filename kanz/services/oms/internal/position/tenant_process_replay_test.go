package position

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/url"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	ns "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/platform/subject"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
)

const (
	restartTestName = "TestTenantPositionReplayAfterProcessExit"
	restartGroup    = "position-restart"
	restartAckWait  = 3 * time.Second
	exitAfterFold   = 73
)

type restartTenant struct {
	tenant          string
	qty, price      int64
	eventID         string
	url             string
	pool            *pgxpool.Pool
	durableCreated  time.Time
	order, position jetstream.Stream
	worker          *positionProcess
	firstPID        int
}

// Each real JetStream account has the same order subject and durable name.
// Both OMS projection processes use the same PostgreSQL schema and identical
// portfolio, instrument and execution keys, but own different quantities.
func TestTenantPositionReplayAfterProcessExit(t *testing.T) {
	if tenant := os.Getenv("KANZ_TEST_POSITION_TENANT"); tenant != "" {
		runPositionChild(t, tenant)
		return
	}
	cases := []restartTenant{
		{tenant: "fold-alpha", qty: 2, price: 13},
		{tenant: "fold-beta", qty: 7, price: 19},
	}
	for i := range cases {
		c := &cases[i]
		c.pool = newPool(t, c.tenant)
		var privileged bool
		if err := c.pool.QueryRow(t.Context(), "SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname=current_user").Scan(&privileged); err != nil || privileged {
			t.Fatalf("restricted PostgreSQL role required: privileged=%t err=%v", privileged, err)
		}
	}
	freshSchema(t, cases[0].pool)
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	endpoints := restartAccountEndpoints(t, []string{cases[0].tenant, cases[1].tenant})
	for i := range cases {
		c := &cases[i]
		c.url = endpoints[c.tenant]
		c.publishSimulatedFill(t, ctx)
		c.worker = startPositionProcess(t, ctx, c.tenant, c.url, "exit")
		c.firstPID = c.worker.cmd.Process.Pid
	}
	for i := range cases {
		c := &cases[i]
		var exit *exec.ExitError
		if err := c.worker.wait(); !errors.As(err, &exit) || exit.ExitCode() != exitAfterFold {
			t.Fatalf("%s first process did not exit at the committed pre-ACK boundary: %v", c.tenant, err)
		}
	}
	for i := range cases {
		cases[i].verifyReceipt(t, ctx, restartReceipt{deliveries: 1, pending: 1})
	}
	restartAt := time.Now()
	for i := range cases {
		c := &cases[i]
		c.worker = startPositionProcess(t, ctx, c.tenant, c.url, "ack")
		if c.worker.cmd.Process.Pid == c.firstPID {
			t.Fatal("replacement PID must differ from the terminated worker")
		}
	}
	for i := range cases {
		cases[i].verifyReceipt(t, ctx, restartReceipt{deliveries: 2, ackFloor: 2, ackedStream: 1})
	}
	t.Logf("two tenant replacements to confirmed durable ACKs=%s fixture_AckWait=%s", time.Since(restartAt), restartAckWait)
	for i := range cases {
		c := &cases[i]
		if err := c.worker.stdin.Close(); err != nil {
			t.Fatal(err)
		}
		if err := c.worker.wait(); err != nil {
			t.Fatalf("%s replacement exit: %v", c.tenant, err)
		}
	}
}

func (c *restartTenant) publishSimulatedFill(t *testing.T, ctx context.Context) {
	t.Helper()
	nc, err := nats.Connect(c.url)
	if err != nil {
		t.Fatalf("observer could not connect to fixture account %s", c.tenant)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	c.order, err = js.CreateStream(ctx, jetstream.StreamConfig{Name: "ORDER", Subjects: []string{orderEventFilled}})
	if err != nil {
		t.Fatal(err)
	}
	c.position, err = js.CreateStream(ctx, jetstream.StreamConfig{Name: positionStream, Subjects: []string{"risk.position.>"}, MaxMsgsPerSubject: 1})
	if err != nil {
		t.Fatal(err)
	}
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: c.url, Name: "simulated-fill"})
	if err != nil {
		t.Fatalf("producer could not connect to fixture account %s", c.tenant)
	}
	t.Cleanup(func() { _ = client.Close() })
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "oms", ProducerVersion: "test", Tenant: c.tenant})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	f := buy("same-fill", "ASSET", fmt.Sprint(c.qty), fmt.Sprint(c.price), at)
	f.OrderId = "same-order"
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	c.eventID = id.String()
	fact := &orderpb.OrderFilled{
		OrderId: f.OrderId, Fill: f,
		State: &orderpb.OrderState{OrderId: f.OrderId, PortfolioId: "fund", InstrumentId: f.InstrumentId,
			Venue: f.Venue, Status: orderpb.OrderStatus_ORDER_STATUS_FILLED},
	}
	if err := producer.Publish(ctx, bus.Event{
		EventID: c.eventID, Subject: orderEventFilled, EventType: orderEventFilled,
		EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, Domain: "order", SchemaVersion: 1,
		PartitionKey: f.OrderId, EventTime: at, Payload: fact,
	}); err != nil {
		t.Fatal(err)
	}
}

type restartReceipt struct {
	deliveries, ackFloor, ackedStream uint64
	pending                           int
}

func (c *restartTenant) verifyReceipt(t *testing.T, ctx context.Context, want restartReceipt) {
	t.Helper()
	var names []string
	durables := c.order.ConsumerNames(ctx)
	for name := range durables.Name() {
		names = append(names, name)
	}
	if err := durables.Err(); err != nil || len(names) != 1 {
		t.Fatalf("durables=%v err=%v", names, err)
	}
	consumer, err := c.order.Consumer(ctx, names[0])
	if err != nil {
		t.Fatal(err)
	}
	for {
		receipt, err := consumer.Info(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if receipt.AckFloor.Consumer >= want.ackFloor {
			if c.durableCreated.IsZero() {
				c.durableCreated = receipt.Created
			} else if !receipt.Created.Equal(c.durableCreated) {
				t.Fatalf("%s durable was replaced across process restart", c.tenant)
			}
			if receipt.Delivered.Consumer != want.deliveries || receipt.Delivered.Stream != 1 ||
				receipt.AckFloor.Consumer != want.ackFloor || receipt.AckFloor.Stream != want.ackedStream ||
				receipt.NumPending != 0 || receipt.NumAckPending != want.pending || receipt.Config.AckWait != restartAckWait {
				t.Fatalf("%s broker receipt: got %+v, want %+v", c.tenant, receipt, want)
			}
			facts := 2 * int(want.deliveries)
			c.verifyPosition(t, ctx, facts)
			t.Logf("tenant=%s pid=%d deliveries=%d ack_floor=%d unacknowledged=%d position_FACTs=%d",
				c.tenant, c.worker.cmd.Process.Pid, receipt.Delivered.Consumer, receipt.AckFloor.Consumer, receipt.NumAckPending, facts)
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (c *restartTenant) verifyPosition(t *testing.T, ctx context.Context, facts int) {
	t.Helper()
	var claims, applied, published, total int
	err := c.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM position_fills),
		(SELECT count(*) FROM position_fills WHERE raw_fill_id='same-fill'),
		count(*) FILTER (WHERE envelope_tenant_id=$1 AND causation_id=$2 AND published_at IS NOT NULL), count(*) FROM outbox`, c.tenant, c.eventID).Scan(&claims, &applied, &published, &total)
	if err != nil || claims != 1 || applied != 1 || published != facts || total != facts {
		t.Fatalf("%s claims=%d same-fill=%d published=%d total=%d err=%v", c.tenant, claims, applied, published, total, err)
	}
	book, err := NewPostgres(c.pool, "USD").Snapshot(ctx, "fund", time.Now())
	if err != nil || len(book.GetPositions()) != 1 {
		t.Fatalf("%s durable book: %v err=%v", c.tenant, book, err)
	}
	holding := book.Positions[0]
	if holding.GetInstrumentId() != "ASSET" || dec.FromProto(holding.Quantity).Cmp(big.NewRat(c.qty, 1)) != 0 || dec.FromProto(holding.AveragePrice).Cmp(big.NewRat(c.price, 1)) != 0 {
		t.Fatalf("%s durable holding: %v", c.tenant, holding)
	}
	info, err := c.position.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.State.LastSeq != uint64(facts) || info.State.Msgs != 2 {
		t.Fatalf("%s stream state: %+v", c.tenant, info.State)
	}
	announcements := []struct{ subject, eventType, venue string }{
		{subject.PositionFor(c.tenant, "fund", "ASSET"), subject.PositionChanged, ""},
		{subject.VenuePositionFor(c.tenant, "fund", "XBIN", "ASSET"), subject.VenuePositionChanged, "XBIN"},
	}
	for index, want := range announcements {
		raw, err := c.position.GetLastMsgForSubject(ctx, want.subject)
		if err != nil {
			t.Fatal(err)
		}
		env, payload, err := bus.Unframe(raw.Data)
		if err != nil {
			t.Fatal(err)
		}
		if err := bus.Validate(env); err != nil {
			t.Fatal(err)
		}
		var state domainpb.PositionState
		if err := proto.Unmarshal(payload, &state); err != nil {
			t.Fatal(err)
		}
		if raw.Sequence != uint64(facts-1+index) || env.EventType != want.eventType || state.Venue != want.venue ||
			env.TenantId != c.tenant || env.CausationId != c.eventID || env.EventClass != envelopepb.EventClass_EVENT_CLASS_FACT ||
			state.PortfolioId != "fund" || state.InstrumentId != "ASSET" || dec.FromProto(state.Quantity).Cmp(big.NewRat(c.qty, 1)) != 0 || dec.FromProto(state.AveragePrice).Cmp(big.NewRat(c.price, 1)) != 0 {
			t.Fatalf("%s incorrect retained position sequence=%d tenant=%s cause=%s state=%v", want.subject, raw.Sequence, env.TenantId, env.CausationId, &state)
		}
	}
}

// Each login is generated for this loopback server and exists only in memory.
func restartAccountEndpoints(t *testing.T, tenants []string) map[string]string {
	t.Helper()
	opts := &ns.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir(), NoSigs: true}
	passwords := map[string]string{}
	for _, tenant := range tenants {
		account := ns.NewAccount(tenant)
		passwords[tenant] = uuid.NewString()
		opts.Accounts = append(opts.Accounts, account)
		opts.Users = append(opts.Users, &ns.User{Username: tenant, Password: passwords[tenant], Account: account})
	}
	srv, err := ns.NewServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	srv.Start()
	t.Cleanup(func() { srv.Shutdown(); srv.WaitForShutdown() })
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("tenant JetStream did not start")
	}
	out := map[string]string{}
	for _, tenant := range tenants {
		account, err := srv.LookupAccount(tenant)
		if err != nil {
			t.Fatal(err)
		}
		if err := account.EnableJetStream(nil, nil); err != nil {
			t.Fatal(err)
		}
		endpoint, err := url.Parse(srv.ClientURL())
		if err != nil {
			t.Fatal(err)
		}
		endpoint.User = url.UserPassword(tenant, passwords[tenant])
		out[tenant] = endpoint.String()
	}
	return out
}

type positionProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	waited bool
}

func (p *positionProcess) wait() error { p.waited = true; return p.cmd.Wait() }

func startPositionProcess(t *testing.T, ctx context.Context, tenant, endpoint, phase string) *positionProcess {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, executable, "-test.run=^"+restartTestName+"$", "-test.count=1")
	cmd.Env = []string{"KANZ_TEST_POSITION_TENANT=" + tenant, "KANZ_TEST_POSITION_URL=" + endpoint, "KANZ_TEST_POSITION_PHASE=" + phase}
	for _, key := range []string{"TEST_POSTGRES_URL", "SystemRoot", "TEMP", "TMP", "TMPDIR", "GORACE"} {
		if value, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	p := &positionProcess{cmd: cmd}
	p.stdin, err = cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !p.waited {
			_ = p.stdin.Close()
			_ = cmd.Process.Kill()
			_ = p.wait()
		}
	})
	return p
}

func runPositionChild(t *testing.T, tenant string) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _, _ = io.Copy(io.Discard, os.Stdin); cancel() }()
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: os.Getenv("KANZ_TEST_POSITION_URL"), Name: "position-worker", ConsumerTuning: &bus.ConsumerTuning{AckWait: restartAckWait, MaxDeliver: 4, MaxAckPending: 1}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "oms", ProducerVersion: "test", Tenant: tenant})
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewProjector(NewPostgres(newPool(t, tenant), "USD"), producer, tenant)
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := bus.NewConsumer(client, bus.WithDLQ(client))
	if err != nil {
		t.Fatal(err)
	}
	handler := p.Handle
	switch os.Getenv("KANZ_TEST_POSITION_PHASE") {
	case "exit":
		// The real consumer acknowledges only after the handler returns.
		handler = func(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
			if err := p.Handle(ctx, env, payload); err != nil {
				return err
			}
			os.Exit(exitAfterFold)
			return nil
		}
	case "ack":
		// Normal dispatch and acknowledgement in the replacement process.
	default:
		t.Fatal("unexpected position worker phase")
	}
	err = consumer.Subscribe(ctx, orderEventFilled, restartGroup, handler)
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
