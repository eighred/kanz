package recoveryconformance

import (
	"context"
	"errors"
	"fmt"
	"github.com/eighred/kanz/internal/dualcontrol"
	"github.com/eighred/kanz/internal/fillfact"
	"github.com/eighred/kanz/services/oms/internal/order"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/grpc/status"
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
	}{{dualcontrol.ErrExpired, "ApprovalExpired"}, {dualcontrol.ErrSelfApproval, "SelfApproval"}, {order.ErrConflict, "Conflict"}, {order.ErrRecoveryEvidenceConflict, "RecoveryEvidenceConflict"}, {fillfact.ErrFeeRevision, "FeeRevision"}, {context.DeadlineExceeded, "Deadline"}, {context.Canceled, "Canceled"}} {
		if errors.Is(err, v.err) {
			return v.name
		}
	}
	if _, ok := status.FromError(err); ok {
		return fmt.Sprintf("GRPC=%d", status.Code(err))
	}
	return "other"
}
