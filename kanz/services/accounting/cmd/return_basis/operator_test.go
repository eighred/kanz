package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"os/exec"
	"regexp"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/pg"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal"
	"github.com/eighred/kanz/services/accounting/internal/cashmove"
	"github.com/eighred/kanz/services/accounting/internal/consume"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/eighred/kanz/services/accounting/internal/return_basis"
)

const fixtureBenchmarkID = "SYNTH-TR"
const fixtureBenchmarkIssuer = "synthetic-index-publisher"

func proveDailyReturn(t *testing.T, ctx context.Context, reader *pgxpool.Pool, benchmarkSigner ed25519.PrivateKey, editions []returnbasis.Evidence, digests []string, admit func(returnbasis.Evidence) (string, error)) {
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
		Subjects: []string{returnbasis.Subject}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: "ACCOUNTING_OTHER_FIXTURE",
		Subjects: []string{consume.SubjectPortfolioCash, "accounting.cash.>", "accounting.custody.>", "accounting.collateral.>", "order.>", "market.>", "settlement.>", "dlq.>"}}); err != nil {
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
	store := returnbasis.NewStore(reader)
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
		// Use the real reviewed Accounting command, outbox and journal consumer.
		runtime, err := pg.NewTenantPool(ctx, runtimeDSN, first.Tenant)
		if err != nil {
			t.Fatal("cash command fixture could not connect")
		}
		defer runtime.Close()
		movement := cashmove.CashMovement{MovementID: "intraday-subscription", PortfolioID: first.Portfolio, Kind: cashmove.Subscription,
			Amount: big.NewRat(40, 1), Currency: first.Currency, Effective: time.Date(2020, 2, 4, 12, 0, 0, 0, time.UTC),
			SourceRef: "synthetic-transfer-r1"}
		journal := ledger.NewPostgres(runtime)
		start := time.Date(2020, 2, 4, 0, 0, 0, 0, time.UTC)
		end := start.AddDate(0, 0, 1)
		seedTime := start.AddDate(0, 0, -2)
		for _, entry := range []*ledger.Event{
			{EntryID: "fixture-opening-funding", PortfolioID: first.Portfolio, Type: ledger.EntryCash,
				Cash: big.NewRat(100, 1), CashCurrency: first.Currency, Effective: seedTime, Knowledge: seedTime},
			{EntryID: "fixture-opening-trade", PortfolioID: first.Portfolio, Type: ledger.EntryTrade, InstrumentID: "SYNTH",
				Quantity: big.NewRat(10, 1), Price: big.NewRat(10, 1), Cash: big.NewRat(-100, 1),
				CashCurrency: first.Currency, Effective: seedTime.Add(time.Hour), Knowledge: seedTime.Add(time.Hour)},
		} {
			if err := journal.Append(ctx, entry, nil); err != nil {
				t.Fatal("seed opening book", err)
			}
		}
		command := cashmove.Command{Tenant: first.Tenant, Actor: "synthetic-accountant", Reason: "midday contribution fixture", Movement: movement}
		commandDigest, err := cashmove.ReviewDigest(command)
		if err != nil {
			t.Fatal(err)
		}
		commands := cashmove.NewCommands(runtime)
		if _, err := commands.Accept(ctx, command, commandDigest); err != nil {
			t.Fatal("cash command acceptance failed", err)
		}
		wait("Accounting did not book the real cash movement", func(check context.Context) bool {
			r, err := commands.Status(check, first.Tenant, first.Portfolio, movement.MovementID)
			return err == nil && r.Status == "journal_recorded"
		})
		events, err := journal.Journal(ctx, first.Portfolio)
		if err != nil {
			t.Fatal("read booked valuation inputs", err)
		}
		captureNAV := func(ref, quote, want string, at, effectiveCut, known time.Time) *returnbasis.Close {
			t.Helper()
			book := ledger.ReplayAsOf(first.Portfolio, events, effectiveCut, known)
			value, err := accounting.ComputeNAV(book, first.Currency, at, map[string]*big.Rat{"SYNTH": dec.Rat(quote)})
			if err != nil || value.Total.Cmp(dec.Rat(want)) != 0 {
				t.Fatal("book and price did not produce the expected signed NAV", ref, err)
			}
			return &returnbasis.Close{Reference: ref, Total: dec.Exact(value.Total.RatString())}
		}
		appendEdition := func(e returnbasis.Evidence) string {
			t.Helper()
			got, err := admit(e)
			if err != nil {
				t.Fatal("signed split edition refused", err)
			}
			editions = append(editions, e)
			digests = append(digests, got)
			wait("Accounting did not observe the split edition", func(check context.Context) bool {
				_, err := store.ReadObserved(check, e.Portfolio, e.Currency, e.CloseDate, e.AsOf)
				return err == nil
			})
			return got
		}
		count := uint64(1)
		intraday := first
		intraday.CloseDate = end.Format(time.DateOnly)
		intraday.AsOf = time.Now().UTC().Truncate(time.Microsecond)
		intraday.Opening = captureNAV("nav-intraday-open", "10", "100", start, start, intraday.AsOf)
		intraday.Closing = captureNAV("nav-intraday-close", "12.5", "165", end, end, intraday.AsOf)
		split := returnbasis.FlowSplit{MovementID: movement.MovementID, CommandDigest: commandDigest, Effective: movement.Effective,
			SignedAmount: "40",
			Before:       captureNAV("nav-pre-flow", "11", "110", movement.Effective, movement.Effective.Add(-time.Microsecond), intraday.AsOf),
			After:        captureNAV("nav-post-flow", "11", "150", movement.Effective, movement.Effective, intraday.AsOf)}
		intraday.Flows = &returnbasis.FlowCompletion{Scope: first.Flows.Scope, Reference: "fund-flows-split", EventCount: &count, Split: &split}
		benchmark := returnbasis.BenchmarkObservation{
			Format: returnbasis.BenchmarkFormat, Tenant: first.Tenant, Portfolio: first.Portfolio, Currency: first.Currency,
			BenchmarkID: fixtureBenchmarkID, Issuer: fixtureBenchmarkIssuer, DefinitionEffective: seedTime,
			Start: start, End: end, KnownAt: end.Add(time.Hour), OpeningLevel: "250", ClosingLevel: "270",
		}
		signIndex := func(mark returnbasis.BenchmarkObservation) *returnbasis.BenchmarkEvidence {
			t.Helper()
			raw, err := json.Marshal(mark)
			if err != nil {
				t.Fatal(err)
			}
			return &returnbasis.BenchmarkEvidence{Observation: &mark, Signature: ed25519.Sign(benchmarkSigner, raw)}
		}
		intraday.Benchmark = signIndex(benchmark)
		badSplit := split
		badSplit.SignedAmount = "41"
		badSplit.After = &returnbasis.Close{Reference: "wrong-post-flow-nav", Total: "151"}
		wrong := intraday
		badFlows := *wrong.Flows
		badFlows.Split = &badSplit
		wrong.Flows = &badFlows
		if _, err := admit(wrong); err == nil {
			t.Fatal("signed NAV bridge with a different booked movement amount was accepted")
		}
		badSplit.SignedAmount = "40"
		if _, err := admit(wrong); err == nil {
			t.Fatal("signed NAV bridge that did not reconcile to the cash amount was accepted")
		}
		wrong = intraday
		wrong.AsOf = time.Date(2020, 2, 5, 1, 0, 0, 0, time.UTC)
		if _, err := admit(wrong); err == nil {
			t.Fatal("knowledge cut before booking accepted a future cash movement")
		}
		wrong = intraday
		altered := benchmark
		altered.KnownAt = wrong.AsOf.Add(time.Hour)
		wrong.Benchmark = signIndex(altered)
		if _, err := admit(wrong); err == nil {
			t.Fatal("future benchmark became visible under an older knowledge horizon")
		}
		altered = benchmark
		altered.BenchmarkID = "UNENROLLED-TR"
		wrong.Benchmark = signIndex(altered)
		if _, err := admit(wrong); err == nil {
			t.Fatal("signer reused for an unapproved benchmark")
		}
		altered = benchmark
		altered.Tenant = "other"
		wrong.Benchmark = signIndex(altered)
		if _, err := admit(wrong); err == nil {
			t.Fatal("foreign benchmark evidence reached the local report")
		}
		wrong.Benchmark = signIndex(benchmark)
		wrong.Benchmark.Observation.ClosingLevel = "275"
		if _, err := admit(wrong); err == nil {
			t.Fatal("Accounting close signature authorized a changed benchmark")
		}
		splitDigest := appendEdition(intraday)
		corrected := intraday
		corrected.PreviousDigest = splitDigest
		corrected.AsOf = time.Now().UTC().Truncate(time.Microsecond)
		corrected.Closing = captureNAV("nav-intraday-restated", "9.2", "132", end, end, corrected.AsOf)
		correctedDigest := appendEdition(corrected)
		indexCorrected := corrected
		indexCorrected.PreviousDigest = correctedDigest
		indexCorrected.AsOf = time.Now().UTC().Truncate(time.Microsecond)
		revision := benchmark
		revision.ClosingLevel = "275"
		revision.KnownAt = indexCorrected.AsOf
		indexCorrected.Benchmark = signIndex(revision)
		indexDigest := appendEdition(indexCorrected)

		missingBenchmark := indexCorrected
		missingBenchmark.PreviousDigest = indexDigest
		missingBenchmark.AsOf = time.Now().UTC().Truncate(time.Microsecond)
		missingBenchmark.Benchmark = &returnbasis.BenchmarkEvidence{}
		missingIndexDigest := appendEdition(missingBenchmark)
		mismatched := missingBenchmark
		mismatched.PreviousDigest = missingIndexDigest
		mismatched.AsOf = time.Now().UTC().Truncate(time.Microsecond)
		wrongDay := benchmark
		wrongDay.Start, wrongDay.End = start.AddDate(0, 0, -1), start
		wrongDay.KnownAt = wrongDay.End.Add(time.Hour)
		mismatched.Benchmark = signIndex(wrongDay)
		mismatchDigest := appendEdition(mismatched)

		missing := indexCorrected
		missing.PreviousDigest = mismatchDigest
		missing.AsOf = time.Now().UTC().Truncate(time.Microsecond)
		partial := split
		partial.Before = nil
		partialFlows := *missing.Flows
		partialFlows.Split = &partial
		missing.Flows = &partialFlows
		lastDigest := appendEdition(missing)

		// A second booking learned after these editions must count even when it
		// is denominated in another currency. Earlier reports remain immutable.
		additional := command
		additional.Movement.MovementID = "intraday-late-subscription"
		additional.Movement.Currency = "EUR"
		additional.Movement.Effective = movement.Effective.Add(time.Hour)
		additionalDigest, err := cashmove.ReviewDigest(additional)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = commands.Accept(ctx, additional, additionalDigest); err != nil {
			t.Fatal("late cash command acceptance failed", err)
		}
		wait("Accounting did not book the second movement", func(check context.Context) bool {
			r, err := commands.Status(check, first.Tenant, first.Portfolio, additional.Movement.MovementID)
			return err == nil && r.Status == "journal_recorded"
		})
		wrong = corrected
		wrong.PreviousDigest = lastDigest
		wrong.AsOf = time.Now().UTC().Truncate(time.Microsecond)
		if _, err := admit(wrong); err == nil {
			t.Fatal("single-flow edition accepted after a second cash movement in another currency")
		}
		if replay, err := admit(intraday); err != nil || replay != splitDigest {
			t.Fatal("original signed intraday close changed after restatement", err)
		}
		t.Logf("booked external flow %s command=%s effective=%s", movement.MovementID, commandDigest, movement.Effective.Format(time.RFC3339Nano))
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
	payloads := make([][]byte, len(editions))
	for i := range editions {
		msg, err := stream.GetMsg(ctx, uint64(i+1))
		if err != nil || msg.Subject != returnbasis.Subject {
			t.Fatal("wrong broker completion", err)
		}
		envelope, payload, err := bus.Unframe(msg.Data)
		if err != nil || envelope.GetCorrelationId() != digests[i] {
			t.Fatal("broker completion did not bind the signed source")
		}
		payloads[i] = payload
	}
	assertRetained := func() {
		t.Helper()
		for i, edition := range editions {
			var persisted []byte
			err := reader.QueryRow(ctx, `SELECT observed_payload FROM return_basis_publications
				WHERE portfolio_id=$1 AND currency=$2 AND close_date=$3::text::date AND as_of=$4`,
				edition.Portfolio, edition.Currency, edition.CloseDate, edition.AsOf).Scan(&persisted)
			if err != nil || !bytes.Equal(persisted, payloads[i]) {
				t.Fatal("the original observed bytes differ from the broker FACT", err)
			}
		}
	}
	assertRetained()
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

	assertRetained()
	values := make([]map[string]string, len(editions))
	for i, e := range editions {
		values[i] = map[string]string{"as_of": e.AsOf.Format(time.RFC3339Nano), "digest": digests[i], "date": e.CloseDate}
		if e.Flows != nil && e.Flows.Split != nil {
			values[i]["movement_id"] = e.Flows.Split.MovementID
			values[i]["command_digest"] = e.Flows.Split.CommandDigest
		}
		if e.Benchmark != nil && e.Benchmark.Observation != nil {
			raw, err := json.Marshal(e.Benchmark.Observation)
			if err != nil {
				t.Fatal(err)
			}
			values[i]["benchmark_digest"] = fmt.Sprintf("%x", sha256.Sum256(raw))
		}
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
	t.Log("observed PostgreSQL bytes matched each original FACT before and after replay")
	for i, e := range editions {
		t.Logf("edition close=%s horizon=%s source_sha256=%s benchmark_sha256=%s", e.CloseDate, e.AsOf.Format(time.RFC3339Nano), digests[i], values[i]["benchmark_digest"])
	}
}
