package main

import (
	"bytes"
	"os"
	"testing"
)

func TestPublishedContractsMatchRegisteredReads(t *testing.T) {
	browser, gateway, err := render()
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string][]byte{
		"../../../../../web/src/api/broker_contract.ts":           browser,
		"../../../api_gateway/internal/gateway/broker_reads.json": gateway,
	} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(bytes.ReplaceAll(got, []byte("\r\n"), []byte("\n")), want) {
			t.Errorf("%s is stale: run go generate ./services/tv_sync/internal/projection", path)
		}
	}
}
