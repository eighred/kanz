package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eighred/kanz/pkg/observability"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/proto"
	"strings"

	"github.com/eighred/kanz/internal/identity"
	"github.com/eighred/kanz/internal/platform/halt"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	lifecyclepb "github.com/eighred/kanz/kanz-schemas-go/lifecycle/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/api_gateway/internal/config"
	"github.com/eighred/kanz/services/api_gateway/internal/gateway"
	"github.com/eighred/kanz/services/api_gateway/internal/orders"
	"github.com/eighred/kanz/services/api_gateway/internal/proxy"
)

// Start a fresh broker for this test so the lifecycle FACT can only affect
// the test gateway. Never publish a resume on a caller-configured broker.
func TestSyntheticOrderAuthenticatedIngress(t *testing.T) {
	signedSyntheticOrder(t)
}

type syntheticSession struct {
	frame       *envelopepb.EventFrame
	broker      string
	js          jetstream.JetStream
	cancelOrder func() *envelopepb.EventFrame
}

func signedSyntheticOrder(t *testing.T) *envelopepb.EventFrame {
	return exerciseSyntheticSession(t, nil)
}

func exerciseSyntheticSession(t *testing.T, exercise func(context.Context, syntheticSession)) *envelopepb.EventFrame {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	broker, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir(), NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	broker.Start()
	t.Cleanup(func() { broker.Shutdown(); broker.WaitForShutdown() })
	if !broker.ReadyForConnections(5 * time.Second) {
		t.Fatal("synthetic broker startup timed out")
	}

	nc, err := nats.Connect(broker.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	const tenant, portfolio, orderID = "synthetic-tenant", "synthetic-portfolio", "0f36dca681444918a676761d47b63844"
	orderSubject := bus.TenantRoutedSubject(tenant, "order.order.submit")
	stream, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: "SYNTHETIC_ORDERS", Subjects: []string{orderSubject, bus.TenantRoutedSubject(tenant, "order.order.cancel")}, Storage: jetstream.FileStorage})
	if err != nil {
		t.Fatal(err)
	}
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{Name: "SYNTHETIC_CONTROL", Subjects: []string{halt.SubjectModeChanged, auth.AuthzDecisionEventType}})
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{Source: "api-gateway", NATSURL: broker.ClientURL(), RequiredRole: baselineRole, TradeRole: traderRole}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	gate := halt.NewGate(time.Now)
	obs := testObservability(t)
	reg := obs.Registry
	producer, consumer, closeBus, err := buildBus(ctx, cfg, gate, logger, bus.NewBusMetrics(reg))
	if err != nil {
		t.Fatal(err)
	}
	defer closeBus()
	subCtx, stop := context.WithCancel(ctx)
	wait, err := halt.Arm(subCtx, consumer, gate, logger)
	if err != nil {
		stop()
		t.Fatal(err)
	}
	defer func() { stop(); _ = wait() }()
	recorder, closeRecorder := buildDecisionRecorder(producer, reg, logger)
	defer closeRecorder()
	st := newIdentityStub(t)
	st.feedUp.Store(true)
	authn, revocations := buildWired(t, st, &captureHandler{})
	if err := revocations.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	router, err := buildRouter(cfg, gateway.New(nil, nil, nil, "", logger), orders.New(producer, "", gate), proxy.New(nil, proxyRoles(cfg)), nil, obs, &atomic.Bool{}, logger, recorder, authn, nil)
	if err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(router)
	defer api.Close()
	token := func(subject string, roles ...string) string {
		t.Helper()
		jwt, _, err := st.signer.Mint(&identity.User{Subject: subject, Tenant: tenant, Portfolios: []string{portfolio}, Roles: roles, Status: identity.StatusActive})
		if err != nil {
			t.Fatal(err)
		}
		return jwt
	}
	trader := token("synthetic-trader", baselineRole, traderRole)
	body := []byte(`{"order_id":"` + orderID + `","portfolio_id":"` + portfolio + `","instrument_id":"SYNTH-USD","side":"SIDE_BUY","quantity":{"coefficient":"25","exponent":-1},"limit_price":{"coefficient":"9101","exponent":-2},"order_type":"ORDER_TYPE_LIMIT","time_in_force":"TIME_IN_FORCE_GTC","venue":"XSIM"}`)
	post := func(path, jwt, key string, payload []byte, want int) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, api.URL+path, bytes.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if jwt != "" {
			req.Header.Set("Authorization", "Bearer "+jwt)
		}
		req.Header.Set("Idempotency-Key", key)
		resp, err := api.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		reply, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if err != nil || resp.StatusCode != want {
			t.Fatalf("submit: got HTTP %d (%s), want %d: %v", resp.StatusCode, reply, want, err)
		}
	}
	post("/v1/orders", "", "no-session", body, http.StatusUnauthorized)
	post("/v1/orders", token("synthetic-viewer", baselineRole), "no-trade", body, http.StatusForbidden)
	post("/v1/orders", trader, "before-resume", body, http.StatusLocked)
	info, err := stream.Info(ctx)
	if err != nil || info.State.Msgs != 0 {
		t.Fatal("a refused request reached the order stream", err)
	}
	// The lifecycle publisher is scoped to this broker's private dynamic port.
	notice := &lifecyclepb.ModeChanged{Component: halt.ComponentSystem, NewMode: lifecyclepb.OperatingMode_OPERATING_MODE_NORMAL, ChangedBy: "user:synthetic-operator", Reason: "disposable OMS session"}
	operator, err := bus.DialNATS(ctx, bus.NATSConfig{URL: broker.ClientURL(), Name: "synthetic-halt-operator"})
	if err != nil {
		t.Fatal(err)
	}
	defer operator.Close()
	lifecycle, err := bus.NewProducer(operator, bus.ProducerConfig{Source: "kanz-halt", ProducerVersion: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	err = lifecycle.Publish(ctx, bus.Event{Subject: halt.SubjectModeChanged, EventType: halt.SubjectModeChanged, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, Domain: "platform", PayloadSchemaRef: "lifecycle.v1.ModeChanged:1", EventTime: time.Now().UTC(), PartitionKey: notice.Component, TenantID: tenant, Payload: notice})
	if err != nil {
		t.Fatal(err)
	}
	for gate.Halted() {
		select {
		case <-ctx.Done():
			t.Fatal("gateway did not observe operator resume")
		case <-time.After(10 * time.Millisecond):
		}
	}
	const key = "synthetic-first-order"
	post("/v1/orders", trader, key, body, http.StatusAccepted)
	post("/v1/orders", trader, key, body, http.StatusAccepted)
	info, err = stream.Info(ctx)
	if err != nil || info.State.Msgs != 1 {
		t.Fatal("expected exactly one durable order command", err)
	}
	if exercise != nil {
		conflict := bytes.Replace(body, []byte(`"9101"`), []byte(`"9102"`), 1)
		post("/v1/orders", trader, key, conflict, http.StatusConflict)
	}
	raw, err := stream.GetMsg(ctx, info.State.LastSeq)
	if err != nil {
		t.Fatal(err)
	}
	var frame envelopepb.EventFrame
	if err = proto.Unmarshal(raw.Data, &frame); err != nil {
		t.Fatal(err)
	}
	env := frame.GetEnvelope()
	var command orderpb.SubmitOrder
	if err = proto.Unmarshal(frame.GetPayload(), &command); err != nil {
		t.Fatal(err)
	}
	tid := trace.SpanContextFromContext(observability.ContextWithTraceparent(ctx, env.GetTraceContext())).TraceID()
	if !tid.IsValid() {
		t.Fatal("order ingress produced no trace ID")
	}
	metrics, err := api.Client().Get(api.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer metrics.Body.Close()
	metricWire, err := io.ReadAll(io.LimitReader(metrics.Body, 1<<20))
	if err != nil || metrics.StatusCode != http.StatusOK || !strings.Contains(string(metricWire), `kanz_bus_publish_total{result="ok",subject="order.order.submit"} 1`) || strings.Contains(string(metricWire), orderSubject) {
		t.Fatalf("gateway order publish metric must aggregate exactly one command across tenants (HTTP %d)", metrics.StatusCode)
	}
	scope := command.GetMetadata()
	if env.GetTenantId() != tenant || env.GetIdempotencyKey() != key || env.GetPartitionKey() != orderID || command.GetOrderId() != orderID || scope.GetIssuer() != "user:synthetic-trader" || scope.GetTargetId() != orderID || len(scope.GetPrincipalPortfolios()) != 1 || scope.GetPrincipalPortfolios()[0] != portfolio || command.GetPortfolioId() != portfolio {
		t.Fatal("durable command changed the authenticated order identity")
	}
	if command.GetLimitPrice().GetCoefficient() != 9101 || command.GetLimitPrice().GetExponent() != -2 || command.GetQuantity().GetCoefficient() != 25 || command.GetQuantity().GetExponent() != -1 {
		t.Fatal("durable command changed exact price or quantity")
	}
	if exercise != nil {
		exercise(ctx, syntheticSession{frame: &frame, broker: broker.ClientURL(), js: js, cancelOrder: func() *envelopepb.EventFrame {
			t.Helper()
			post("/v1/orders/"+orderID+"/cancel", trader, "synthetic-cancel", nil, http.StatusAccepted)
			msg, err := stream.GetLastMsgForSubject(ctx, bus.TenantRoutedSubject(tenant, "order.order.cancel"))
			if err != nil {
				t.Fatal(err)
			}
			var cancelFrame envelopepb.EventFrame
			var cancelCmd orderpb.CancelOrder
			if proto.Unmarshal(msg.Data, &cancelFrame) != nil || proto.Unmarshal(cancelFrame.Payload, &cancelCmd) != nil || cancelFrame.GetEnvelope().GetEventClass() != envelopepb.EventClass_EVENT_CLASS_COMMAND || cancelFrame.GetEnvelope().GetEventType() != "order.order.cancel" || cancelFrame.GetEnvelope().GetSource() != "api-gateway" || cancelFrame.GetEnvelope().GetTenantId() != tenant || cancelCmd.GetOrderId() != orderID || cancelCmd.GetMetadata().GetIssuer() != scope.GetIssuer() || !auth.PortfolioEntitled(cancelCmd.GetMetadata().GetPrincipalPortfolios(), portfolio) {
				t.Fatal("cancellation did not carry the authenticated order authority")
			}
			return &cancelFrame
		}})
	}
	return &frame
}
