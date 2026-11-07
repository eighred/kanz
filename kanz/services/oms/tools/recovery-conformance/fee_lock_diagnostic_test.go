package recoveryconformance

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PIDs and application names remain in memory. Only closed categories and
// counts leave this observer; transitive reachability is diagnostic only.
type feeLockNode struct {
	pid, owner     int32
	active, locked bool
	blockers       []int32
}
type feeLockCounts struct{ present, active, locked, direct, reachable int }
type feeLockSnapshot struct {
	owners [2]feeLockCounts
	direct int
}
type feeLockReport struct {
	last, best               feeLockSnapshot
	polls, samples           int
	observed                 bool
	queryError, releaseError string
}

func feeLockGraph(nodes []feeLockNode, holder int32) (feeLockSnapshot, error) {
	var out feeLockSnapshot
	if holder <= 0 || len(nodes) > 128 {
		return out, errors.New("lock graph bound")
	}
	byPID := map[int32]feeLockNode{}
	for _, node := range nodes {
		if node.pid <= 0 || node.owner < 0 || node.owner > 2 || len(node.blockers) > 128 {
			return out, errors.New("lock graph shape")
		}
		for _, blocker := range node.blockers {
			if blocker < 0 {
				return out, errors.New("lock graph shape")
			}
		}
		if _, exists := byPID[node.pid]; exists {
			return out, errors.New("lock graph duplicate")
		}
		byPID[node.pid] = node
	}
	reaches := func(start int32) bool {
		seen := map[int32]bool{}
		pending := []int32{start}
		for len(pending) > 0 {
			pid := pending[len(pending)-1]
			pending = pending[:len(pending)-1]
			if seen[pid] {
				continue
			}
			seen[pid] = true
			for _, next := range byPID[pid].blockers {
				if next == holder {
					return true
				}
				if _, known := byPID[next]; known && !seen[next] {
					pending = append(pending, next)
				}
			}
		}
		return false
	}
	for _, node := range nodes {
		direct := false
		for _, blocker := range node.blockers {
			if blocker == holder {
				direct = true
			}
		}
		if direct {
			out.direct++
		}
		if node.owner == 0 {
			continue
		}
		c := &out.owners[node.owner-1]
		c.present++
		if node.active {
			c.active++
		}
		if node.locked {
			c.locked++
		}
		if direct {
			c.direct++
		}
		if reaches(node.pid) {
			c.reachable++
		}
	}
	return out, nil
}
func observeFeeLocks(ctx context.Context, pool *pgxpool.Pool, holder int32, goName, rustName string) (feeLockSnapshot, error) {
	rows, err := pool.Query(ctx, `SELECT pid,CASE application_name WHEN $1 THEN 1 WHEN $2 THEN 2 ELSE 0 END,
COALESCE(state='active',false),COALESCE(wait_event_type='Lock',false),pg_blocking_pids(pid)
FROM pg_stat_activity WHERE datname=current_database() AND usename=current_user LIMIT 129`, goName, rustName)
	if err != nil {
		return feeLockSnapshot{}, err
	}
	defer rows.Close()
	nodes := make([]feeLockNode, 0, 16)
	for rows.Next() {
		if len(nodes) == 128 {
			return feeLockSnapshot{}, errors.New("lock graph bound")
		}
		var n feeLockNode
		if err := rows.Scan(&n.pid, &n.owner, &n.active, &n.locked, &n.blockers); err != nil {
			return feeLockSnapshot{}, err
		}
		nodes = append(nodes, n)
	}
	if err := rows.Err(); err != nil {
		return feeLockSnapshot{}, err
	}
	return feeLockGraph(nodes, holder)
}
func (s feeLockSnapshot) score() int { return s.owners[0].reachable + s.owners[1].reachable }
func (s feeLockSnapshot) text() string {
	a, b := s.owners[0], s.owners[1]
	return fmt.Sprintf("direct=%d go_present=%d go_active=%d go_lock=%d go_direct=%d go_reachable=%d rust_present=%d rust_active=%d rust_lock=%d rust_direct=%d rust_reachable=%d", s.direct, a.present, a.active, a.locked, a.direct, a.reachable, b.present, b.active, b.locked, b.direct, b.reachable)
}
func feeRustOutcome(r response) string {
	if r.Status == "ok" && r.Error == "" {
		return "ok"
	}
	if r.Status != "error" {
		return "unknown"
	}
	switch r.Error {
	case "Conflict", "Deadline", "PublicationUnconfirmed", "FeeRevision", "RecoveryEvidenceConflict", "InvalidInput", "Database":
		return r.Error
	}
	return "other"
}
func testFeeLockDiagnostics(t *testing.T) {
	t.Helper()
	nodes := []feeLockNode{{pid: 1}, {pid: 2, owner: 1, active: true, locked: true, blockers: []int32{1}}, {pid: 3, owner: 2, active: true, locked: true, blockers: []int32{2}}}
	s, err := feeLockGraph(nodes, 1)
	if err != nil || s.direct != 1 || s.owners[0].direct != 1 || s.owners[1].direct != 0 || s.score() != 2 {
		t.Fatal("direct and transitive lock evidence conflated")
	}
	nodes[1].blockers = []int32{3}
	s, err = feeLockGraph(nodes, 1)
	if err != nil || s.score() != 0 {
		t.Fatal("cycle invented holder reachability")
	}
	if _, err := feeLockGraph(append(nodes, nodes[0]), 1); err == nil {
		t.Fatal("duplicate PID accepted")
	}
	if _, err := feeLockGraph(make([]feeLockNode, 129), 1); err == nil {
		t.Fatal("unbounded graph accepted")
	}
	if feeRustOutcome(response{Status: "error", Error: "private detail"}) != "other" {
		t.Fatal("raw outcome leaked")
	}
}
