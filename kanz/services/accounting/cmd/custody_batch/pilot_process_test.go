package main

import (
	"context"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/eighred/kanz/services/accounting/internal/custody"
)

// Each command receives only its own fixture connection. In particular the
// importer cannot open the accounting runtime's pool, or authenticate as its
// broker consumer. Child diagnostics never expose connection settings.
func pilotProcessEnvironment() []string {
	env := []string{"ACCOUNTING_TENANT=synthetic-custody-pilot", "ACCOUNTING_CUSTODY_SIGNED_BATCH_ONLY=true"}
	for _, name := range []string{"SystemRoot", "TEMP", "TMP", "TMPDIR"} {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return env
}

func runPilotImporter(t *testing.T, ctx context.Context, sourceDB, broker, authority, batch, signature string) {
	t.Helper()
	cmd := exec.CommandContext(ctx, os.Getenv("CUSTODY_PILOT_IMPORTER"), "-authority", authority, "-batch", batch, "-signature", signature)
	cmd.Env = append(pilotProcessEnvironment(), "ACCOUNTING_DATABASE_URL="+sourceDB, "ACCOUNTING_NATS_URL="+broker,
		"ACCOUNTING_CUSTODY_PAIRS=PF1:CUST-A", "ACCOUNTING_CUSTODY_ACCOUNTS=PF1:CUST-A:sandbox-account")
	if err := cmd.Run(); err != nil {
		t.Fatal("signed custody importer process failed", err)
	}
}

// With no scheduled pairs the service consumes source FACTs. With the dated
// pair it also reconciles on startup and serves the resulting customer read.
// Waiting for source acknowledgement before that tick fixes the order of the
// independent processes without replacing the scheduler or its clock.
func startPilotAccounting(t *testing.T, ctx context.Context, broker pilotBroker, group string, day time.Time) (string, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	endpoint, err := url.Parse(broker.address)
	if err != nil {
		t.Fatal("invalid local fixture broker")
	}
	endpoint.User = url.UserPassword(broker.accounting.Username, broker.accounting.Password)
	worker, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(worker, os.Getenv("CUSTODY_PILOT_SERVICE"))
	cmd.Env = append(pilotProcessEnvironment(), "ACCOUNTING_DATABASE_URL="+os.Getenv("TEST_POSTGRES_URL"),
		"ACCOUNTING_NATS_URL="+endpoint.String(), "ACCOUNTING_LISTEN="+address,
		"ACCOUNTING_METRICS_LISTEN=127.0.0.1:0", "ACCOUNTING_CONSUMER_GROUP="+group,
		"ACCOUNTING_CUSTODY_REVIEW_READ_ONLY=true", "ACCOUNTING_SNAPSHOT_INTERVAL=0s")
	if !day.IsZero() {
		lag := int(custody.BusinessDay(time.Now()).Sub(custody.BusinessDay(day)) / (24 * time.Hour))
		cmd.Env = append(cmd.Env, "ACCOUNTING_CUSTODY_LAG_DAYS="+strconv.Itoa(lag), "ACCOUNTING_CUSTODY_PAIRS=PF1:CUST-A",
			"ACCOUNTING_CUSTODY_ACCOUNTS=PF1:CUST-A:sandbox-account")
	}
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal("cannot start fixture accounting process", err)
	}
	stop := func() {
		cancel()
		_ = cmd.Wait()
	}
	return "http://" + address, stop
}

func awaitPilot(t *testing.T, ctx context.Context, condition string, ready func(context.Context) bool) {
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
		case <-check.Done():
			t.Fatalf("accounting process did not reach %s", condition)
		case <-ticker.C:
		}
	}
}

func awaitPilotStatementAck(t *testing.T, ctx context.Context, broker pilotBroker, group string, sequence uint64) {
	t.Helper()
	awaitPilot(t, ctx, "durable signed statement acknowledgement", func(check context.Context) bool {
		c, err := broker.statements.Consumer(check, group+"-accounting_custody_statement")
		if err != nil {
			return false
		}
		info, err := c.Info(check)
		return err == nil && info.Config.FilterSubject == custody.SubjectStatement && info.AckFloor.Stream == sequence && info.NumAckPending == 0
	})
}
