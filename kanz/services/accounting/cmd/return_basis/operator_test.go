package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"regexp"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/return_basis"
)

func proveDailyReturn(t *testing.T, ctx context.Context, store *returnbasis.Store, editions []returnbasis.Evidence, digests []string) {
	t.Helper()
	runtimeDSN := os.Getenv("TEST_RETURN_BASIS_RUNTIME")
	if runtimeDSN == "" || os.Getenv("RETURN_PILOT_ACCOUNTING_BIN") == "" || os.Getenv("RETURN_PILOT_GATEWAY_PROBE") == "" {
		t.Fatal("operator qualification requires Accounting and browser probes")
	}
	broker, err := natsserver.NewServer(&natsserver.Options{
		Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir(), NoSigs: true,
	})
	if err != nil {
		t.Fatal("start isolated JetStream")
	}
	broker.Start()
	t.Cleanup(func() { broker.Shutdown(); broker.WaitForShutdown() })
	if !broker.ReadyForConnections(5 * time.Second) {
		t.Fatal("JetStream unavailable")
	}
	control, err := nats.Connect(broker.ClientURL())
	if err != nil {
		t.Fatal("fixture connection unavailable")
	}
	defer control.Close()
	js, err := jetstream.New(control)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: "ACCOUNTING_RETURN_FIXTURE",
		Subjects: []string{"accounting.>", "order.>", "market.>", "settlement.>", "dlq.>"}})
	if err != nil {
		t.Fatal(err)
	}
	start := func(group string) (string, func()) {
		t.Helper()
		socket, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		address := socket.Addr().String()
		_ = socket.Close()
		worker, cancel := context.WithCancel(ctx)
		cmd := exec.CommandContext(worker, os.Getenv("RETURN_PILOT_ACCOUNTING_BIN"))
		cmd.Env = []string{"ACCOUNTING_DAILY_RETURN_ENABLED=true", "ACCOUNTING_DATABASE_URL=" + runtimeDSN,
			"ACCOUNTING_TENANT=basis-test", "ACCOUNTING_NATS_URL=" + broker.ClientURL(),
			"ACCOUNTING_LISTEN=" + address, "ACCOUNTING_METRICS_LISTEN=127.0.0.1:0",
			"ACCOUNTING_CONSUMER_GROUP=" + group, "ACCOUNTING_SNAPSHOT_INTERVAL=0s"}
		for _, k := range []string{"SystemRoot", "TEMP", "TMP", "TMPDIR"} {
			if v, ok := os.LookupEnv(k); ok {
				cmd.Env = append(cmd.Env, k+"="+v)
			}
		}
		if err := cmd.Start(); err != nil {
			cancel()
			t.Fatal("start Accounting")
		}
		return "http://" + address, func() { cancel(); _ = cmd.Wait() }
	}

	first := editions[0]
	if _, err := store.ReadObserved(ctx, first.Portfolio, first.Currency, first.CloseDate, first.AsOf); err != returnbasis.ErrNotFound {
		t.Fatal("unsent evidence appeared in the observed view", err)
	}
	wait := func(label string, ready func(context.Context) bool) {
		t.Helper()
		check, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			if ready(check) {
				return
			}
			select {
			case <-ticker.C:
			case <-check.Done():
				t.Fatal(label)
			}
		}
	}
	_, stop := start("return-initial")
	func() {
		defer stop()
		wait("Accounting did not observe each report", func(check context.Context) bool {
			for _, version := range editions {
				if _, err := store.ReadObserved(check, version.Portfolio, version.Currency, version.CloseDate, version.AsOf); err != nil {
					return false
				}
			}
			return true
		})
	}()
	before, err := stream.Info(ctx)
	if err != nil || before.State.Msgs != uint64(len(editions)) {
		t.Fatal("expected a single JetStream FACT for each signed edition")
	}
	for i := range editions {
		msg, err := stream.GetMsg(ctx, uint64(i+1))
		if err != nil || msg.Subject != returnbasis.Subject {
			t.Fatal("wrong broker completion", err)
		}
		envelope, _, err := bus.Unframe(msg.Data)
		if err != nil || envelope.GetCorrelationId() != digests[i] {
			t.Fatal("broker completion did not bind the signed source")
		}
	}
	site, stop := start("return-replay")
	defer stop()
	wait("fresh durable did not replay all editions", func(check context.Context) bool {
		cs := stream.ListConsumers(check)
		for item := range cs.Info() {
			if item.Name == "return-replay-accounting_performance_daily_return" &&
				item.AckFloor.Stream == before.State.LastSeq && item.NumAckPending == 0 {
				return true
			}
		}
		return false
	})
	after, err := stream.Info(ctx)
	if err != nil || after.State.Msgs != before.State.Msgs {
		t.Fatal("replay produced another completion")
	}

	values := make([]map[string]string, len(editions))
	for i, e := range editions {
		values[i] = map[string]string{"as_of": e.AsOf.Format(time.RFC3339Nano), "digest": digests[i]}
	}
	wire, err := json.Marshal(map[string]any{"date": first.CloseDate, "editions": values})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, os.Getenv("RETURN_PILOT_GATEWAY_PROBE"), "-test.run=^TestDailyReturnGatewayBrowser$", "-test.v", "-test.count=1")
	cmd.Env = []string{"RETURN_PILOT_ACCOUNTING_URL=" + site}
	for _, k := range []string{"RETURN_PILOT_BFF_PROBE", "RETURN_PILOT_BROWSER_SCRIPT", "TEST_MFA_NODE", "TEST_MFA_PLAYWRIGHT", "TEST_MFA_STATIC_DIR",
		"PATH", "SystemRoot", "SystemDrive", "USERPROFILE", "LOCALAPPDATA", "TEMP", "TMP", "HOME", "PLAYWRIGHT_BROWSERS_PATH"} {
		if v, ok := os.LookupEnv(k); ok {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	cmd.Stdin = bytes.NewReader(wire)
	cmd.Stderr = io.Discard
	output, err := cmd.Output()
	if err != nil || !bytes.Contains(output, []byte("daily return gateway passed")) {
		// Do not retain provisioned sessions or infrastructure environment.
		stage := regexp.MustCompile(`return-browser-stage=[a-z-]+`).Find(output)
		t.Fatalf("daily return operator probe failed %s", stage)
	}
	t.Logf("real JetStream replay and signed browser read passed with %d FACTs", after.State.Msgs)
	for i, e := range editions {
		t.Logf("edition horizon=%s source_sha256=%s", e.AsOf.Format(time.RFC3339Nano), digests[i])
	}
}
