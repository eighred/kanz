package main

import (
	"context"
	"os"
	"os/exec"
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
	cmd := exec.CommandContext(ctx, os.Getenv("SYNTHETIC_BOOK_BIN"), "-test.v", "-test.count=1", "-test.run=^TestSyntheticSettlementJournal$")
	cmd.Env = []string{"SYNTHETIC_BOOK_TENANT=" + tenant, "SYNTHETIC_BOOK_STEP=" + step, "SYNTHETIC_BOOK_ORDER=" + orderID, "SYNTHETIC_BOOK_URL=" + os.Getenv("SYNTHETIC_BOOK_URL"), "SYNTHETIC_BOOK_NATS=" + s.broker}
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
		loss := level.GetAsOf().AsTime().Sub(frame.Envelope.GetEventTime().AsTime())
		pool := syntheticPool(t, ctx, "CASH", tenant)
		apply := syntheticvenue.CapitalCashHandler(pool, tenant)
		err := apply(bus.WithTenantID(ctx, tenant), frame.Envelope, frame.Payload)
		if loss == 0 {
			if err != nil {
				t.Fatal("matching accounting cutoff refused", err)
			}
			return
		}
		if loss != 321*time.Nanosecond || err == nil {
			t.Fatalf("unexpected accounting cutoff difference %s or result %v", loss, err)
		}
		var quarantined bool
		if err := pool.QueryRow(ctx, `SELECT octet_length(fault_digest)=32
			AND NOT EXISTS(SELECT FROM capital_balances) FROM capital_source_health WHERE tenant_id=$1`, tenant).Scan(&quarantined); err != nil || !quarantined {
			t.Fatal("mismatched cash cutoff must quarantine before creating a balance", err)
		}
		t.Logf("XSIM accounting cutoff difference=%s cash_source_quarantined=true", loss)
	})
}

func bookSyntheticFill(t *testing.T, ctx context.Context, s syntheticSession, tenant, orderID string) []*envelopepb.EventFrame {
	t.Helper()
	facts, err := s.js.Stream(ctx, "SYNTHETIC_FACTS")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := facts.GetLastMsgForSubject(ctx, fillfact.SubjectPartiallyFilled)
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
		runSyntheticBook(t, ctx, s, tenant, step, orderID)
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
			!env.GetEventTime().AsTime().Equal(level.GetAsOf().AsTime()) || level.GetBaseCurrency() != "USD" || dec.FromProto(level.GetTotal()).RatString() != "874" || commit.GetRevision() != int64(i+2) ||
			len(level.ExcludedCurrencies) != 1 || level.ExcludedCurrencies[0] != "BNB" {
			t.Fatalf("cash step=%s source=%q tenant=%q partition=%q type=%q cause matches=%v correlation matches=%v cutoff matches=%v currency=%q total=%s revision=%d complete=%v excluded=%v", step, env.GetSource(), env.GetTenantId(), env.GetPartitionKey(), env.GetEventType(), env.GetCausationId() == fill.GetEnvelope().GetEventId(), env.GetCorrelationId() == fill.GetEnvelope().GetCorrelationId(), env.GetEventTime().AsTime().Equal(level.GetAsOf().AsTime()), level.GetBaseCurrency(), dec.FromProto(level.GetTotal()).RatString(), commit.GetRevision(), commit.GetComplete(), level.ExcludedCurrencies)
		}
		if !commit.GetComplete() {
			if tenant != "synthetic-funded-venue-nanos" || commit.GetUnattributedEntries() != 1 || len(commit.GetApplied()) != 0 {
				t.Fatal("incomplete execution attribution must retain the debit")
			}
		} else if commit.GetUnattributedEntries() != 0 {
			t.Fatal("complete cash acknowledgement has an unattributed entry")
		} else if i == 0 {
			if len(commit.Applied) != 1 || commit.Applied[0].OrderId != orderID || dec.FromProto(commit.Applied[0].Debit).RatString() != "126" {
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
