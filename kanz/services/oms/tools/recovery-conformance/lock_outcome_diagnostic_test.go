package recoveryconformance

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/eighred/kanz/internal/dec"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"
)

// Failure-only, one-statement snapshot of this disposable tenant/order/case.
// The original fixture context bounds observation; no new lifetime or retry.
// These are diagnostic witnesses, not full book/identity or terminal proofs.
// In particular, the pre-close snapshot cannot establish terminal rollback.
// Bookkeeping can precede the financial transaction and is not erased here.
type lockOutcomeDiagnostic struct {
	countsObserved                       bool
	known, initialState, committedState  bool
	version, caseVersion                 int64
	status                               int32
	caseStatus                           string
	checkpoint, expected                 int64
	fills, executions, targets           int64
	outbox, recovered, pending, attempts int64
	acks, fees                           int64
}

func observeLockOutcome(f *fixture, initial *orderpb.OrderState, caseID string) lockOutcomeDiagnostic {
	d := lockOutcomeDiagnostic{caseStatus: "UNKNOWN"}
	var raw []byte
	// CASE bounds transfer before protobuf decoding. Scalar subqueries share
	// the same PostgreSQL statement snapshot; tenant RLS remains on f.pool.
	err := f.pool.QueryRow(f.ctx, `SELECT
 CASE WHEN octet_length(o.state)<=1048576 THEN o.state ELSE NULL END,o.version,
 COALESCE(c.version,0),COALESCE(c.checkpoint,0),COALESCE(c.expected_ack_count,0),
 CASE WHEN c.case_id IS NULL THEN 'absent'
 WHEN c.status IN ('observed','investigating','corrected','blocked') THEN c.status ELSE 'UNKNOWN' END,
 (SELECT count(*) FROM order_fills WHERE order_id=$1),
 (SELECT count(*) FROM order_executions WHERE order_id=$1),
 (SELECT count(*) FROM execution_recovery_targets WHERE case_id=$2),
 (SELECT count(*) FROM outbox WHERE partition_key=$1),
 (SELECT count(*) FROM outbox WHERE partition_key=$1 AND event_type='order.order.execution_recovered'),
 (SELECT count(*) FROM outbox WHERE partition_key=$1 AND event_type='order.order.execution_recovered' AND published_at IS NULL),
 (SELECT count(*) FROM outbox WHERE partition_key=$1 AND event_type='order.order.execution_recovered' AND attempts<>0),
 (SELECT count(*) FROM execution_recovery_acks WHERE case_id=$2),
 (SELECT count(*) FROM execution_fee_revisions WHERE order_id=$1)
 FROM orders o LEFT JOIN execution_recovery_cases c ON c.case_id=$2 AND c.order_id=o.order_id
 WHERE o.order_id=$1`, initial.OrderId, caseID).Scan(&raw, &d.version,
		&d.caseVersion, &d.checkpoint, &d.expected, &d.caseStatus,
		&d.fills, &d.executions, &d.targets, &d.outbox, &d.recovered, &d.pending, &d.attempts, &d.acks, &d.fees)
	if err != nil {
		return lockOutcomeDiagnostic{caseStatus: "UNKNOWN"}
	}
	d.countsObserved = true
	if f.ctx.Err() != nil {
		return d
	}
	return d.observeState(raw, initial)
}

func (d lockOutcomeDiagnostic) observeState(raw []byte, initial *orderpb.OrderState) lockOutcomeDiagnostic {
	d.known, d.initialState, d.committedState = false, false, false
	d.status = 0
	if len(raw) == 0 || len(raw) > 1<<20 {
		return d
	}
	var state orderpb.OrderState
	if proto.Unmarshal(raw, &state) != nil || state.OrderId != initial.OrderId {
		return d
	}
	if _, ok := orderpb.OrderStatus_name[int32(state.Status)]; !ok {
		return d
	}
	// Diagnostic-only canonical domain admission precedes every dec.Cmp.
	// InDomainDeep inspects exponents without constructing powers or rationals.
	// Unsupported state keeps the measured counters, but is never a witness.
	if _, ok := dec.InDomainDeep(&state); !ok {
		return d
	}
	d.status = int32(state.Status)
	d.initialState = proto.Equal(&state, initial)
	d.committedState = state.Status == orderpb.OrderStatus_ORDER_STATUS_CANCELLED &&
		state.FilledQuantity != nil && state.LeavesQuantity != nil && state.AverageFillPrice != nil &&
		dec.Cmp(state.FilledQuantity, &commonpb.Decimal{Coefficient: 8000}) == 0 &&
		dec.Cmp(state.LeavesQuantity, &commonpb.Decimal{Coefficient: 1}) == 0 &&
		dec.Cmp(state.AverageFillPrice, &commonpb.Decimal{Coefficient: 1005, Exponent: -1}) == 0
	d.known = true
	return d
}

func (d lockOutcomeDiagnostic) classification() string {
	if !d.known {
		return "UNKNOWN"
	}
	switch d.caseStatus {
	case "absent", "observed", "investigating", "corrected", "blocked":
	default:
		return "UNKNOWN"
	}
	if d.outbox < d.recovered || d.pending < 0 || d.pending > d.recovered || d.attempts < 0 || d.attempts > d.recovered {
		return "partial_or_inconsistent"
	}
	if d.version == 0 && d.initialState && d.checkpoint == 0 && d.expected == 0 &&
		d.caseStatus != "corrected" && d.fills == 0 && d.executions == 0 && d.targets == 0 &&
		d.recovered == 0 && d.pending == 0 && d.attempts == 0 && d.acks == 0 && d.fees == 0 {
		return "initial_financial_witness"
	}
	if d.version == 1 && d.committedState && d.checkpoint == 8000 && d.expected == 16000 &&
		d.caseStatus == "investigating" && d.fills == 8000 && d.executions == 8000 && d.targets == 8000 &&
		d.recovered == 8000 && d.acks == 0 && d.fees == 0 && d.attempts == 0 && d.pending >= 0 && d.pending <= d.recovered {
		return "committed_financial_witness"
	}
	return "partial_or_inconsistent"
}

func (d lockOutcomeDiagnostic) summary(phase string) string {
	// No raw state, identifiers, SQL errors or arbitrary database strings.
	switch phase {
	case "before_close", "after_joined_close":
	default:
		phase = "UNKNOWN"
	}
	status := d.caseStatus
	switch status {
	case "absent", "observed", "investigating", "corrected", "blocked":
	default:
		status = "UNKNOWN"
	}
	return fmt.Sprintf("unexpected recovery durable observation: phase=%s classification=%s counts_observed=%t known=%t initial_state=%t committed_state=%t order_status=%d version=%d case_status=%s case_version=%d checkpoint=%d expected_acks=%d fills=%d executions=%d targets=%d outbox=%d recovered=%d pending=%d attempted=%d acks=%d fees=%d", phase, d.classification(), d.countsObserved, d.known, d.initialState, d.committedState, d.status, d.version, status, d.caseVersion, d.checkpoint, d.expected, d.fills, d.executions, d.targets, d.outbox, d.recovered, d.pending, d.attempts, d.acks, d.fees)
}

func testLockOutcomeDiagnostics(t *testing.T) {
	t.Helper()
	rollback := lockOutcomeDiagnostic{known: true, initialState: true, caseStatus: "investigating", outbox: 1}
	commit := lockOutcomeDiagnostic{known: true, committedState: true, version: 1, caseStatus: "investigating", checkpoint: 8000, expected: 16000, fills: 8000, executions: 8000, targets: 8000, outbox: 8001, recovered: 8000, pending: 7999}
	if rollback.classification() != "initial_financial_witness" || commit.classification() != "committed_financial_witness" || (lockOutcomeDiagnostic{}).classification() != "UNKNOWN" {
		t.Fatal("durable outcome classification differs")
	}
	for _, phase := range []string{"before_close", "after_joined_close"} {
		if !strings.Contains(rollback.summary(phase), "phase="+phase+" classification=initial_financial_witness") || !strings.Contains(commit.summary(phase), "phase="+phase+" classification=committed_financial_witness") {
			t.Fatal("diagnostic witness phase missing")
		}
	}
	if strings.Contains(commit.summary("private phase"), "private phase") || !strings.Contains(commit.summary("private phase"), "phase=UNKNOWN") {
		t.Fatal("unrecognized diagnostic phase leaked")
	}
	for _, mutate := range []func(*lockOutcomeDiagnostic){
		func(d *lockOutcomeDiagnostic) { d.fills-- },
		func(d *lockOutcomeDiagnostic) { d.executions-- },
		func(d *lockOutcomeDiagnostic) { d.targets-- },
		func(d *lockOutcomeDiagnostic) { d.recovered-- },
		func(d *lockOutcomeDiagnostic) { d.acks++ },
		func(d *lockOutcomeDiagnostic) { d.fees++ },
		func(d *lockOutcomeDiagnostic) { d.committedState = false },
		func(d *lockOutcomeDiagnostic) { d.version++ },
		func(d *lockOutcomeDiagnostic) { d.checkpoint-- },
		func(d *lockOutcomeDiagnostic) { d.expected-- },
	} {
		d := commit
		mutate(&d)
		if d.classification() != "partial_or_inconsistent" {
			t.Fatal("inconsistent inventory classified as committed witness")
		}
	}
	d := rollback
	d.fills = 1
	if d.classification() != "partial_or_inconsistent" {
		t.Fatal("partial claim classified as initial witness")
	}
	d = rollback
	d.caseStatus = "private database value"
	if d.classification() != "UNKNOWN" || strings.Contains(d.summary("before_close"), d.caseStatus) {
		t.Fatal("unknown database state leaked or classified as a witness")
	}
	testLockOutcomeDecimalDomain(t)
}

func testLockOutcomeDecimalDomain(t *testing.T) {
	t.Helper()
	st, _ := largeHistory(0)
	state := proto.Clone(st).(*orderpb.OrderState)
	state.Status = orderpb.OrderStatus_ORDER_STATUS_CANCELLED
	state.FilledQuantity = &commonpb.Decimal{Coefficient: 8000}
	state.LeavesQuantity = &commonpb.Decimal{Coefficient: 1}
	state.AverageFillPrice = &commonpb.Decimal{Coefficient: 1005, Exponent: -1}
	observed := lockOutcomeDiagnostic{countsObserved: true, version: 1, fills: 8000}
	check := func(s *orderpb.OrderState) lockOutcomeDiagnostic {
		raw, err := proto.Marshal(s)
		if err != nil {
			t.Fatal("diagnostic test state encoding failed")
		}
		return observed.observeState(raw, st)
	}
	if d := check(state); !d.known || !d.committedState {
		t.Fatal("bounded economic witness lost")
	}
	state.AverageFillPrice = &commonpb.Decimal{Coefficient: 10050, Exponent: -2}
	if d := check(state); !d.known || !d.committedState {
		t.Fatal("equivalent bounded Decimal witness lost")
	}
	for _, exponent := range []int32{math.MinInt32, math.MaxInt32, -65, 65} {
		for _, field := range []string{"filled", "leaves", "average"} {
			bad := proto.Clone(state).(*orderpb.OrderState)
			switch field {
			case "filled":
				bad.FilledQuantity.Exponent = exponent
			case "leaves":
				bad.LeavesQuantity.Exponent = exponent
			case "average":
				bad.AverageFillPrice.Exponent = exponent
			}
			d := check(bad)
			if d.known || d.classification() != "UNKNOWN" || !d.countsObserved || d.fills != 8000 || d.version != 1 {
				t.Fatal("unsupported Decimal lost observations or became a witness")
			}
		}
	}
	if d := observed.observeState([]byte{0xff}, st); d.known || !d.countsObserved || d.fills != 8000 {
		t.Fatal("malformed state lost observations or became a witness")
	}
}
