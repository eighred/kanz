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
	"github.com/eighred/kanz/internal/fillfact"
	"github.com/eighred/kanz/internal/pg"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal"
	"github.com/eighred/kanz/services/accounting/internal/cashmove"
	"github.com/eighred/kanz/services/accounting/internal/consume"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/eighred/kanz/services/accounting/internal/return_basis"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const fixtureBenchmarkID = "SYNTH-TR"
const fixtureBenchmarkIssuer = "synthetic-index-publisher"

func proveDailyReturn(t *testing.T, ctx context.Context, reader *pgxpool.Pool, benchmarkSigner ed25519.PrivateKey, editions []returnbasis.Evidence, digests []string, admit func(returnbasis.Evidence) (string, error)) {
	t.Helper()
	runtimeDSN := os.Getenv("TEST_RETURN_BASIS_RUNTIME")
	if runtimeDSN == "" || os.Getenv("RETURN_PILOT_ACCOUNTING_BIN") == "" || os.Getenv("RETURN_PILOT_GATEWAY_PROBE") == "" {
		t.Fatal("operator qualification requires Accounting and browser probes")
	}
	master := startMasterPilot(t, ctx)
	historyURL := "http://" + master.historyAddr
	t.Setenv("ACCOUNTING_DATAMASTER_HISTORY_URL", historyURL)
	classificationReferences := map[string]string{}
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
	other, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: "ACCOUNTING_OTHER_FIXTURE",
		Subjects: []string{consume.SubjectPortfolioCash, "accounting.cash.>", "accounting.custody.>", "accounting.collateral.>", "order.>", "market.>", "settlement.>", "dlq.>"}})
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
			"ACCOUNTING_DATAMASTER_HISTORY_URL=" + historyURL,
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
	store := returnbasis.NewStore(reader, nil)
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
			{EntryID: "fixture-opening-trade-a", PortfolioID: first.Portfolio, Type: ledger.EntryTrade, InstrumentID: "SYNTH-A",
				Quantity: big.NewRat(4, 1), Price: big.NewRat(15, 1), Cash: big.NewRat(-60, 1),
				CashCurrency: first.Currency, Effective: seedTime.Add(time.Hour), Knowledge: seedTime.Add(time.Hour)},
			{EntryID: "fixture-opening-trade-b", PortfolioID: first.Portfolio, Type: ledger.EntryTrade, InstrumentID: "SYNTH-B",
				Quantity: big.NewRat(5, 1), Price: big.NewRat(8, 1), Cash: big.NewRat(-40, 1),
				CashCurrency: first.Currency, Effective: seedTime.Add(2 * time.Hour), Knowledge: seedTime.Add(2 * time.Hour)},
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
		fee := command
		fee.Movement.MovementID = "intraday-fee"
		fee.Movement.Kind = cashmove.Fee
		fee.Movement.Amount = big.NewRat(3, 1)
		fee.Movement.Effective = movement.Effective.Add(time.Hour)
		fee.Movement.SourceRef = "synthetic-fee-r1"
		feeDigest, err := cashmove.ReviewDigest(fee)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := commands.Accept(ctx, fee, feeDigest); err != nil {
			t.Fatal("fee command acceptance failed", err)
		}
		wait("Accounting did not book the real fee", func(check context.Context) bool {
			r, err := commands.Status(check, first.Tenant, first.Portfolio, fee.Movement.MovementID)
			return err == nil && r.Status == "journal_recorded"
		})
		t.Logf("booked fee %s command=%s", fee.Movement.MovementID, feeDigest)
		events, err := journal.Journal(ctx, first.Portfolio)
		if err != nil {
			t.Fatal("read booked valuation inputs", err)
		}
		marks := func(ref string, a, b dec.Exact, at, known time.Time) *returnbasis.PositionMarks {
			return &returnbasis.PositionMarks{
				Reference: ref, Effective: at, KnownAt: known,
				Prices: map[string]dec.Exact{"SYNTH-A": a, "SYNTH-B": b},
			}
		}
		captureNAV := func(ref string, marks *returnbasis.PositionMarks, want string, effectiveCut, known time.Time) *returnbasis.Close {
			t.Helper()
			book := ledger.ReplayAsOf(first.Portfolio, events, effectiveCut, known)
			prices := make(map[string]*big.Rat, len(marks.Prices))
			for id, price := range marks.Prices {
				prices[id], _ = price.Rat()
			}
			value, err := accounting.ComputeNAV(book, first.Currency, marks.Effective, prices)
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
		intraday.PositionBasis = &returnbasis.PositionBasis{
			Opening: marks("prices-open", "15", "8", start, start),
			AtFlow:  marks("prices-flow", "18", "7.6", movement.Effective, movement.Effective),
			Closing: marks("prices-close-r1", "21", "8.8", end, end),
		}
		intraday.Opening = captureNAV("nav-intraday-open", intraday.PositionBasis.Opening, "100", start, intraday.AsOf)
		intraday.Closing = captureNAV("nav-intraday-close", intraday.PositionBasis.Closing, "165", end, intraday.AsOf)
		split := returnbasis.FlowSplit{MovementID: movement.MovementID, CommandDigest: commandDigest, Effective: movement.Effective,
			SignedAmount: "40",
			Before:       captureNAV("nav-pre-flow", intraday.PositionBasis.AtFlow, "110", movement.Effective.Add(-time.Microsecond), intraday.AsOf),
			After:        captureNAV("nav-post-flow", intraday.PositionBasis.AtFlow, "150", movement.Effective, intraday.AsOf)}
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
		wrong = intraday
		futurePrices := *intraday.PositionBasis
		futurePrices.Closing = marks("prices-future", "21", "8.8", end, wrong.AsOf.Add(time.Hour))
		wrong.PositionBasis = &futurePrices
		if _, err := admit(wrong); err == nil {
			t.Fatal("future position marks became visible under an older knowledge horizon")
		}
		splitDigest := appendEdition(intraday)
		corrected := intraday
		corrected.PreviousDigest = splitDigest
		corrected.AsOf = time.Now().UTC().Truncate(time.Microsecond)
		restatedPrices := *intraday.PositionBasis
		restatedPrices.Closing = marks("prices-close-r2", "16", "6.2", end, corrected.AsOf)
		corrected.PositionBasis = &restatedPrices
		corrected.Closing = captureNAV("nav-intraday-restated", restatedPrices.Closing, "132", end, corrected.AsOf)
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
		missingNAVDigest := appendEdition(missing)

		missingMarks := indexCorrected
		missingMarks.PreviousDigest = missingNAVDigest
		missingMarks.AsOf = time.Now().UTC().Truncate(time.Microsecond)
		incompletePrices := restatedPrices
		incompletePrices.Closing = &returnbasis.PositionMarks{
			Reference: "prices-close-incomplete", Effective: end, KnownAt: missingMarks.AsOf,
			Prices: map[string]dec.Exact{"SYNTH-A": "16"},
		}
		missingMarks.PositionBasis = &incompletePrices
		missingMarksDigest := appendEdition(missingMarks)

		badMarks := indexCorrected
		badMarks.PreviousDigest = missingMarksDigest
		badMarks.AsOf = time.Now().UTC().Truncate(time.Microsecond)
		unreconciledPrices := restatedPrices
		unreconciledPrices.Closing = marks("prices-close-unreconciled", "16", "6.3", end, badMarks.AsOf)
		badMarks.PositionBasis = &unreconciledPrices
		badMarksDigest := appendEdition(badMarks)

		// Deliver ordinary fill FACTs through the real Accounting subscriber.
		// The sale precedes the external flow and the purchase follows it.
		fillClient, err := bus.DialNATS(ctx, bus.NATSConfig{URL: broker.ClientURL(), Name: "return-pilot-fills"})
		if err != nil {
			t.Fatal("connect synthetic fill producer")
		}
		defer func() { _ = fillClient.Close() }()
		producer, err := bus.NewProducer(fillClient, bus.ProducerConfig{Source: "oms", ProducerVersion: "pilot", Tenant: first.Tenant})
		if err != nil {
			t.Fatal("construct synthetic fill producer")
		}
		sell := publishReturnFill(t, ctx, producer, first, "sell-b", "SYNTH-B", orderpb.Side_SIDE_SELL, 2, "8.1", start.Add(2*time.Hour))
		buy := publishReturnFill(t, ctx, producer, first, "buy-a", "SYNTH-A", orderpb.Side_SIDE_BUY, 1, "20.2", movement.Effective.Add(2*time.Hour))
		wait("Accounting did not book both ordinary fill FACTs", func(check context.Context) bool {
			rows, err := journal.Journal(check, first.Portfolio)
			if err != nil {
				return false
			}
			var found int
			for _, entry := range rows {
				if entry.EntryID == sell.EntryID || entry.EntryID == buy.EntryID {
					found++
				}
			}
			if found != 2 {
				return false
			}
			events = rows
			return true
		})
		unsupported := indexCorrected
		unsupported.PreviousDigest = badMarksDigest
		unsupported.AsOf = time.Now().UTC().Truncate(time.Microsecond)
		tradedPrices := *intraday.PositionBasis
		tradedPrices.Closing = marks("prices-close-traded-r1", "21", "10", end, unsupported.AsOf)
		tradedFlow := split
		tradedFlow.Before = captureNAV("nav-pre-flow-traded", tradedPrices.AtFlow, "110", movement.Effective.Add(-time.Microsecond), unsupported.AsOf)
		tradedFlow.After = captureNAV("nav-post-flow-traded", tradedPrices.AtFlow, "150", movement.Effective, unsupported.AsOf)
		tradedFlows := *intraday.Flows
		tradedFlows.Split = &tradedFlow
		unsupported.Flows = &tradedFlows
		unsupported.PositionBasis = &tradedPrices
		unsupported.Closing = captureNAV("nav-close-traded-r1", tradedPrices.Closing, "166", end, unsupported.AsOf)
		staticDigest := appendEdition(unsupported)

		traded := unsupported
		traded.PreviousDigest = staticDigest
		traded.AsOf = time.Now().UTC().Truncate(time.Microsecond)
		coverage := returnbasis.TradeCoverage{Reference: "booked-trades-r1", Fills: []returnbasis.TradeReference{sell, buy}}
		tradedBasis := tradedPrices
		tradedBasis.TradeCoverage = &coverage
		traded.PositionBasis = &tradedBasis
		tradedDigest := appendEdition(traded)

		tradeRestated := traded
		tradeRestated.PreviousDigest = tradedDigest
		tradeRestated.AsOf = time.Now().UTC().Truncate(time.Microsecond)
		restatedTradeBasis := tradedBasis
		restatedTradeBasis.Closing = marks("prices-close-traded-r2", "20", "9", end, tradeRestated.AsOf)
		tradeRestated.PositionBasis = &restatedTradeBasis
		tradeRestated.Closing = captureNAV("nav-close-traded-r2", restatedTradeBasis.Closing, "158", end, tradeRestated.AsOf)
		lastDigest := appendEdition(tradeRestated)

		for _, test := range []struct {
			ref   string
			fills []returnbasis.TradeReference
		}{
			{"booked-trades-unknown", nil},
			{"booked-trades-incomplete", []returnbasis.TradeReference{sell}},
			{"booked-trades-wrong-time", []returnbasis.TradeReference{sell, {EntryID: buy.EntryID, Effective: movement.Effective.Add(-time.Hour)}}},
		} {
			next := tradeRestated
			next.PreviousDigest = lastDigest
			next.AsOf = time.Now().UTC().Truncate(time.Microsecond)
			badCoverage := restatedTradeBasis
			badCoverage.TradeCoverage = &returnbasis.TradeCoverage{Reference: test.ref, Fills: test.fills}
			next.PositionBasis = &badCoverage
			lastDigest = appendEdition(next)
		}
		unpricedTrade := tradeRestated
		unpricedTrade.PreviousDigest = lastDigest
		unpricedTrade.AsOf = time.Now().UTC().Truncate(time.Microsecond)
		unpricedBasis := restatedTradeBasis
		unpricedBasis.Closing = &returnbasis.PositionMarks{Reference: "prices-close-traded-incomplete", Effective: end,
			KnownAt: unpricedTrade.AsOf, Prices: map[string]dec.Exact{"SYNTH-A": "20"}}
		unpricedTrade.PositionBasis = &unpricedBasis
		lastDigest = appendEdition(unpricedTrade)
		t.Logf("ordinary booked fills sell=%s buy=%s", sell.EntryID, buy.EntryID)

		sectorReport := first
		sectorReport.SectorAttribution = true
		sectorReport.PreviousDigest = digests[2]
		sectorOpen, sectorClose := start.AddDate(0, 0, -1), start
		sectorReport.PositionBasis = &returnbasis.PositionBasis{
			Opening: marks("sector-prices-open", "15", "8", sectorOpen, sectorOpen),
			Closing: marks("sector-prices-close", "18", "8.5", sectorClose, sectorClose),
		}
		sectorIndex := benchmark
		sectorIndex.Start, sectorIndex.End, sectorIndex.KnownAt = sectorOpen, sectorClose, sectorClose.Add(time.Hour)
		sectorIndex.ClosingLevel = "265"
		composition := returnbasis.BenchmarkComposition{Reference: "sector-index-members-r1",
			Constituents: []returnbasis.BenchmarkMember{
				{InstrumentID: "SYNTH-A", Weight: "0.2", Return: "0.1"},
				{InstrumentID: "SYNTH-B", Weight: "0.5", Return: "0.02"},
				{InstrumentID: "SYNTH-C", Weight: "0.3", Return: "0.1"},
			}}
		sectorIndex.Composition = &composition
		sectorReport.Benchmark = signIndex(sectorIndex)
		firstHorizon, originalClasses := master.awaitEdition(t, wait, "GROWTH")
		sectorReport.AsOf = firstHorizon
		nextDate, err := master.source.At(ctx, sectorTenant, nextSectorDay, firstHorizon, sectorInstruments)
		if err != nil || len(nextDate) != 1 || nextDate[0].Code != "FINANCE" {
			t.Fatal("Datamaster did not isolate the second effective date")
		}
		sectorReport.Opening = captureNAV("sector-nav-open", sectorReport.PositionBasis.Opening, "100", sectorOpen, sectorReport.AsOf)
		sectorReport.Closing = captureNAV("sector-nav-close", sectorReport.PositionBasis.Closing, "114.5", sectorClose, sectorReport.AsOf)
		sectorDigest := appendEdition(sectorReport)
		classificationReferences[sectorDigest] = classReference(t, originalClasses)
		master.write(t, 2, "CURRENT")
		correctedHorizon, correctedClasses := master.awaitEdition(t, wait, "VALUE")
		if !correctedClasses[2].KnownAt.After(firstHorizon) {
			t.Fatal("Datamaster allowed the sector correction to leak into the original knowledge horizon")
		}
		sectorRestatement := sectorReport
		sectorRestatement.PreviousDigest = sectorDigest
		sectorRestatement.AsOf = correctedHorizon
		sectorRestatedDigest := appendEdition(sectorRestatement)
		classificationReferences[sectorRestatedDigest] = classReference(t, correctedClasses)
		master.write(t, 3, "CURRENT")
		unknownHorizon, _ := master.awaitEdition(t, wait, "")
		unknownSector := sectorRestatement
		unknownSector.PreviousDigest = sectorRestatedDigest
		unknownSector.AsOf = unknownHorizon
		sectorLastDigest := appendEdition(unknownSector)
		for _, comp := range []*returnbasis.BenchmarkComposition{
			nil,
			{Reference: "sector-index-incomplete", Constituents: composition.Constituents[:2]},
			{Reference: "sector-index-disagrees", Constituents: []returnbasis.BenchmarkMember{
				composition.Constituents[0], composition.Constituents[1],
				{InstrumentID: "SYNTH-C", Weight: "0.3", Return: "0.11"},
			}},
		} {
			next := sectorReport
			next.PreviousDigest = sectorLastDigest
			next.AsOf = time.Now().UTC().Truncate(time.Microsecond)
			index := sectorIndex
			index.Composition = comp
			next.Benchmark = signIndex(index)
			sectorLastDigest = appendEdition(next)
		}
		unsupportedSector := tradeRestated
		unsupportedSector.SectorAttribution = true
		unsupportedSector.PreviousDigest = lastDigest
		unsupportedSector.AsOf = time.Now().UTC().Truncate(time.Microsecond)
		lastDigest = appendEdition(unsupportedSector)
		master.verifyRetained(t, ctx, wait, firstHorizon, correctedHorizon, originalClasses, correctedClasses, nextDate)
		master.shutdown()
		wrong = sectorReport
		wrong.PreviousDigest = sectorLastDigest
		wrong.AsOf = time.Now().UTC().Truncate(time.Microsecond)
		if _, err := admit(wrong); err == nil {
			t.Fatal("unavailable Datamaster published a new classification result")
		}
		t.Log("Datamaster was stopped for the fresh Accounting replay")

		// A separate close has a subscription and a redemption. Each invested
		// subperiod grows by 10%, so (11/10)^3 - 1 is exactly 331/1000.
		multiDay := time.Date(2020, 2, 7, 0, 0, 0, 0, time.UTC)
		multiEnd := multiDay.AddDate(0, 0, 1)
		bookExternal := func(id string, kind cashmove.Kind, amount int64, hour int) returnbasis.FlowSplit {
			t.Helper()
			leg := cashmove.Command{Tenant: first.Tenant, Actor: command.Actor, Reason: "dated external capital",
				Movement: cashmove.CashMovement{MovementID: id, PortfolioID: first.Portfolio, Kind: kind, Currency: first.Currency,
					Amount: big.NewRat(amount, 1), SourceRef: "transfer-" + id, Effective: multiDay.Add(time.Duration(hour) * time.Hour)}}
			digest, err := cashmove.ReviewDigest(leg)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := commands.Accept(ctx, leg, digest); err != nil {
				t.Fatal("accept external flow", id, err)
			}
			wait("unbooked external flow "+id, func(check context.Context) bool {
				status, err := commands.Status(check, first.Tenant, first.Portfolio, id)
				return err == nil && status.Status == "journal_recorded"
			})
			if kind == cashmove.Redemption {
				amount = -amount
			}
			return returnbasis.FlowSplit{MovementID: id, CommandDigest: digest, Effective: leg.Movement.Effective, SignedAmount: dec.Exact(fmt.Sprint(amount))}
		}
		subscription := bookExternal("multi-subscription", cashmove.Subscription, 50, 10)
		redemption := bookExternal("multi-redemption", cashmove.Redemption, 40, 16)
		events, err = journal.Journal(ctx, first.Portfolio)
		if err != nil {
			t.Fatal("read multi-flow valuation journal", err)
		}
		multi := first
		multi.CloseDate = multiEnd.Format(time.DateOnly)
		multi.AsOf = time.Now().UTC().Truncate(time.Microsecond)
		nav := func(ref, total string, a dec.Exact, cut time.Time) *returnbasis.Close {
			t.Helper()
			return captureNAV(ref, marks("prices-"+ref, a, "8", cut, multi.AsOf), total, cut, multi.AsOf)
		}
		multi.Opening = nav("multi-open", "100", "9", multiDay)
		subscription.Before = nav("multi-before-1", "110", "11", subscription.Effective.Add(-time.Microsecond))
		subscription.After = nav("multi-after-1", "160", "11", subscription.Effective)
		redemption.Before = nav("multi-before-2", "176", "14.2", redemption.Effective.Add(-time.Microsecond))
		redemption.After = nav("multi-after-2", "136", "14.2", redemption.Effective)
		multi.Closing = nav("multi-close", "149.6", "16.92", multiEnd)
		two := uint64(2)
		multi.Flows = &returnbasis.FlowCompletion{Scope: first.Flows.Scope, Reference: "complete-multi-cash", EventCount: &two,
			Splits: []returnbasis.FlowSplit{subscription, redemption}}
		badSecond := redemption
		badSecond.CommandDigest = subscription.CommandDigest
		wrong = multi
		wrongFlows := *multi.Flows
		wrongFlows.Splits = []returnbasis.FlowSplit{subscription, badSecond}
		wrong.Flows = &wrongFlows
		if _, err := admit(wrong); err == nil {
			t.Fatal("second movement accepted another booked command's digest")
		}
		multiDigest := appendEdition(multi)
		report, err := store.ReadObserved(ctx, multi.Portfolio, multi.Currency, multi.CloseDate, multi.AsOf)
		if err != nil || report.GetMeasured() == nil || report.GetMeasured().GetReturnValue() != 0.331 || report.GetExternalFlowCount() != 2 ||
			report.GetIntradayFlow() != nil || len(report.GetIntradayFlows()) != 2 {
			t.Fatal("observed multi-flow TWR or flow coverage differs from 331/1000", report, err)
		}
		for i, leg := range multi.Flows.Splits {
			got := report.GetIntradayFlows()[i]
			amount, ok := dec.FromProtoChecked(got.GetSignedAmount())
			want, _ := leg.SignedAmount.Rat()
			if !ok || amount.Cmp(want) != 0 || got.GetMovementId() != leg.MovementID || got.GetCommandDigest() != leg.CommandDigest ||
				!got.GetEffectiveTime().AsTime().Equal(leg.Effective) || got.GetPreFlowNavReference() != leg.Before.Reference || got.GetPostFlowNavReference() != leg.After.Reference {
				t.Fatalf("published multi-flow leg %d differs from its booked command and signed cut", i)
			}
		}
		partialMulti := multi
		partialMulti.PreviousDigest = multiDigest
		partialMulti.AsOf = time.Now().UTC().Truncate(time.Microsecond)
		partialSecond := redemption
		partialSecond.Before = nil
		partialSet := *multi.Flows
		partialSet.Splits = []returnbasis.FlowSplit{subscription, partialSecond}
		partialMulti.Flows = &partialSet
		partialDigest := appendEdition(partialMulti)
		report, err = store.ReadObserved(ctx, multi.Portfolio, multi.Currency, multi.CloseDate, partialMulti.AsOf)
		if err != nil || report.GetMeasured() != nil || report.GetUnassessedReason() != "nav_at_flow_missing" || len(report.GetIntradayFlows()) != 2 ||
			report.GetIntradayFlows()[1].GetPreFlowNavReference() != "" {
			t.Fatal("missing second NAV produced a measurement or discarded its movement", report, err)
		}
		bookExternal("multi-late-third", cashmove.Subscription, 1, 19)
		omitted := multi
		omitted.PreviousDigest = partialDigest
		omitted.AsOf = time.Now().UTC().Truncate(time.Microsecond)
		if _, err := admit(omitted); err == nil {
			t.Fatal("multi-flow edition omitted a third independently booked movement")
		}
		if repeat, err := admit(multi); err != nil || repeat != multiDigest {
			t.Fatal("new booking altered the historical multi-flow edition", err)
		}

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
	wait("fresh durable did not acknowledge both ordinary fills", func(check context.Context) bool {
		info, err := other.Consumer(check, "return-replay-order_order_filled")
		if err != nil {
			return false
		}
		state, err := info.Info(check)
		return err == nil && state.AckFloor.Consumer >= 2 && state.NumAckPending == 0
	})
	pool, err := pg.NewTenantPool(ctx, runtimeDSN, first.Tenant)
	if err != nil {
		t.Fatal("reopen the journal after replay")
	}
	defer pool.Close()
	var trades int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ledger_entries
		WHERE portfolio_id=$1 AND entry_type=$2 AND entry_id LIKE 'fill:%'`, first.Portfolio, ledger.EntryTrade).Scan(&trades); err != nil || trades != 2 {
		t.Fatal("fill redelivery changed the booked execution count")
	}
	after, err := stream.Info(ctx)
	if err != nil || after.State.Msgs != before.State.Msgs {
		t.Fatal("replay produced another completion")
	}

	assertRetained()
	values := make([]map[string]any, len(editions))
	for i, e := range editions {
		values[i] = map[string]any{"as_of": e.AsOf.Format(time.RFC3339Nano), "digest": digests[i], "date": e.CloseDate}
		if reference := classificationReferences[digests[i]]; reference != "" {
			values[i]["classification_reference"] = reference
		}
		if e.Flows != nil && e.Flows.Split != nil {
			values[i]["movement_id"] = e.Flows.Split.MovementID
			values[i]["command_digest"] = e.Flows.Split.CommandDigest
			if e.Flows.Split.Before != nil {
				values[i]["pre_flow_nav"] = e.Flows.Split.Before.Reference
			}
			if e.Flows.Split.After != nil {
				values[i]["post_flow_nav"] = e.Flows.Split.After.Reference
			}
		}
		if e.Benchmark != nil && e.Benchmark.Observation != nil {
			raw, err := json.Marshal(e.Benchmark.Observation)
			if err != nil {
				t.Fatal(err)
			}
			values[i]["benchmark_digest"] = fmt.Sprintf("%x", sha256.Sum256(raw))
		}
		if e.Flows != nil && len(e.Flows.Splits) > 0 {
			var legs []map[string]string
			for _, leg := range e.Flows.Splits {
				out := map[string]string{"movement_id": leg.MovementID, "command_digest": leg.CommandDigest, "effective_time": leg.Effective.Format(time.RFC3339Nano),
					"signed_amount": string(leg.SignedAmount), "pre_flow_nav_reference": "", "post_flow_nav_reference": ""}
				if leg.Before != nil {
					out["pre_flow_nav_reference"] = leg.Before.Reference
				}
				if leg.After != nil {
					out["post_flow_nav_reference"] = leg.After.Reference
				}
				legs = append(legs, out)
			}
			values[i]["flow_legs"] = legs
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
		benchmarkDigest, _ := values[i]["benchmark_digest"].(string)
		t.Logf("edition close=%s horizon=%s source_sha256=%s benchmark_sha256=%s classification=%s", e.CloseDate, e.AsOf.Format(time.RFC3339Nano), digests[i], benchmarkDigest, classificationReferences[digests[i]])
	}
}

func publishReturnFill(t *testing.T, ctx context.Context, producer *bus.Producer, scope returnbasis.Evidence,
	id, instrument string, side orderpb.Side, quantity int64, price string, at time.Time) returnbasis.TradeReference {
	t.Helper()
	fill := &orderpb.Fill{
		FillId: "pilot-fill-" + id, OrderId: "pilot-order-" + id, InstrumentId: instrument,
		Venue: "BINANCE", VenueAccountId: "synthetic-spot", VenueExecutionId: "pilot-exec-" + id,
		Side: side, Quantity: dec.ToProto(big.NewRat(quantity, 1)), Price: dec.ToProto(dec.Rat(price)),
		ExecutedAt: timestamppb.New(at),
		Fee:        &commonpb.Money{CurrencyCode: scope.Currency, Amount: dec.ToProto(big.NewRat(1, 1))},
	}
	if err := producer.Publish(ctx, bus.Event{
		Subject: fillfact.SubjectFilled, EventType: fillfact.SubjectFilled, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion: 1, Domain: "order", EventTime: at, PartitionKey: fill.OrderId, PayloadSchemaRef: "order.v1.OrderFilled:1",
		Payload: &orderpb.OrderFilled{OrderId: fill.OrderId, Fill: fill, State: &orderpb.OrderState{PortfolioId: scope.Portfolio}},
	}); err != nil {
		t.Fatal("publish synthetic typed fill FACT", err)
	}
	return returnbasis.TradeReference{EntryID: "fill:" + fillfact.ExecutionKey(fill), Effective: at}
}
