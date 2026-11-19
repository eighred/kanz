package main

import (
	"bytes"
	"os"
	"testing"
)

func TestBrowserContractMatchesProjection(t *testing.T) {
	want, err := render()
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile("../../../../../web/src/api/broker_contract.ts")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.ReplaceAll(got, []byte("\r\n"), []byte("\n")), want) {
		t.Fatal("broker contract is stale: run go generate ./services/tv_sync/internal/projection")
	}
}
