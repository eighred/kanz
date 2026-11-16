package server

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"time"
)

const preflightFormat = "kanz-oms-preflight-v1"
const maxPreflightBytes = 1 << 20

var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

var requiredPreflightChecks = []string{
	"deployed_release_identity",
	"mandate_enforcement",
	"mandate_change_surface",
	"mandate_distinct_signatory_pairs",
	"portfolio_inventory",
	"mandate_inventory",
	"venue_account_enforcement",
	"portfolio_account_binding_coverage",
	"exchange_account_proof_enforcement",
	"exchange_account_proof_sources",
	"exchange_accounts_observed_verified",
	"okx_margin_observation_current",
	"okx_margin_observation_complete",
	"dual_control_enforcement",
	"dual_control_threshold",
	"order_approval_surface",
	"distinct_maker_checker_pairs",
	"oms_singleton_ready",
	"recent_control_posture",
}

type preflightEvidence struct {
	path   string
	maxAge time.Duration
}

type preflightArtifact struct {
	Format         string                   `json:"format"`
	ObservedAt     time.Time                `json:"observed_at"`
	VerifierCommit string                   `json:"verifier_commit"`
	DeployedCommit string                   `json:"deployed_commit"`
	CommandID      string                   `json:"command_id"`
	Verdict        string                   `json:"verdict"`
	Checks         []preflightArtifactCheck `json:"checks"`
	WorkloadImages map[string]string        `json:"workload_images"`
}

type preflightArtifactCheck struct {
	Name     string          `json:"name"`
	Pass     bool            `json:"pass"`
	Observed json.RawMessage `json:"observed"`
}

type preflightStatus struct {
	Status         string                 `json:"status"`
	Reason         string                 `json:"reason,omitempty"`
	ObservedAt     *time.Time             `json:"observed_at,omitempty"`
	AgeSeconds     int64                  `json:"age_seconds,omitempty"`
	MaxAgeSeconds  int64                  `json:"max_age_seconds"`
	VerifierCommit string                 `json:"verifier_commit,omitempty"`
	DeployedCommit string                 `json:"deployed_commit,omitempty"`
	CommandID      string                 `json:"command_id,omitempty"`
	Checks         []preflightStatusCheck `json:"checks"`
	WorkloadImages map[string]string      `json:"workload_images,omitempty"`
}

type preflightStatusCheck struct {
	Name     string          `json:"name"`
	Status   string          `json:"status"`
	Observed json.RawMessage `json:"observed,omitempty"`
}

func newPreflightEvidence(path string, maxAge time.Duration) *preflightEvidence {
	if maxAge <= 0 {
		maxAge = 15 * time.Minute
	}
	return &preflightEvidence{path: path, maxAge: maxAge}
}

func (p *preflightEvidence) unknown(reason string) preflightStatus {
	return preflightStatus{Status: "UNKNOWN", Reason: reason, Checks: []preflightStatusCheck{}, MaxAgeSeconds: int64(p.maxAge.Seconds())}
}

func (p *preflightEvidence) read(now time.Time) preflightStatus {
	if p.path == "" {
		return p.unknown("preflight evidence is not configured")
	}
	f, err := os.Open(p.path)
	if err != nil {
		return p.unknown("preflight evidence is unavailable")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() > maxPreflightBytes {
		return p.unknown("preflight evidence is unreadable")
	}

	dec := json.NewDecoder(io.LimitReader(f, maxPreflightBytes))
	dec.DisallowUnknownFields()
	var a preflightArtifact
	if err := dec.Decode(&a); err != nil {
		return p.unknown("preflight evidence is unreadable")
	}
	if err := ensureJSONEnds(dec); err != nil {
		return p.unknown("preflight evidence is unreadable")
	}
	if err := validatePreflightArtifact(a); err != nil {
		return p.unknown(err.Error())
	}

	age := now.Sub(a.ObservedAt)
	if age < -5*time.Minute {
		return p.unknown("preflight evidence timestamp is in the future")
	}
	if age > p.maxAge {
		status := p.statusOf(a, age)
		status.Status = "UNKNOWN"
		status.Reason = "preflight evidence is stale"
		return status
	}
	return p.statusOf(a, age)
}

func ensureJSONEnds(dec *json.Decoder) error {
	var extra any
	err := dec.Decode(&extra)
	if err == io.EOF {
		return nil
	}
	if err == nil {
		return fmt.Errorf("more than one JSON value")
	}
	return err
}

func validatePreflightArtifact(a preflightArtifact) error {
	if a.Format != preflightFormat {
		return fmt.Errorf("preflight evidence format is not recognized")
	}
	if a.ObservedAt.IsZero() || !commitPattern.MatchString(a.VerifierCommit) || !commitPattern.MatchString(a.DeployedCommit) || a.CommandID == "" {
		return fmt.Errorf("preflight evidence identity is incomplete")
	}
	want := make(map[string]bool, len(requiredPreflightChecks))
	for _, name := range requiredPreflightChecks {
		want[name] = true
	}
	seen := make(map[string]bool, len(a.Checks))
	allPass := true
	for _, check := range a.Checks {
		if !want[check.Name] || seen[check.Name] || !json.Valid(check.Observed) {
			return fmt.Errorf("preflight evidence control set is invalid")
		}
		seen[check.Name] = true
		allPass = allPass && check.Pass
	}
	if len(seen) != len(want) {
		return fmt.Errorf("preflight evidence control set is incomplete")
	}
	derived := "FAIL"
	if allPass {
		derived = "PASS"
	}
	if a.Verdict != derived {
		return fmt.Errorf("preflight evidence verdict disagrees with its controls")
	}
	for _, name := range []string{"oms", "api_gateway", "venue_binance", "venue_okx"} {
		if a.WorkloadImages[name] == "" {
			return fmt.Errorf("preflight evidence deployed versions are incomplete")
		}
	}
	return nil
}

func (p *preflightEvidence) statusOf(a preflightArtifact, age time.Duration) preflightStatus {
	if age < 0 {
		age = 0
	}
	checks := make([]preflightStatusCheck, 0, len(a.Checks))
	for _, check := range a.Checks {
		state := "FAIL"
		if check.Pass {
			state = "PASS"
		}
		checks = append(checks, preflightStatusCheck{Name: check.Name, Status: state, Observed: check.Observed})
	}
	observed := a.ObservedAt
	return preflightStatus{
		Status: a.Verdict, ObservedAt: &observed, AgeSeconds: int64(age.Seconds()),
		MaxAgeSeconds: int64(p.maxAge.Seconds()), VerifierCommit: a.VerifierCommit,
		DeployedCommit: a.DeployedCommit, CommandID: a.CommandID, Checks: checks,
		WorkloadImages: a.WorkloadImages,
	}
}
