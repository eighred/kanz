package main

import (
	"reflect"
	"testing"
)

func TestUnjoinedReadRetainsOnlyPoolAndStillJoinsDiagnosticWorker(t *testing.T) {
	for _, retain := range []bool{false, true} {
		var calls []string
		cleanup := &storesCleanup{closeWorker: func() { calls = append(calls, "worker-joined") }, closePool: func() { calls = append(calls, "pool-closed") }}
		if retain {
			cleanup.RetainPoolUntilExit()
		}
		cleanup.Close()
		want := []string{"worker-joined", "pool-closed"}
		if retain {
			want = []string{"worker-joined"}
		}
		if !reflect.DeepEqual(calls, want) {
			t.Fatalf("retain=%v cleanup lost ownership ordering: %v, want %v", retain, calls, want)
		}
	}
}
