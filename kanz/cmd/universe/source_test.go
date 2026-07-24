package main

import (
	"testing"
	"time"

	operatorpb "github.com/kanz-eng/kanz-schemas-go/operator/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestToNodeRowsNilCreatedAtRendersDash(t *testing.T) {
	rows := toNodeRows([]*operatorpb.Node{
		{Name: "ghost", Status: operatorpb.NodeStatus_NODE_STATUS_UNSPECIFIED}, // no CreatedAt
	})
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if rows[0].Age != "-" {
		t.Errorf("nil CreatedAt Age = %q, want %q (must not render the epoch)", rows[0].Age, "-")
	}
	if rows[0].Status != "Unknown" {
		t.Errorf("UNSPECIFIED status label = %q, want Unknown", rows[0].Status)
	}
}

func TestToNodeRowsRealCreatedAtRendersAge(t *testing.T) {
	rows := toNodeRows([]*operatorpb.Node{
		{Name: "fresh", CreatedAt: timestamppb.New(time.Now().Add(-72 * time.Hour))},
	})
	if rows[0].Age != "3d" {
		t.Errorf("72h-old node Age = %q, want 3d", rows[0].Age)
	}
}
