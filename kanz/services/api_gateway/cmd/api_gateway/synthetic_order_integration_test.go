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

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/proto"

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

func signedSyntheticOrder(t *testing.T) *envelopepb.EventFrame {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
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
	stream, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: "SYNTHETIC_ORDERS", Subjects: []string{orderSubject}, Storage: jetstream.FileStorage})
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
	reg := prometheus.NewRegistry()
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
	router, err := buildRouter(cfg, gateway.New(nil, nil, nil, "", logger), orders.New(producer, "", gate), proxy.New(nil, proxyRoles(cfg)), nil, testObservability(t), &atomic.Bool{}, logger, recorder, authn, nil)
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
	post := func(jwt, key string, want int) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, api.URL+"/v1/orders", bytes.NewReader(body))
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
	post("", "no-session", http.StatusUnauthorized)
	post(token("synthetic-viewer", baselineRole), "no-trade", http.StatusForbidden)
	post(trader, "before-resume", http.StatusLocked)
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
	post(trader, key, http.StatusAccepted)
	post(trader, key, http.StatusAccepted)
	info, err = stream.Info(ctx)
	if err != nil || info.State.Msgs != 1 {
		t.Fatal("expected exactly one durable order command", err)
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
	scope := command.GetMetadata()
	if env.GetTenantId() != tenant || env.GetIdempotencyKey() != key || env.GetPartitionKey() != orderID || command.GetOrderId() != orderID || scope.GetIssuer() != "user:synthetic-trader" || scope.GetTargetId() != orderID || len(scope.GetPrincipalPortfolios()) != 1 || scope.GetPrincipalPortfolios()[0] != portfolio || command.GetPortfolioId() != portfolio {
		t.Fatal("durable command changed the authenticated order identity")
	}
	if command.GetLimitPrice().GetCoefficient() != 9101 || command.GetLimitPrice().GetExponent() != -2 || command.GetQuantity().GetCoefficient() != 25 || command.GetQuantity().GetExponent() != -1 {
		t.Fatal("durable command changed exact price or quantity")
	}
	return &frame
}
