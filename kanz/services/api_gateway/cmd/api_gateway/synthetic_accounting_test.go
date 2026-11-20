package main

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/fillfact"
	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/oms/tools/syntheticvenue"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
)

const syntheticCashSubject = "accounting.balance.portfolio"

func runSyntheticBook(t *testing.T, ctx context.Context, s syntheticSession, tenant, step, orderID string) {
	t.Helper()
	runSyntheticBookTrade(t, ctx, s, tenant, step, orderID, 1)
}

func upgradingBook(tenant string) bool {
	return tenant == "synthetic-funded-venue-nanos" && os.Getenv("SYNTHETIC_PRIOR_BOOK_BIN") != ""
}

func syntheticBookSubject(tenant string, sequence int) string {
	if os.Getenv("KANZ_SYNTHETIC_GO_FUNDED_REMAINDER") == "1" && tenant == fundedWorkingTenant && sequence == 2 {
		return fillfact.SubjectFilled
	}
	return fillfact.SubjectPartiallyFilled
}

func runSyntheticBookTrade(t *testing.T, ctx context.Context, s syntheticSession, tenant, step, orderID string, sequence int) {
	t.Helper()
	binary := os.Getenv("SYNTHETIC_BOOK_BIN")
	if (upgradingBook(tenant) && (step == "open" || step == "book")) || (tenant == "synthetic-cash-clock" && step == "open" && os.Getenv("SYNTHETIC_PRIOR_BOOK_BIN") != "") {
		binary = os.Getenv("SYNTHETIC_PRIOR_BOOK_BIN")
	}
	cmd := exec.CommandContext(ctx, binary, "-test.v", "-test.count=1", "-test.run=^TestSyntheticSettlementJournal$")
	cmd.Env = []string{"SYNTHETIC_BOOK_TENANT=" + tenant, "SYNTHETIC_BOOK_STEP=" + step, "SYNTHETIC_BOOK_ORDER=" + orderID, "SYNTHETIC_BOOK_URL=" + os.Getenv("SYNTHETIC_BOOK_URL"), "SYNTHETIC_BOOK_NATS=" + s.broker,
		"SYNTHETIC_BOOK_SUBJECT=" + syntheticBookSubject(tenant, sequence), "SYNTHETIC_BOOK_SEQUENCE=" + strconv.Itoa(sequence), "SYNTHETIC_BOOK_UPGRADE=" + strconv.FormatBool(upgradingBook(tenant))}
	data, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(data), "--- PASS: TestSyntheticSettlementJournal") || strings.Contains(string(data), "--- SKIP:") {
		t.Fatalf("accounting %s: %v\n%s", step, err, data)
	}
}

func prepareSyntheticAccounting(t *testing.T, ctx context.Context, s syntheticSession, tenant string) *envelopepb.EventFrame {
	t.Helper()
	stream, err := s.js.CreateStream(ctx, jetstream.StreamConfig{Name: "SYNTHETIC_CASH", Subjects: []string{syntheticCashSubject}, Storage: jetstream.FileStorage})
	if err != nil {
		t.Fatal(err)
	}
	runSyntheticBook(t, ctx, s, tenant, "open", "")
	msg, err := stream.GetLastMsgForSubject(ctx, syntheticCashSubject)
	if err != nil {
		t.Fatal(err)
	}
	var frame envelopepb.EventFrame
	var level accountingpb.PortfolioCashBalance
	if proto.Unmarshal(msg.Data, &frame) != nil || bus.Validate(frame.GetEnvelope()) != nil || proto.Unmarshal(frame.Payload, &level) != nil ||
		frame.GetEnvelope().GetSource() != "accounting" || frame.GetEnvelope().GetTenantId() != tenant || level.GetCashCommit().GetRevision() != 1 ||
		len(level.GetCashCommit().GetApplied()) != 0 || dec.FromProto(level.Total).RatString() != "1000" {
		t.Fatal("missing scoped opening USD balance")
	}
	return &frame
}

// Check the clock of the accounting announcement independently of the venue
// execution clock. No orders or reservations are present in this tenant.
func TestSyntheticAccountingCashClock(t *testing.T) {
	if os.Getenv("SYNTHETIC_BOOK_BIN") == "" {
		t.Skip("requires the isolated accounting and OMS databases")
	}
	const tenant = "synthetic-cash-clock"
	exerciseSyntheticSession(t, tenant, func(ctx context.Context, s syntheticSession) {
		frame := prepareSyntheticAccounting(t, ctx, s, tenant)
		var level accountingpb.PortfolioCashBalance
		if err := proto.Unmarshal(frame.Payload, &level); err != nil {
			t.Fatal(err)
		}
		if os.Getenv("SYNTHETIC_PRIOR_BOOK_BIN") != "" {
			recoverLegacyCashClock(t, ctx, tenant, frame)
			return
		}
		if level.GetAsOf().GetNanos()%1000 != 321 || !level.AsOf.AsTime().Equal(frame.Envelope.GetEventTime().AsTime()) {
			t.Fatal("cash cutoff did not retain the accounting clock's original 321ns remainder")
		}
		pool := syntheticPool(t, ctx, "CASH", tenant)
		apply := syntheticvenue.CapitalCashHandler(pool, tenant)
		for range 2 {
			if err := apply(bus.WithTenantID(ctx, tenant), frame.Envelope, frame.Payload); err != nil {
				t.Fatal("matching accounting cutoff refused", err)
			}
		}
		var quarantined bool
		if err := pool.QueryRow(ctx, `SELECT fault_digest IS NOT NULL FROM capital_source_health WHERE tenant_id=$1`, tenant).Scan(&quarantined); err != nil || quarantined {
			t.Fatal("exact cutoff quarantined a valid accounting source", err)
		}
		var observed int64
		if err := pool.QueryRow(ctx, `SELECT observed_at_ns FROM capital_balances WHERE portfolio_id='synthetic-portfolio' AND currency='USD' AND revision=1`).Scan(&observed); err != nil || observed != level.AsOf.AsTime().UnixNano() {
			t.Fatal("OMS retained a different cash cutoff", err)
		}
		t.Log("XSIM accounting cutoff difference=0s source_remainder_ns=321 cash_source_quarantined=false")
	})
	const mismatch = "synthetic-cash-clock-mismatch"
	exerciseSyntheticSession(t, mismatch, func(ctx context.Context, s syntheticSession) {
		frame := prepareSyntheticAccounting(t, ctx, s, mismatch)
		altered := proto.Clone(frame.Envelope).(*envelopepb.Envelope)
		altered.EventTime.Nanos--
		pool := syntheticPool(t, ctx, "CASH", mismatch)
		if err := syntheticvenue.CapitalCashHandler(pool, mismatch)(bus.WithTenantID(ctx, mismatch), altered, frame.Payload); err == nil {
			t.Fatal("contradictory cutoff accepted")
		}
		var quarantined bool
		if err := pool.QueryRow(ctx, `SELECT octet_length(fault_digest)=32 AND NOT EXISTS(SELECT FROM capital_balances) FROM capital_source_health WHERE tenant_id=$1`, mismatch).Scan(&quarantined); err != nil || !quarantined {
			t.Fatal("mismatched cash cutoff must quarantine before creating a balance", err)
		}
	})
}

func bookSyntheticFill(t *testing.T, ctx context.Context, s syntheticSession, tenant, orderID string) []*envelopepb.EventFrame {
	t.Helper()
	return bookSyntheticTrade(t, ctx, s, tenant, orderID, 1)
}

func bookSyntheticTrade(t *testing.T, ctx context.Context, s syntheticSession, tenant, orderID string, sequence int) []*envelopepb.EventFrame {
	t.Helper()
	facts, err := s.js.Stream(ctx, "SYNTHETIC_FACTS")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := facts.GetLastMsgForSubject(ctx, syntheticBookSubject(tenant, sequence))
	if err != nil {
		t.Fatal(err)
	}
	var fill envelopepb.EventFrame
	if proto.Unmarshal(raw.Data, &fill) != nil || fill.GetEnvelope().GetPartitionKey() != orderID {
		t.Fatal("missing relayed fill")
	}
	consumer, err := s.js.CreateOrUpdateConsumer(ctx, "SYNTHETIC_CASH", jetstream.ConsumerConfig{Durable: "booked-cash", DeliverPolicy: jetstream.DeliverNewPolicy, AckPolicy: jetstream.AckExplicitPolicy})
	if err != nil {
		t.Fatal(err)
	}
	var frames []*envelopepb.EventFrame
	for i, step := range []string{"book", "replay"} {
		runSyntheticBookTrade(t, ctx, s, tenant, step, orderID, sequence)
		msg, err := consumer.Next(jetstream.FetchMaxWait(3 * time.Second))
		if err != nil {
			t.Fatal(err)
		}
		var frame envelopepb.EventFrame
		var level accountingpb.PortfolioCashBalance
		if proto.Unmarshal(msg.Data(), &frame) != nil || bus.Validate(frame.GetEnvelope()) != nil || proto.Unmarshal(frame.Payload, &level) != nil {
			t.Fatal("invalid cash FACT")
		}
		env, commit := frame.GetEnvelope(), level.GetCashCommit()
		if env.GetSource() != "accounting" || env.GetTenantId() != tenant || env.GetPartitionKey() != "synthetic-portfolio" || env.GetEventType() != syntheticCashSubject ||
			env.GetCausationId() != fill.GetEnvelope().GetEventId() || env.GetCorrelationId() != fill.GetEnvelope().GetCorrelationId() ||
			!env.GetEventTime().AsTime().Equal(level.GetAsOf().AsTime()) || level.GetBaseCurrency() != "USD" || dec.FromProto(level.GetTotal()).RatString() != strconv.Itoa(1000-126*sequence) || commit.GetRevision() != int64(2*sequence+i) ||
			len(level.ExcludedCurrencies) != 1 || level.ExcludedCurrencies[0] != "BNB" {
			t.Fatalf("cash step=%s source=%q tenant=%q partition=%q type=%q cause matches=%v correlation matches=%v cutoff matches=%v currency=%q total=%s revision=%d complete=%v excluded=%v", step, env.GetSource(), env.GetTenantId(), env.GetPartitionKey(), env.GetEventType(), env.GetCausationId() == fill.GetEnvelope().GetEventId(), env.GetCorrelationId() == fill.GetEnvelope().GetCorrelationId(), env.GetEventTime().AsTime().Equal(level.GetAsOf().AsTime()), level.GetBaseCurrency(), dec.FromProto(level.GetTotal()).RatString(), commit.GetRevision(), commit.GetComplete(), level.ExcludedCurrencies)
		}
		old := upgradingBook(tenant)
		if !(old && i == 0) && level.GetAsOf().GetNanos()%1000 != 321 {
			t.Fatal("cash FACT lost the accounting clock precision")
		}
		if old && i == 0 {
			if commit.GetComplete() || commit.GetUnattributedEntries() != 1 || len(commit.GetApplied()) != 0 {
				t.Fatal("legacy writer unexpectedly attributed submicrosecond fill")
			}
		} else if !commit.GetComplete() || commit.GetUnattributedEntries() != 0 {
			t.Fatal("cash acknowledgement did not attribute the exact execution")
		} else if (i == 0 && !old) || (i == 1 && old) {
			// Accounting publishes the changed order's cumulative debit. Only the
			// remainder profile books both executions against the same order.
			expectedDebit := "126"
			if syntheticBookSubject(tenant, sequence) == fillfact.SubjectFilled {
				expectedDebit = "252"
			}
			if len(commit.Applied) != 1 || commit.Applied[0].OrderId != orderID || dec.FromProto(commit.Applied[0].Debit).RatString() != expectedDebit {
				t.Fatal("book did not attribute the exact USD debit")
			}
		} else if len(commit.Applied) != 0 {
			t.Fatal("replayed fill produced a second debit")
		}
		frames = append(frames, &frame)
		if err := msg.DoubleAck(ctx); err != nil {
			t.Fatal(err)
		}
	}
	return frames
}
