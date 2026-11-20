package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"testing"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/protobuf/proto"
)

// This order deliberately cannot transfer. Give this check its own database
// in generation one; do not remove a committed order to enable a later switch.
func TestSyntheticGoWriterLineageHandoff(t *testing.T) {
	if os.Getenv("KANZ_SYNTHETIC_GO_LINEAGE") != "1" {
		t.Skip("requires a dedicated synthetic generation-one database")
	}
	const tenant, orderID = "synthetic-go-handoff", "0f36dca681444918a676761d47b61533"
	ctx := t.Context()
	runtime := syntheticPool(t, ctx, "RUNTIME", tenant)
	operator := syntheticPool(t, ctx, "SWITCH", tenant)
	relay := syntheticPool(t, ctx, "RELAY", tenant)
	// Positive control on precisely the same logins and schema before admission.
	// Rolling back the rehearsal leaves generation one active for the Go store.
	rehearsal, err := operator.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rehearsal.Rollback(ctx)
	var next int64
	if err := rehearsal.QueryRow(ctx, "SELECT synthetic_test_handoff()").Scan(&next); err != nil || next != 2 {
		t.Fatalf("empty estate handoff returned epoch %d: %v", next, err)
	}
	if err := rehearsal.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	child := exec.CommandContext(ctx, "/fixture/order", "-test.v", "-test.run=^TestSyntheticGoRetainedAdmission$")
	child.Env = []string{"KANZ_SYNTHETIC_GO_LINEAGE=1", "SYNTHETIC_OMS_RUNTIME_URL=" + os.Getenv("SYNTHETIC_OMS_RUNTIME_URL")}
	log, err := child.CombinedOutput()
	if err != nil || bytes.Count(log, []byte("--- PASS: TestSyntheticGoRetainedAdmission ")) != 1 {
		t.Fatalf("Go admission probe: %v\n%s", err, log)
	}

	// Snapshot the actual stored bytes and full pending outbox record, including
	// ID, event time, payload, causal lineage and publication attempts.
	read := func() ([]byte, string) {
		t.Helper()
		var state, payload []byte
		var version, commands, observations int
		err := runtime.QueryRow(ctx, `SELECT state,version,(SELECT count(*) FROM synthetic_oms_commands),
			(SELECT count(*) FROM synthetic_oms_observations) FROM orders WHERE order_id=$1`, orderID).Scan(&state, &version, &commands, &observations)
		if err != nil || version != 0 || commands != 0 || observations != 0 {
			t.Fatalf("retained version=%d commands=%d observations=%d: %v", version, commands, observations, err)
		}
		var row string
		err = relay.QueryRow(ctx, `SELECT to_jsonb(o)::text,payload FROM outbox o WHERE partition_key=$1 AND
			producer_service='oms' AND subject='order.order.accepted' AND causation_id='go-retained-command'
			AND published_at IS NULL AND attempts=0 AND (SELECT count(*) FROM outbox)=1`, orderID).Scan(&row, &payload)
		var order orderpb.OrderState
		var fact orderpb.OrderAccepted
		if err != nil || proto.Unmarshal(state, &order) != nil || proto.Unmarshal(payload, &fact) != nil ||
			order.OrderId != orderID || order.Status != orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW ||
			order.AcceptedAnnouncedAt == nil || order.Quarantine != nil || order.VenueAckAt != nil ||
			fact.OrderId != orderID || !proto.Equal(fact.State, &order) {
			t.Fatalf("pending exact Go acceptance: %v", err)
		}
		return state, row
	}
	state, pending := read()
	for attempt := 0; attempt < 2; attempt++ {
		var pgerr *pgconn.PgError
		_, err := operator.Exec(ctx, "SELECT synthetic_test_handoff()")
		if !errors.As(err, &pgerr) || pgerr.Code != "P1533" || pgerr.Message != "unowned retained economic state" {
			t.Fatalf("unexpected transfer result: %v", err)
		}
		var epoch, generations int
		if err = runtime.QueryRow(ctx, "SELECT synthetic_oms_assert_writer()").Scan(&epoch); err != nil || epoch != 1 {
			t.Fatalf("active epoch %d: %v", epoch, err)
		}
		if err = operator.QueryRow(ctx, "SELECT count(*) FROM synthetic_oms_writer_history").Scan(&generations); err != nil || generations != 1 {
			t.Fatalf("writer generations %d: %v", generations, err)
		}
		get := nativeSubmit{Version: 1, Op: "get", Tenant: tenant, OrderID: orderID, Portfolio: "synthetic-portfolio", Principal: "user:go-owner"}
		for _, candidate := range []struct{ role, code string }{{"RUNTIME", "NOT_FOUND"}, {"NEXT_RUNTIME", "WRITER_FENCED"}} {
			r, err := callSyntheticOwner(ctx, os.Getenv("SYNTHETIC_OMS_"+candidate.role+"_URL"), get)
			if err != nil || r.OK || r.Code != candidate.code || len(r.State) != 0 || r.OrderVersion != 0 {
				t.Fatalf("%s lookup returned %s: %v", candidate.role, r.Code, err)
			}
		}
		after, queued := read()
		if !bytes.Equal(state, after) || queued != pending {
			t.Fatal("refused transfer changed the Go state or its pending acceptance")
		}
	}
	t.Log("Go retained v0 and its sole pending acceptance across two P1533 refusals; current Rust lookup NOT_FOUND and successor WRITER_FENCED")
}
