package sink_test

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kanz-eng/kanz/services/lake-sink/internal/sink"
)

func TestFileSinkHivePartitionAndContent(t *testing.T) {
	root := t.TempDir()
	s, err := sink.NewFileSink(root, "inst1")
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Date(2026, 6, 25, 10, 0, 0, 0, time.UTC)
	rows := []sink.Row{
		{EventID: "e1", Domain: "market", Entity: "MarketDataEvent", EventTime: ts, Payload: json.RawMessage(`{"x":1}`)},
		{EventID: "e2", Domain: "market", Entity: "MarketDataEvent", EventTime: ts},
		{EventID: "e3", Domain: "risk", Entity: "PortfolioState", EventTime: ts.Add(24 * time.Hour)},
	}
	for _, r := range rows {
		if err := s.Write(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Hive layout: {domain}/{entity}/dt=YYYY-MM-DD/part-{instance}.ndjson
	marketPart := filepath.Join(root, "market", "MarketDataEvent", "dt=2026-06-25", "part-inst1.ndjson")
	ids := readEventIDs(t, marketPart)
	if len(ids) != 2 || ids[0] != "e1" || ids[1] != "e2" {
		t.Errorf("market partition got %v, want [e1 e2]", ids)
	}
	riskPart := filepath.Join(root, "risk", "PortfolioState", "dt=2026-06-26", "part-inst1.ndjson")
	if ids := readEventIDs(t, riskPart); len(ids) != 1 || ids[0] != "e3" {
		t.Errorf("risk partition got %v, want [e3]", ids)
	}
}

func TestFileSinkSanitizesPathSegments(t *testing.T) {
	root := t.TempDir()
	s, err := sink.NewFileSink(root, "inst1")
	if err != nil {
		t.Fatal(err)
	}
	// A hostile domain must not escape the root or inject a separator.
	row := sink.Row{EventID: "x", Domain: "../../etc", Entity: "a/b", EventTime: time.Unix(0, 0).UTC()}
	if err := s.Write(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Everything written must live under root.
	var found int
	_ = filepath.Walk(root, func(p string, info os.FileInfo, _ error) error {
		if info != nil && !info.IsDir() {
			rel, err := filepath.Rel(root, p)
			if err != nil || len(rel) >= 2 && rel[0] == '.' && rel[1] == '.' {
				t.Errorf("file escaped root: %s", p)
			}
			found++
		}
		return nil
	})
	if found != 1 {
		t.Errorf("expected exactly one landed file under root, got %d", found)
	}
}

func readEventIDs(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	var ids []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var row sink.Row
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			t.Fatalf("bad ndjson line: %v", err)
		}
		ids = append(ids, row.EventID)
	}
	return ids
}
