package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnreadableRuleCannotCreateCLIProposal(t *testing.T) {
	for _, rule := range []string{
		`{"rule_id":"missing","type":"RULE_TYPE_CONCENTRATION","on_violation":"COMPLIANCE_STATUS_WARN"}`,
		`{"rule_id":"wrong","type":"RULE_TYPE_CONCENTRATION","buying_power":{}}`,
		`{"rule_id":"domain","type":"RULE_TYPE_CONCENTRATION","concentration":{"max_weight":{"coefficient":"1","exponent":65}}}`,
	} {
		dir := t.TempDir()
		body := strings.TrimSuffix(strings.TrimSpace(mandateJSON), "}") + `,"rules":[` + rule + `]}`
		input := writeFile(t, dir, "mandate.json", body)
		output := filepath.Join(dir, "proposal.json")
		var buf bytes.Buffer
		err := run([]string{"propose", "--tenant", "acme", "--file", input, "--by", "operator:alice", "--reason", "invalid rule", "--out", output}, &buf)
		if err == nil {
			t.Fatal("unreadable rule created proposal")
		}
		if _, statErr := os.Stat(output); !os.IsNotExist(statErr) {
			t.Fatalf("unreadable proposal artifact exists: %v", statErr)
		}
	}
}
