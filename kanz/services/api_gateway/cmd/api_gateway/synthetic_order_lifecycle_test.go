package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"testing"
	"time"

	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/proto"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/outbox"
	"github.com/eighred/kanz/internal/pg"
	"github.com/eighred/kanz/internal/testidentity"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/oms/tools/syntheticvenue"
)

type nativeSubmit struct {
	Version   int    `json:"version"`
	Op        string `json:"op"`
	Tenant    string `json:"tenant"`
	Portfolio string `json:"portfolio"`
	Principal string `json:"principal"`
	Key       string `json:"idempotency_key,omitempty"`
	Command   []byte `json:"command_b64,omitempty"`
	OrderID   string `json:"order_id,omitempty"`
}

func callSyntheticOwner(ctx context.Context, endpoint string, request any) (syntheticvenue.Receipt, error) {
	var receipt syntheticvenue.Receipt
	payload, err := json.Marshal(request)
	if err != nil {
		return receipt, err
	}
	cmd := exec.CommandContext(ctx, os.Getenv("KANZ_SYNTHETIC_OMS_BIN"))
	// The subprocess gets exactly one database role. In particular the OMS
	// process must never inherit the test coordinator's control-writer DSN.
	cmd.Env = []string{"SYNTHETIC_OMS_DATABASE_URL=" + endpoint}
	cmd.Stdin = bytes.NewReader(payload)
	wire, err := cmd.Output()
	if err != nil {
		return receipt, fmt.Errorf("synthetic OMS process: %w", err)
	}
	if len(wire) > 1<<20 {
		return receipt, errors.New("oversize synthetic OMS receipt")
	}
	err = json.Unmarshal(wire, &receipt)
	return receipt, err
}

func requireNativeReceipt(t *testing.T, ctx context.Context, request nativeSubmit, want string) syntheticvenue.Receipt {
	t.Helper()
	got, err := callSyntheticOwner(ctx, os.Getenv("SYNTHETIC_OMS_RUNTIME_URL"), request)
	if err != nil || got.Code != want || got.OK != (want == "ACCEPTED" || want == "DUPLICATE" || want == "FOUND") {
		t.Fatalf("native %s: got %q, want %q (%v)", request.Op, got.Code, want, err)
	}
	return got
}

func isolatedOrderDSN(raw, role string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "postgres" || u.User == nil || u.User.Username() != role || u.Path != "/kanz_synthetic_oms" || u.RawQuery != "sslmode=disable" || u.Fragment != "" {
		return false
	}
	host := net.ParseIP(u.Hostname())
	return host != nil && host.IsLoopback()
}

func syntheticPool(t *testing.T, ctx context.Context, role, tenant string) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("SYNTHETIC_OMS_" + role + "_URL")
	expected := map[string]string{"SWITCH": "kanz_synthetic_handoff_probe", "RELAY": "kanz_synthetic_oms_relay", "CONTROL": "kanz_synthetic_oms_control", "RUNTIME": "kanz_synthetic_oms_runtime", "VENUE": "kanz_synthetic_venue"}[role]
	if expected == "" || !(isolatedOrderDSN(dsn, expected) || (role == "RUNTIME" && isolatedOrderDSN(dsn, expected+"_g2"))) {
		t.Fatal("refused a database outside the synthetic role and loopback scope")
	}
	pool, err := pg.NewTenantPool(ctx, dsn, tenant)
	if err != nil {
		t.Fatalf("open %s fixture pool: %T", role, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestSyntheticOrderCommandLifecycle(t *testing.T) {
	if os.Getenv("KANZ_SYNTHETIC_OMS_BIN") == "" {
		t.Skip("use python kanz/dev/synthetic_orders.py --native <Linux owner>")
	}
	exerciseSyntheticSession(t, func(ctx context.Context, s syntheticSession) {
		env := s.frame.GetEnvelope()
		var cmd orderpb.SubmitOrder
		if err := proto.Unmarshal(s.frame.Payload, &cmd); err != nil {
			t.Fatal(err)
		}
		tenant, portfolio, orderID := env.GetTenantId(), cmd.GetPortfolioId(), cmd.GetOrderId()
		request := nativeSubmit{Version: 1, Op: "submit", Tenant: tenant, Portfolio: portfolio, Principal: cmd.GetMetadata().GetIssuer(), Key: env.GetIdempotencyKey(), Command: s.frame.Payload}
		requireNativeReceipt(t, ctx, request, "CONTROL_DENIED")
		now := time.Now().UTC()
		controls := newSyntheticControls(now)
		rows, err := controls.evaluate(ctx, s.frame, now)
		if err != nil {
			t.Fatal(err)
		}
		pool := syntheticPool(t, ctx, "CONTROL", tenant)
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		digest := sha256.Sum256(s.frame.Payload)
		for _, r := range rows {
			traceContext := ""
			if r.kind == "entitlement" {
				traceContext = env.GetTraceContext()
			}
			_, err = tx.Exec(ctx, `INSERT INTO synthetic_oms_evidence (tenant_id,principal_id,portfolio_id,order_id,instrument_id,command_sha256,kind,decision,source_id,effective_at,observed_at,expires_at,trace_context) VALUES($1,$2,$3,$4,$5,$6,$7,'ALLOW',$8,$9,$10,$11,$12)`, tenant, request.Principal, portfolio, orderID, cmd.GetInstrumentId(), digest[:], r.kind, r.sourceID, r.effective, r.observed, r.expires, traceContext)
			if err != nil {
				t.Fatal(err)
			}
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		requireNativeReceipt(t, ctx, request, "COMMIT_UNCONFIRMED")
		runtimePool := syntheticPool(t, ctx, "RUNTIME", tenant)
		var orders, commands, factsAtRollback int
		if err = runtimePool.QueryRow(ctx, `SELECT (SELECT count(order_id) FROM orders),(SELECT count(order_id) FROM synthetic_oms_commands),(SELECT count(producer_service) FROM outbox)`).Scan(&orders, &commands, &factsAtRollback); err != nil {
			t.Fatal(err)
		}
		if orders != 0 || commands != 0 || factsAtRollback != 0 {
			t.Fatalf("failed commit left orders=%d commands=%d outbox=%d", orders, commands, factsAtRollback)
		}
		var original syntheticvenue.Receipt
		if !t.Run("fenced_restart", func(sub *testing.T) { original = exerciseFencedRestart(sub, ctx, s, request, runtimePool) }) {
			t.FailNow()
		}
		useWriterGeneration(t, "NEXT")
		duplicate := requireNativeReceipt(t, ctx, request, "DUPLICATE")
		if !bytes.Equal(original.State, duplicate.State) {
			t.Fatal("duplicate submission changed order state")
		}
		conflict := request
		changed := proto.Clone(&cmd).(*orderpb.SubmitOrder)
		changed.LimitPrice = &commonpb.Decimal{Coefficient: 9102, Exponent: -2}
		conflict.Command, err = proto.Marshal(changed)
		if err != nil {
			t.Fatal(err)
		}
		requireNativeReceipt(t, ctx, conflict, "CONFLICT")
		read := nativeSubmit{Version: 1, Op: "get", Tenant: tenant, Portfolio: portfolio, Principal: request.Principal, OrderID: orderID}
		foreign := read
		foreign.Tenant = "unrelated-tenant"
		requireNativeReceipt(t, ctx, foreign, "NOT_FOUND")

		facts, err := s.js.CreateStream(ctx, jetstream.StreamConfig{Name: "SYNTHETIC_FACTS", Subjects: []string{"order.>"}, Storage: jetstream.FileStorage})
		if err != nil {
			t.Fatal(err)
		}
		client, err := bus.DialNATS(ctx, bus.NATSConfig{Name: "oms", URL: s.broker})
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		publisher, err := bus.NewProducer(client, bus.ProducerConfig{Source: "oms", ProducerVersion: "synthetic"})
		if err != nil {
			t.Fatal(err)
		}
		relayPool := syntheticPool(t, ctx, "RELAY", tenant)
		relay, err := outbox.NewRelay(outbox.NewPostgres(relayPool, outbox.ServiceOMS), publisher, slog.Default())
		if err != nil {
			t.Fatal(err)
		}
		if n, err := relay.Flush(ctx, orderID); err != nil || n != 1 {
			t.Fatalf("accepted outbox relay: %d published, %v", n, err)
		}
		consumer, err := facts.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{Durable: "synthetic-orders", FilterSubject: syntheticvenue.AcceptedSubject, AckPolicy: jetstream.AckExplicitPolicy})
		if err != nil {
			t.Fatal(err)
		}

		venue, err := syntheticvenue.New(ctx, syntheticvenue.Config{Tenant: tenant, Portfolio: portfolio, FillTime: time.Now().UTC(), LoseExecuteAck: true}, syntheticPool(t, ctx, "VENUE", tenant))
		if err != nil {
			t.Fatal(err)
		}
		ca, err := testidentity.NewAuthority(spiffeid.RequireTrustDomainFromString("kanz.internal"))
		if err != nil {
			t.Fatal(err)
		}
		issue := func(id string) *testidentity.Source {
			t.Helper()
			s, err := ca.Issue(spiffeid.RequireFromString(id), now.Add(-time.Minute), now.Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			return s
		}
		protocol := issue(syntheticvenue.VenueIdentity)
		actor := issue(syntheticvenue.OMSIdentity)
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsconfig.MTLSServerConfig(protocol, protocol, tlsconfig.AuthorizeID(spiffeid.RequireFromString(syntheticvenue.OMSIdentity))))))
		venuepb.RegisterVenueAdapterServiceServer(srv, venue)
		served := make(chan error, 1)
		go func() { served <- srv.Serve(listener) }()
		t.Cleanup(func() { srv.Stop(); <-served })
		conn, err := grpc.NewClient("passthrough:///"+listener.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(tlsconfig.MTLSClientConfig(actor, actor, tlsconfig.AuthorizeID(spiffeid.RequireFromString(syntheticvenue.VenueIdentity))))))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		venueClient := venuepb.NewVenueAdapterServiceClient(conn)
		ingress, err := syntheticvenue.NewIngress(tenant, portfolio, venueClient, func(ctx context.Context, cmd syntheticvenue.Command) (syntheticvenue.Receipt, error) {
			return callSyntheticOwner(ctx, os.Getenv("SYNTHETIC_OMS_ADAPTER_URL"), struct {
				Version int `json:"version"`
				syntheticvenue.Command
			}{1, cmd})
		})
		if err != nil {
			t.Fatal(err)
		}
		consume := func(delivery uint64, expectUnknown bool) {
			t.Helper()
			msg, err := consumer.Next(jetstream.FetchMaxWait(4 * time.Second))
			if err != nil {
				t.Fatal(err)
			}
			meta, err := msg.Metadata()
			if err != nil || meta.NumDelivered != delivery {
				t.Fatalf("unexpected broker delivery count: %v", err)
			}
			var frame envelopepb.EventFrame
			if err = proto.Unmarshal(msg.Data(), &frame); err != nil {
				t.Fatal(err)
			}
			if err = bus.Validate(frame.GetEnvelope()); err != nil {
				t.Fatal(err)
			}
			if frame.Envelope.GetCorrelationId() != env.GetEventId() || frame.Envelope.GetCausationId() != env.GetEventId() || frame.Envelope.GetTraceContext() != env.GetTraceContext() {
				t.Fatal("accepted fact lost the authenticated command lineage or W3C trace")
			}
			err = ingress.HandleAccepted(ctx, frame.Envelope, frame.Payload)
			if expectUnknown {
				if err == nil {
					t.Fatal("lost venue ACK became success")
				}
				var unknown orderpb.OrderState
				if err = proto.Unmarshal(requireNativeReceipt(t, ctx, read, "FOUND").State, &unknown); err != nil || unknown.Quarantine == nil {
					t.Fatal("remote uncertainty not recorded", err)
				}
				assertSyntheticOrderBlotter(t, os.Getenv("SYNTHETIC_OMS_READ_URL"), tenant, portfolio, &unknown)
				if err = msg.Nak(); err != nil {
					t.Fatal(err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if err = msg.DoubleAck(ctx); err != nil {
					t.Fatal(err)
				}
			}
		}
		consume(1, true)
		if !t.Run("unknown_across_handoff", func(sub *testing.T) {
			before := requireNativeReceipt(sub, ctx, read, "FOUND")
			var beforeState orderpb.OrderState
			if proto.Unmarshal(before.State, &beforeState) != nil || beforeState.GetQuarantine() == nil {
				sub.Fatal("expected a durable unknown remote outcome")
			}
			operator := syntheticPool(sub, ctx, "SWITCH", tenant)
			var epoch int64
			if e := operator.QueryRow(ctx, "SELECT synthetic_test_recovery_handoff()").Scan(&epoch); e != nil || epoch != 3 {
				sub.Fatalf("recovery handoff: %v", e)
			}
			old, e := callSyntheticOwner(ctx, os.Getenv("SYNTHETIC_OMS_RUNTIME_URL"), request)
			if e != nil || old.OK || old.Code != "WRITER_FENCED" {
				sub.Fatal("retired writer confirmed a replay after uncertain venue execution")
			}
			useWriterGeneration(sub, "RECOVERY")
			after := requireNativeReceipt(sub, ctx, read, "FOUND")
			if !bytes.Equal(after.State, before.State) || after.OrderVersion != before.OrderVersion {
				sub.Fatal("handoff changed quarantine, version or confirmed economics")
			}
		}) {
			t.FailNow()
		}
		useWriterGeneration(t, "RECOVERY")
		consume(2, false)
		cancel := s.cancelOrder().GetEnvelope()
		if err = ingress.Cancel(ctx, orderID, cancel.GetEventId(), cancel.GetEventTime().AsTime()); err != nil {
			t.Fatal(err)
		}
		final := requireNativeReceipt(t, ctx, read, "FOUND")
		var state orderpb.OrderState
		if err = proto.Unmarshal(final.State, &state); err != nil {
			t.Fatal(err)
		}
		half := &commonpb.Decimal{Coefficient: 125, Exponent: -2}
		if state.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_CANCELLED || state.Quarantine != nil || state.GetFilledQuantity() == nil || dec.FromProto(state.GetFilledQuantity()).Cmp(dec.FromProto(half)) != 0 {
			t.Fatal("cancel and concurrent partial fill did not resolve to expected economic state")
		}
		if err = ingress.Cancel(ctx, orderID, cancel.GetEventId(), cancel.GetEventTime().AsTime()); err != nil {
			t.Fatal(err)
		}
		replay := requireNativeReceipt(t, ctx, read, "FOUND")
		if !bytes.Equal(replay.State, final.State) || replay.OrderVersion != final.OrderVersion {
			t.Fatal("replay changed final durable state")
		}
		var originalEpoch int64
		var observedEpochs []int64
		if e := runtimePool.QueryRow(ctx, "SELECT writer_epoch FROM synthetic_oms_commands WHERE order_id=$1", orderID).Scan(&originalEpoch); e != nil || originalEpoch != 1 {
			t.Fatalf("immutable admission epoch: %v", e)
		}
		if e := runtimePool.QueryRow(ctx, "SELECT array_agg(DISTINCT writer_epoch ORDER BY writer_epoch) FROM synthetic_oms_observations WHERE order_id=$1", orderID).Scan(&observedEpochs); e != nil || len(observedEpochs) != 2 || observedEpochs[0] != 2 || observedEpochs[1] != 3 {
			t.Fatalf("observed venue generations: %v, %v", observedEpochs, e)
		}
		assertSyntheticOrderBlotter(t, os.Getenv("SYNTHETIC_OMS_READ_URL"), tenant, portfolio, &state)
	})
}
