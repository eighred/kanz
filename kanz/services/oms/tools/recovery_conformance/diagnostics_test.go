package recoveryconformance

import (
	"context"
	"errors"
	"fmt"
	"github.com/eighred/kanz/internal/dualcontrol"
	"github.com/eighred/kanz/internal/fillfact"
	"github.com/eighred/kanz/services/oms/internal/order"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/nats-io/nats.go"
	"google.golang.org/grpc/status"
	"testing"
)

// Closed categories only: never emit SQL, DSNs, remote reasons or payloads.
func goErrorCategory(err error) string {
	if err == nil {
		return "none"
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) && len(pg.Code) == 5 {
		valid := true
		for _, c := range pg.Code {
			if !(c >= '0' && c <= '9' || c >= 'A' && c <= 'Z') {
				valid = false
			}
		}
		if valid {
			return "SQLSTATE=" + pg.Code
		}
	}
	for _, v := range []struct {
		err  error
		name string
	}{{dualcontrol.ErrExpired, "ApprovalExpired"}, {dualcontrol.ErrSelfApproval, "SelfApproval"}, {order.ErrConflict, "Conflict"}, {order.ErrRecoveryEvidenceConflict, "RecoveryEvidenceConflict"}, {fillfact.ErrFeeRevision, "FeeRevision"}, {context.DeadlineExceeded, "Deadline"}, {context.Canceled, "Canceled"}, {nats.ErrMaxPayload, "BrokerMaxPayload"}, {order.ErrPostedExecutionUnknown, "PostedExecutionUnknown"}} {
		if errors.Is(err, v.err) {
			return v.name
		}
	}
	if _, ok := status.FromError(err); ok {
		return fmt.Sprintf("GRPC=%d", status.Code(err))
	}

	for current, depth := err, 0; current != nil && depth < 16; current, depth = errors.Unwrap(current), depth+1 {
		switch current.Error() {
		case "recovered execution exceeds payload bound":
			return "RecoveredPayloadBound"
		case "oms: invalid recovery history scope":
			return "HistoryScope"
		case "oms: recovery history exceeds storage bound":
			return "HistoryStorageBound"
		case "oms: recovery history has no matching proven account mapping":
			return "HistoryMapping"
		case "oms: invalid execution recovery observation":
			return "ObservationShape"
		}
	}
	if pgconn.Timeout(err) {
		return "DatabaseTimeout"
	}
	return "other"
}

// The canonical relay exposes this full-batch outcome as an unwrapped formatted
// error, not a sentinel. Match only this fixture key and the unchanged default
// batch; no arbitrary infrastructure or broker error is retryable here.
func isFullBatchStall(err error, key string) bool {
	return err != nil && err.Error() == fmt.Sprintf("outbox: %s still has records beyond the 256-record batch", key)
}

func TestRecoveryDrainRetryClassification(t *testing.T) {
	if goErrorCategory(errors.New("recovered execution exceeds payload bound")) != "RecoveredPayloadBound" {
		t.Fatal("known precommit bound not classified")
	}

	const key = "fixture-order"
	if !isFullBatchStall(fmt.Errorf("outbox: %s still has records beyond the 256-record batch", key), key) {
		t.Fatal("canonical full batch refused")
	}
	for _, err := range []error{nil, context.DeadlineExceeded, &pgconn.PgError{Code: "53200"}, errors.New("outbox: other still has records beyond the 256-record batch"), errors.New("outbox: fixture-order still has records beyond the 128-record batch"), fmt.Errorf("wrapped: %w", errors.New("outbox: fixture-order still has records beyond the 256-record batch"))} {
		if isFullBatchStall(err, key) {
			t.Fatal("unrelated failure accepted for retry")
		}
	}
}
