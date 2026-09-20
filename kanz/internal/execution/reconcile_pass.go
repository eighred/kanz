package execution

import (
	"fmt"
)

// ReconcilePassError distinguishes a partially checked batch from a complete
// observation. Causes preserve typed failures without putting order IDs in labels.
type ReconcilePassError struct {
	Total, Checked, Failed int
	Causes                 []error
}

func (e *ReconcilePassError) Error() string {
	return fmt.Sprintf("incomplete reconciliation: total=%d checked=%d failed=%d", e.Total, e.Checked, e.Failed)
}

func (e *ReconcilePassError) Unwrap() []error { return e.Causes }

func (e *ReconcilePassError) Result() error {
	if e.Failed == 0 && e.Checked == e.Total {
		return nil
	}
	return e
}

func (e *ReconcilePassError) Fail(err error) {
	e.Failed++
	e.Causes = append(e.Causes, err)
}

// OrderObservationError preserves the order that could not be checked for callers
// investigating a pass. Its identity is never a metric label or raw log message.
type OrderObservationError struct {
	OrderID string
	Cause   error
}

func (e *OrderObservationError) Error() string { return "order observation incomplete" }
func (e *OrderObservationError) Unwrap() error { return e.Cause }

func (e *ReconcilePassError) FailOrder(orderID string, err error) {
	e.Fail(&OrderObservationError{OrderID: orderID, Cause: err})
}
