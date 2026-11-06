package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestReplayCommandRefusesArgumentsAndUnknownSource(t *testing.T) {
	original := os.Args
	t.Cleanup(func() { os.Args = original })
	t.Setenv("TV_SYNC_REPLAY_EXPECTED_VERSION", "untrusted-source")
	t.Setenv("TV_SYNC_DATABASE_URL", "synthetic-must-not-appear")
	for _, args := range [][]string{{"tv-sync-replay", "synthetic-must-not-appear"}, {"tv-sync-replay"}} {
		os.Args = args
		var out bytes.Buffer
		if run(&out) == 0 {
			t.Fatal("unqualified command succeeded")
		}
		if strings.Contains(out.String(), "synthetic") || strings.Contains(out.String(), "untrusted-source") {
			t.Fatal("input disclosed")
		}
		var result struct{ Status, Reason string }
		if json.Unmarshal(out.Bytes(), &result) != nil || result.Status != "REFUSED" || result.Reason == "" {
			t.Fatal("missing closed refusal")
		}
	}
}
