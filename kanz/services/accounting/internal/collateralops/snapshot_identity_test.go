package collateralops

import (
	"errors"
	"sync"
	"testing"
	"time"

	pb "github.com/eighred/kanz/kanz-schemas-go/collateral/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestPostgresSnapshotCannotRenameAnObligation(t *testing.T) {
	s, tenantStore := database(t)
	input := fixture()
	input.AsOf = timestamppb.New(input.AsOf.AsTime().Truncate(time.Microsecond).Add(100 * time.Nanosecond))
	if err := s.ImportSnapshot(t.Context(), "tenant-A", input); err != nil {
		t.Fatal(err)
	}
	for _, changed := range []bool{false, true} {
		other := proto.Clone(input).(*pb.WorkflowSnapshot)
		other.SnapshotId = "renamed"
		if changed {
			other.Agreements[0].Exposure = number(1)
		}
		if err := s.ImportSnapshot(t.Context(), "tenant-A", other); !errors.Is(err, ErrConflict) {
			t.Fatalf("renamed obligation (changed=%v) accepted: %v", changed, err)
		}
	}
	if err := s.ImportSnapshot(t.Context(), "tenant-A", input); err != nil {
		t.Fatalf("original retry: %v", err)
	}
	other := proto.Clone(input).(*pb.WorkflowSnapshot)
	other.SnapshotId = "moved-portfolio"
	other.PortfolioId = "PF2"
	other.AsOf = timestamppb.New(input.AsOf.AsTime().Add(200 * time.Nanosecond))
	other.Inventory[0].LotId = "new-lot"
	if err := s.ImportSnapshot(t.Context(), "tenant-A", other); !errors.Is(err, ErrConflict) {
		t.Fatalf("portfolio/sub-microsecond alias accepted: %v", err)
	}
	var count int
	if err := s.pool.QueryRow(t.Context(), `SELECT count(*) FROM collateral_lots WHERE lot_id='new-lot'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rejected import changed inventory: %d %v", count, err)
	}
	if err := tenantStore("tenant-B").ImportSnapshot(t.Context(), "tenant-B", input); err != nil {
		t.Fatalf("independent tenant observation blocked: %v", err)
	}
}

func TestPostgresConcurrentContradictorySnapshots(t *testing.T) {
	s, _ := database(t)
	a := fixture()
	b := proto.Clone(a).(*pb.WorkflowSnapshot)
	b.SnapshotId = "other"
	b.Agreements[0].Exposure = number(1)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, input := range []*pb.WorkflowSnapshot{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = New(s.pool).ImportSnapshot(t.Context(), "tenant-A", input)
		}()
	}
	wg.Wait()
	success, conflicts := 0, 0
	for _, err := range errs {
		if err == nil {
			success++
		} else if errors.Is(err, ErrConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflicts != 1 {
		t.Fatalf("ambiguous committed state: success=%d conflicts=%d", success, conflicts)
	}
}

func TestPostgresFutureSnapshotCannotPoisonFreshness(t *testing.T) {
	s, _ := database(t)
	input := fixture()
	input.AsOf = timestamppb.New(time.Now().UTC().Add(time.Minute))
	if err := s.ImportSnapshot(t.Context(), "tenant-A", input); !errors.Is(err, ErrStale) {
		t.Fatalf("future knowledge accepted: %v", err)
	}
	var count int
	if err := s.pool.QueryRow(t.Context(), `SELECT count(*) FROM collateral_snapshots`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("future snapshot persisted: %d %v", count, err)
	}
}
