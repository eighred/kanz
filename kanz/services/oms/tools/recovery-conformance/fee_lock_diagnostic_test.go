package recoveryconformance

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PIDs and application names remain in memory. Only closed categories and
// counts leave this observer. PostgreSQL 16 pg_blocking_pids includes hard
// holders and soft blockers ahead in the wait queue:
// https://www.postgresql.org/docs/16/functions-info.html
type feeLockNode struct {
	pid, owner     int32
	active, locked bool
	blockers       []int32
}
type feeLockCounts struct{ present, active, locked, direct, reachable, waiting int }
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
	if node, exists := byPID[holder]; !exists || node.owner != 0 {
		return out, errors.New("lock holder identity")
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
			if node.active && node.locked {
				c.waiting++
			}
		}
	}
	return out, nil
}
func observeFeeLocks(ctx context.Context, pool *pgxpool.Pool, holder int32, goName, rustName string) (feeLockSnapshot, error) {
	if goName == "" || rustName == "" || goName == rustName {
		return feeLockSnapshot{}, errors.New("lock owner identity")
	}
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

// Both distinct named operations must be active Lock waiters in the same
// bounded observation, with a hard/soft blocker path to our held transaction.
func (s feeLockSnapshot) overlapped() bool {
	for _, owner := range s.owners {
		if owner.present != 1 || owner.waiting != 1 {
			return false
		}
	}
	return true
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
	if err != nil || s.direct != 1 || s.owners[0].direct != 1 || s.owners[1].direct != 0 || s.score() != 2 || !s.overlapped() {
		t.Fatal("direct and transitive lock evidence conflated")
	}
	for _, queued := range []bool{false, true} {
		positive := append([]feeLockNode(nil), nodes...)
		if !queued {
			positive[2].blockers = []int32{1}
		}
		got, err := feeLockGraph(positive, 1)
		if err != nil || !got.overlapped() {
			t.Fatal("valid hard/soft overlap refused")
		}
	}
	for _, mutate := range []func([]feeLockNode) []feeLockNode{
		func(n []feeLockNode) []feeLockNode { n[2].blockers = []int32{99}; return n },
		func(n []feeLockNode) []feeLockNode { n[2].owner = 0; return n },
		func(n []feeLockNode) []feeLockNode { n[2].active = false; return n },
		func(n []feeLockNode) []feeLockNode { n[2].locked = false; return n },
		func(n []feeLockNode) []feeLockNode { n[1].blockers = []int32{3}; return n },
		func(n []feeLockNode) []feeLockNode {
			return append(n, feeLockNode{pid: 4, owner: 1, active: true, locked: true, blockers: []int32{1}})
		},
		func(n []feeLockNode) []feeLockNode { n[0].owner = 1; return n },
		func(n []feeLockNode) []feeLockNode { return n[1:] },
	} {
		got, err := feeLockGraph(mutate(append([]feeLockNode(nil), nodes...)), 1)
		if err == nil && got.overlapped() {
			t.Fatal("invalid owner overlap accepted")
		}
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
