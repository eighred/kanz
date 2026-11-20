package server

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var updatePreflightContract = flag.Bool("update-preflight-contract", false, "write the browser preflight contract and response fixture")

func TestPreflightObservationUsesGoZeroInstant(t *testing.T) {
	for _, tc := range []struct {
		wire  string
		valid bool
	}{
		{"0001-01-01T00:00:00Z", false},
		{"0001-01-01T01:30:00.000000000+01:30", false},
		{"0001-01-01T00:00:00.000000001Z", true},
	} {
		instant, err := time.Parse(time.RFC3339Nano, tc.wire)
		if err != nil {
			t.Fatal(err)
		}
		if valid := validatePreflightArtifact(artifactAt(instant, true)) == nil; valid != tc.valid {
			t.Errorf("observed_at %s: valid = %t, want %t", tc.wire, valid, tc.valid)
		}
	}
}

// Generate the public control vocabulary from the same lists used to admit an
// artifact. Keep the example response separate so it never enters the browser bundle.
func TestPreflightBrowserContract(t *testing.T) {
	web := filepath.Join("..", "..", "..", "..", "..", "web", "src", "api")
	checkFile := func(path string, data []byte) {
		t.Helper()
		if *updatePreflightContract {
			if err := os.WriteFile(path, data, 0644); err != nil {
				t.Fatal(err)
			}
			return
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(bytes.ReplaceAll(got, []byte("\r\n"), []byte("\n")), data) {
			t.Fatalf("%s is stale; run go generate ./services/web_bff/internal/server", filepath.Base(path))
		}
	}
	var contract bytes.Buffer
	contract.WriteString("// Code generated from the BFF preflight evidence requirements. DO NOT EDIT.\n")
	for _, list := range []struct {
		name  string
		items []string
	}{{"preflightChecks", requiredPreflightChecks}, {"preflightWorkloads", requiredPreflightWorkloads}} {
		contract.WriteString("export const " + list.name + " = [\n")
		for _, item := range list.items {
			b, err := json.Marshal(item)
			if err != nil {
				t.Fatal(err)
			}
			contract.WriteString("  " + string(b) + ",\n")
		}
		contract.WriteString("] as const\n")
	}
	checkFile(filepath.Join(web, "preflight_contract.ts"), contract.Bytes())

	at := time.Date(2026, 9, 13, 12, 0, 0, 123456789, time.UTC)
	evidence := newPreflightEvidence(writeArtifact(t, artifactAt(at, true)), 15*time.Minute)
	status := evidence.read(at)
	if status.Status != "PASS" {
		t.Fatal("reference artifact did not pass the actual reader")
	}
	wire, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	checkFile(filepath.Join("testdata", "preflight_pass.json"), append(wire, '\n'))
}
