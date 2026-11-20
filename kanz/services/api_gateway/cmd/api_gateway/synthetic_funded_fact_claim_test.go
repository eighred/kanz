package main

import (
	"context"
	"errors"
	"testing"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

// Exercise the runtime's real outbox INSERT privilege at the database boundary.
// Before withdrawal it has no receipt to claim. After withdrawal even a byte-
// identical copy of the durable FACT cannot consume its outbox slot again.
func checkFundedTerminalFactClaim(t *testing.T, ctx context.Context, proof *pgxpool.Pool, tenant, orderID string, committed bool) {
	t.Helper()
	const facts = " FROM outbox WHERE partition_key=$1 AND subject='order.order.cancelled'"
	payload, err := proto.Marshal(&orderpb.OrderCancelled{OrderId: orderID})
	if err != nil {
		t.Fatal(err)
	}
	at, correlation, cause, trace := time.Now(), "pending-submit", "pending-cancel", ""
	want := 0
	if committed {
		want = 1
		if err = proof.QueryRow(ctx, "SELECT event_time,correlation_id,causation_id,trace_context,payload"+facts, orderID).
			Scan(&at, &correlation, &cause, &trace, &payload); err != nil {
			t.Fatal(err)
		}
	}
	writer := syntheticPool(t, ctx, "RUNTIME", tenant)
	_, err = writer.Exec(ctx, `INSERT INTO outbox(producer_service,envelope_tenant_id,partition_key,subject,event_type,event_class,
		schema_version,domain,payload_schema_ref,event_time,correlation_id,causation_id,trace_context,payload)
		VALUES('oms',$1,$2,'order.order.cancelled','order.order.cancelled',1,1,'order','order.v1.OrderCancelled:1',$3,$4,$5,$6,$7)`,
		tenant, orderID, at, correlation, cause, trace, payload)
	var refused *pgconn.PgError
	if !errors.As(err, &refused) || refused.Code != "23514" || refused.SchemaName != "public" ||
		refused.TableName != "outbox" || refused.ConstraintName != "cancellation_fact_unclaimed" {
		t.Fatalf("wanted a funded FACT without a pending receipt to be refused, got %v", err)
	}
	var count int
	if err = proof.QueryRow(ctx, "SELECT count(*)"+facts, orderID).Scan(&count); err != nil || count != want {
		t.Fatalf("funded terminal FACT count = %d after refused insertion, want %d: %v", count, want, err)
	}
}

// Copy the actual committed economic FACT. Neither write login may append a
// second accounting input, including when the requested partition is different.
func checkFundedExecutionFactClaims(t *testing.T, ctx context.Context, proof *pgxpool.Pool, tenant, orderID string) {
	t.Helper()
	for _, subject := range []string{"order.order.routed", "order.order.partially_filled"} {
		var at time.Time
		var schema, root, cause, trace string
		var payload []byte
		const facts = " FROM outbox WHERE subject=$1 AND partition_key=$2"
		if err := proof.QueryRow(ctx, "SELECT event_time,payload_schema_ref,correlation_id,causation_id,trace_context,payload"+facts, subject, orderID).
			Scan(&at, &schema, &root, &cause, &trace, &payload); err != nil {
			t.Fatal(err)
		}
		for _, role := range []string{"RUNTIME", "ADAPTER"} {
			writer := syntheticPool(t, ctx, role, tenant)
			for _, key := range []string{orderID, "unrelated-order"} {
				_, err := writer.Exec(ctx, `INSERT INTO outbox(producer_service,envelope_tenant_id,partition_key,subject,event_type,event_class,
				schema_version,domain,payload_schema_ref,event_time,correlation_id,causation_id,trace_context,payload)
				VALUES('oms',$1,$2,$3,$3,1,1,'order',$4,$5,$6,$7,$8,$9)`,
					tenant, key, subject, schema, at, root, cause, trace, payload)
				var denied *pgconn.PgError
				if !errors.As(err, &denied) || denied.Code != "23514" || denied.SchemaName != "public" ||
					denied.TableName != "outbox" || denied.ConstraintName != "settlement_fact_unapproved" {
					t.Fatalf("%s direct %s FACT: %v", role, subject, err)
				}
			}
		}
		var count int
		if err := proof.QueryRow(ctx, "SELECT count(*)"+facts, subject, orderID).Scan(&count); err != nil || count != 1 {
			t.Fatalf("retained %s FACT count=%d, want 1: %v", subject, count, err)
		}
	}
}
