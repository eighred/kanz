package snapshot

import (
	"context"
	"errors"
	"testing"

	lakepb "github.com/eighred/kanz/kanz-schemas-go/lake/v1"
	"github.com/eighred/kanz/pkg/auth"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestCallerAndResourceAuthorization(t *testing.T) {
	policy := auth.NewPolicyAuthorizer(&auth.Policy{Roles: map[string][]auth.Action{"research": {ActionCreate, ActionRead}, "reader": {"read"}}})
	for _, tc := range []struct {
		name    string
		caller  *lakepb.GatewayCaller
		owner   string
		allowed bool
	}{
		{"own", &lakepb.GatewayCaller{Subject: "human", Tenant: "a", Roles: []string{"research"}}, "a", true},
		{"missing", nil, "a", false},
		{"subject", &lakepb.GatewayCaller{Tenant: "a", Roles: []string{"research"}}, "a", false},
		{"foreign", &lakepb.GatewayCaller{Subject: "human", Tenant: "a", Roles: []string{"research"}}, "b", false},
		{"unknown-owner", &lakepb.GatewayCaller{Subject: "human", Tenant: "a", Roles: []string{"research"}}, "", false},
		{"read-is-not-snapshot-grant", &lakepb.GatewayCaller{Subject: "human", Tenant: "a", Roles: []string{"reader"}}, "a", false},
		{"portfolio-restricted", &lakepb.GatewayCaller{Subject: "human", Tenant: "a", Roles: []string{"research"}, Portfolios: []string{"p"}}, "a", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := Caller(tc.caller)
			if err == nil {
				err = Authorize(context.Background(), policy, p, ActionRead, "snapshot", tc.owner)
			}
			if (err == nil) != tc.allowed {
				t.Fatalf("allowed=%v expected=%v", err == nil, tc.allowed)
			}
		})
	}
	p, _ := Caller(&lakepb.GatewayCaller{Subject: "human", Tenant: "a", Roles: []string{"research"}})
	if !errors.Is(Authorize(context.Background(), policy, p, ActionRead, "snapshot", "b"), ErrNotFound) {
		t.Fatal("foreign resource disclosed")
	}
}

func TestSnapshotSelectionAndCursorPresence(t *testing.T) {
	selection := &lakepb.SnapshotSelection{EventTypes: []string{"order.filled"}, EventTimeFrom: &timestamppb.Timestamp{Seconds: 0, Nanos: 1}, EventTimeUntil: &timestamppb.Timestamp{Seconds: 0, Nanos: 2}}
	if err := ValidateSelection(selection); err != nil {
		t.Fatal(err)
	}
	selection.EventTimeUntil.Nanos = 1
	if ValidateSelection(selection) == nil {
		t.Fatal("empty interval accepted")
	}
	selection.EventTimeUntil.Nanos = 1_000_000_000
	if ValidateSelection(selection) == nil {
		t.Fatal("invalid nanoseconds accepted")
	}
	zero := uint64(0)
	q := &lakepb.SnapshotQuery{Operation: lakepb.SnapshotQueryOperation_SNAPSHOT_QUERY_OPERATION_LIST_EVENTS, AfterOrdinal: &zero, PageSize: 1}
	if ValidateQuery(q) != nil {
		t.Fatal("valid zero exclusive cursor rejected")
	}
	q.Operation = lakepb.SnapshotQueryOperation_SNAPSHOT_QUERY_OPERATION_COUNT_BY_EVENT_TYPE
	if ValidateQuery(q) == nil {
		t.Fatal("count ordinal accepted")
	}
	q.AfterOrdinal = nil
	if ValidateQuery(q) != nil {
		t.Fatal("bounded count refused")
	}
	q.PageSize = 1001
	if ValidateQuery(q) == nil {
		t.Fatal("unbounded page accepted")
	}
	q.PageSize = 1
	q.Operation = 999
	if ValidateQuery(q) == nil {
		t.Fatal("unknown operation accepted")
	}
}
