package riskdedupproof

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"

	"github.com/eighred/kanz/internal/pg"
	"github.com/eighred/kanz/internal/platform/subject"
	"github.com/eighred/kanz/internal/risk/publish"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
)

const natsFixture = "nats://127.0.0.1:4222"

// TestLiveRiskDedupPhase runs one step inside the fixture's private loopback
// namespace. The parent starts and stops the selected risk images and Valkey.
// An ordinary test run has no authority to publish or read fixture state.
func TestLiveRiskDedupPhase(t *testing.T) {
	phase := os.Getenv("KANZ_RISK_DEDUP_PROOF_PHASE")
	if phase == "" {
		t.Skip("requires the disposable selected-image risk fixture")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	nc, err := nats.Connect(natsFixture, nats.NoReconnect(), nats.Timeout(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := nc.JetStream(nats.MaxWait(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	groups := []string{"proof-a", "proof-b"}
	var pos int
	switch phase {
	case "nonowner":
		groups = groups[:1]
	case "owner", "duplicates", "checkpoint", "restart":
	case "outage":
		pos = 1
	case "recovery", "recovered-duplicates", "final-checkpoint":
		pos = 2
	default:
		t.Fatal("unknown proof phase")
	}
	o := positions[pos]
	input := o.event()
	if phase == "nonowner" || phase == "outage" || phase == "recovery" {
		client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: natsFixture, DisableReconnect: true})
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "proof-publisher", ProducerVersion: "fixture", Tenant: tenant})
		if err != nil {
			t.Fatal(err)
		}
		if err := producer.Publish(ctx, input); err != nil {
			t.Fatal(err)
		}
	}
	retained, err := js.GetLastMsg("POSITION", input.Subject, nats.Context(ctx))
	if err != nil {
		t.Fatal(err)
	}
	frame, payload, err := bus.Unframe(retained.Data)
	if err != nil {
		t.Fatal(err)
	}
	var state domainpb.PositionState
	if bus.Validate(frame) != nil || frame.GetEventId() != o.id || frame.GetTenantId() != tenant || frame.GetPartitionKey() != portfolio ||
		frame.GetEventClass() != input.EventClass || frame.GetEventType() != input.EventType || proto.Unmarshal(payload, &state) != nil || !proto.Equal(&state, input.Payload) {
		t.Fatal("retained position does not match this phase's exact canonical input")
	}
	waitAck(t, ctx, js, retained.Sequence, groups)
	if phase == "nonowner" {
		_, err := js.GetLastMsg("RISK", publish.EventTypeExposureRecomputed, nats.Context(ctx))
		if !errors.Is(err, nats.ErrMsgNotFound) {
			t.Fatalf("expected empty exposure subject after non-owner ACK, got %v", err)
		}
		return
	}
	if phase == "duplicates" || phase == "recovered-duplicates" || phase == "outage" {
		baseline := make(map[string]counters, len(groups))
		for _, group := range groups {
			baseline[group] = replicaCounters(t, ctx, group)
		}
		previous := retained.Sequence
		for range 3 {
			// Retain the exact EventFrame and its identity. Omitting the broker
			// message ID admits a distinct stream sequence for each delivery.
			// Wait per copy because POSITION compacts per holding.
			ack, err := js.Publish(input.Subject, retained.Data, nats.Context(ctx))
			if err != nil {
				t.Fatal(err)
			}
			if ack.Stream != "POSITION" || ack.Duplicate || ack.Sequence != previous+1 {
				t.Fatal("broker did not store a distinct duplicate frame")
			}
			waitAck(t, ctx, js, ack.Sequence, groups)
			previous = ack.Sequence
		}
		for _, group := range groups {
			if err := checkBurst(baseline[group], replicaCounters(t, ctx, group), phase == "outage"); err != nil {
				t.Fatalf("%s: %v", group, err)
			}
		}
	}
	eventually(t, ctx, func() error {
		msg, err := js.GetLastMsg("RISK", publish.EventTypeExposureRecomputed, nats.Context(ctx))
		if err != nil {
			return err
		}
		env, payload, err := bus.Unframe(msg.Data)
		if err != nil {
			return err
		}
		if bus.Validate(env) != nil || env.GetTenantId() != tenant || env.GetPartitionKey() != portfolio || env.GetSource() != "proof-b" ||
			env.GetEventType() != publish.EventTypeExposureRecomputed || env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_FACT ||
			env.GetPayloadSchemaRef() != "domain.v1.ExposureSet:1" {
			return fmt.Errorf("exposure FACT authority or payload identity mismatch")
		}
		var set domainpb.ExposureSet
		if err := proto.Unmarshal(payload, &set); err != nil {
			return err
		}
		return o.checkExposure(&set)
	})
	if phase == "checkpoint" || phase == "final-checkpoint" || phase == "restart" {
		checkPersisted(t, ctx, pos)
	}
}

func waitAck(t *testing.T, ctx context.Context, js nats.JetStreamContext, seq uint64, replicas []string) {
	t.Helper()
	for _, replica := range replicas {
		name := "risk-engine-" + replica + "-risk_position_changed__"
		eventually(t, ctx, func() error {
			info, err := js.ConsumerInfo("POSITION", name, nats.Context(ctx))
			if err != nil {
				return err
			}
			if info.Config.FilterSubject != subject.PositionAll || info.AckFloor.Stream != seq || info.NumAckPending != 0 || info.NumPending != 0 {
				return fmt.Errorf("durable %s has not acknowledged sequence %d", name, seq)
			}
			return nil
		})
	}
}

func eventually(t *testing.T, ctx context.Context, check func() error) {
	t.Helper()
	for {
		err := check()
		if err == nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("fixture expectation timed out: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func checkPersisted(t *testing.T, ctx context.Context, pos int) {
	t.Helper()
	// Fixed fixture role and database. The private loopback PG fixture accepts
	// the role locally. Production settings and DSNs have no input here.
	pool, err := pg.NewTenantPool(ctx, "postgres://risk_proof@127.0.0.1:5432/risk_proof?sslmode=disable", tenant)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var allowed bool
	if err := pool.QueryRow(ctx, "SELECT current_user = 'risk_proof' AND NOT rolsuper AND NOT rolbypassrls AND current_setting('row_security') = 'on' AND app_current_tenant() = $1 FROM pg_roles WHERE rolname = current_user", tenant).Scan(&allowed); err != nil || !allowed {
		t.Fatalf("fixture role or tenant scope rejected: %v", err)
	}
	var qty, price, value []byte
	var asOf time.Time
	err = pool.QueryRow(ctx, "SELECT quantity, average_price, market_value, as_of FROM positions WHERE portfolio_id=$1 AND instrument_id=$2", portfolio, instrument).Scan(&qty, &price, &value, &asOf)
	if err != nil {
		t.Fatal(err)
	}
	var q, p commonpb.Decimal
	var m commonpb.Money
	o := positions[pos]
	if proto.Unmarshal(qty, &q) != nil || proto.Unmarshal(price, &p) != nil || proto.Unmarshal(value, &m) != nil ||
		!proto.Equal(&q, &commonpb.Decimal{Coefficient: o.quantity}) || !proto.Equal(&p, cents(o.price)) || !matchesUSD(&m, o.marketValue) || !asOf.Equal(o.at) {
		t.Fatal("persisted position differs from the canonical input")
	}
	keys, err := pool.Query(ctx, "SELECT idempotency_key FROM applied_keys WHERE portfolio_id=$1 ORDER BY idempotency_key", portfolio)
	if err != nil {
		t.Fatal(err)
	}
	defer keys.Close()
	count := 0
	for keys.Next() {
		var key string
		if err := keys.Scan(&key); err != nil || count > pos || key != positions[count].id {
			t.Fatal("unexpected persisted idempotency key")
		}
		count++
	}
	if err := keys.Err(); err != nil || count != pos+1 {
		t.Fatalf("persisted idempotency key count %d: %v", count, err)
	}
}
