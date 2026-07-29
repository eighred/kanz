package natsrebuild

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestQualify(t *testing.T) {
	tests := []struct {
		name   string
		tenant string
		base   string
		want   string
	}{
		{"system tenant keeps the un-prefixed legacy name", SystemTenant, "order.order", "order.order"},
		{"named tenant is prefixed", "acme", "order.order", "acme.order.order"},
		{"snapshot suffix survives the prefix", "acme", "risk.position.snapshot", "acme.risk.position.snapshot"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Qualify(tt.tenant, tt.base); got != tt.want {
				t.Errorf("Qualify(%q, %q) = %q, want %q", tt.tenant, tt.base, got, tt.want)
			}
		})
	}
}

// The archiver's naming rule is no longer duplicated here: internal/topic owns
// it and both sides call topic.Qualify. The test that used to read the
// archiver's SOURCE and fail on divergence is gone with the copy it guarded — a
// drift check is the right answer only while the drift is possible, and keeping
// it would assert agreement between a function and itself.

func TestParseTenants(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		set     bool
		want    []string
		wantErr string
	}{
		{
			name: "unset defaults to the system tenant — the historical run, unchanged",
			set:  false,
			want: []string{SystemTenant},
		},
		{
			name: "one named tenant",
			raw:  "acme", set: true,
			want: []string{"acme"},
		},
		{
			name: "several, whitespace tolerated",
			raw:  " acme , globex ,__system__", set: true,
			want: []string{"acme", "globex", SystemTenant},
		},
		{
			name: "SET BUT EMPTY is a refusal, not a default",
			raw:  " , ", set: true,
			wantErr: "names no tenant",
		},
		{
			name: "a dot in a tenant id would cross the ACL prefix boundary",
			raw:  "acme.order", set: true,
			wantErr: "invalid tenant",
		},
		{
			name: "uppercase is not a provisioned tenant id",
			raw:  "Acme", set: true,
			wantErr: "invalid tenant",
		},
		{
			name: "a repeated tenant would drain every topic twice",
			raw:  "acme,globex,acme", set: true,
			wantErr: "more than once",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseTenants(tt.raw, tt.set)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("ParseTenants(%q, %v) = %v, want error containing %q", tt.raw, tt.set, got, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseTenants(%q, %v): %v", tt.raw, tt.set, err)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("ParseTenants(%q, %v) = %v, want %v", tt.raw, tt.set, got, tt.want)
			}
		})
	}
}

func topicsOf(targets []Target) []string {
	out := make([]string, 0, len(targets))
	for _, t := range targets {
		out = append(out, t.Topic)
	}
	return out
}

func TestResolveTargets(t *testing.T) {
	base := []string{"order.order", "risk.position"}
	state := []string{"risk.position"}

	tests := []struct {
		name    string
		tenants []string
		topics  []string
		state   []string
		want    []string
		wantErr string
	}{
		{
			name:    "system tenant only — identical to the pre-tenant topic list",
			tenants: []string{SystemTenant}, topics: base, state: state,
			want: []string{"order.order", "risk.position"},
		},
		{
			name:    "one named tenant resolves prefixed topics",
			tenants: []string{"acme"}, topics: base, state: state,
			want: []string{"acme.order.order", "acme.risk.position"},
		},
		{
			name:    "several tenants, system alongside named",
			tenants: []string{SystemTenant, "acme", "globex"}, topics: base, state: state,
			want: []string{
				"order.order", "risk.position",
				"acme.order.order", "acme.risk.position",
				"globex.order.order", "globex.risk.position",
			},
		},
		{
			name:    "no topics is a refusal — nothing would be restored for anyone",
			tenants: []string{"acme"}, topics: nil, state: nil,
			wantErr: "nothing would be restored",
		},
		{
			name:    "no tenants is a refusal",
			tenants: nil, topics: base, state: state,
			wantErr: "no tenants requested",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveTargets(tt.tenants, tt.topics, tt.state)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(topicsOf(got), tt.want) {
				t.Fatalf("topics = %v, want %v", topicsOf(got), tt.want)
			}
		})
	}
}

// TestResolveTargetsCarriesStateClassAcrossTheTenantPrefix is the compaction
// trap one level up: the state list is un-prefixed, so a prefixed target whose
// State flag was lost would be read through the time window instead of in full
// — and a tenant's mandate armed before the window would not come back.
func TestResolveTargetsCarriesStateClassAcrossTheTenantPrefix(t *testing.T) {
	targets, err := ResolveTargets(
		[]string{"acme"},
		[]string{"order.order", "risk.position"},
		[]string{"risk.position"})
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range targets {
		wantState := target.Base == "risk.position"
		if target.State != wantState {
			t.Fatalf("target %+v: State = %v, want %v", target, target.State, wantState)
		}
		rng := target.Window(24*time.Hour, time.Now())
		if wantState {
			if rng.StartOffset == nil || *rng.StartOffset != 0 || rng.StartTime != nil {
				t.Fatalf("state target %q: want a full read from offset 0, got %+v", target.Topic, rng)
			}
		} else if rng.StartTime == nil || rng.StartOffset != nil {
			t.Fatalf("event target %q: want a time-bounded window, got %+v", target.Topic, rng)
		}
	}
}

// TestRequireTenantCoverageRejectsARequestedTenantWithNoTopics is the
// regression issue #93 exists to prevent: a tenant that was asked for and
// resolved to nothing used to drain nothing and exit 0.
func TestRequireTenantCoverageRejectsARequestedTenantWithNoTopics(t *testing.T) {
	targets := []Target{{Tenant: "acme", Base: "order.order", Topic: "acme.order.order"}}

	if err := RequireTenantCoverage([]string{"acme"}, targets); err != nil {
		t.Fatalf("covered tenant rejected: %v", err)
	}
	err := RequireTenantCoverage([]string{"acme", "globex"}, targets)
	if err == nil {
		t.Fatal("a requested tenant with 0 topics was accepted — this is the exit-0-restores-nothing defect")
	}
	if !strings.Contains(err.Error(), "globex") {
		t.Fatalf("error must name the uncovered tenant, got: %v", err)
	}
}
