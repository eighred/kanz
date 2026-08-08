// THE RECORDER WIRING SURVIVES A MISSING BUS (#352).
//
// The transport moved up: run() now dials the bus before buildGovernor, because
// the AUTH-01d decision recorder needs a producer on the same connection and the
// governor that holds it is built first. The risk that move introduces is
// precise — a service that TODAY serves its read API with no LINEAGE_NATS_URL at
// all could start requiring one, and the failure would be a pod refusing to
// start in a deployment where it starts now.
//
// These tests pin both halves: the governor still builds with the slog recorder
// (the read-only deployment), and the recorder it uses is the one it was HANDED
// rather than one it built for itself.
package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	observationpb "github.com/eighred/kanz/kanz-schemas-go/observation/v1"

	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/lineage/internal/config"
	"github.com/eighred/kanz/services/lineage/internal/governance"
	"github.com/eighred/kanz/services/lineage/internal/graph"
)

// countingRecorder records what it was handed, so a test can prove the governor
// reaches THIS recorder rather than one it constructed itself.
type countingRecorder struct{ entries []*observationpb.DecisionLog }

func (c *countingRecorder) Record(_ context.Context, e *observationpb.DecisionLog) error {
	c.entries = append(c.entries, e)
	return nil
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// piiConfig writes a governance file classifying one dataset as PII. Without it
// every dataset is public and CheckAccess never consults the authorizer at all —
// so a test that skipped this would pass against a governor wired to nothing.
func piiConfig(t *testing.T, dataset string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "governance.json")
	body := `{"pii_datasets":["` + dataset + `"],"pii_schema_ref_substrings":[]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write governance config: %v", err)
	}
	return path
}

// WITH NO POLICY AND NO BUS, THE GOVERNOR STILL BUILDS.
//
// This is the read-only deployment: LINEAGE_NATS_URL unset, so run() passes the
// slog recorder. If this ever fails, hoisting the transport has made lineage
// refuse to start where it starts today.
func TestBuildGovernorWorksWithTheSlogRecorder(t *testing.T) {
	gov, err := buildGovernor(config.Config{}, discardLogger(), auth.NewSlogRecorder(discardLogger()))
	if err != nil {
		t.Fatalf("buildGovernor: %v\n\n"+
			"With no LINEAGE_NATS_URL this service serves the read API and must still start. A "+
			"governor that cannot be built without a bus is the exact regression hoisting the "+
			"transport risks.", err)
	}
	if gov == nil {
		t.Fatal("buildGovernor returned a nil governor with no error")
	}
}

// THE RECORDER IS INJECTED, NOT CONSTRUCTED.
//
// If buildGovernor goes back to building its own, whichever recorder run() chose
// — including the bus-backed one — is wired to nothing, and every authorization
// decision still reaches the log only while the bus wiring looks correct.
func TestBuildGovernorUsesTheInjectedRecorder(t *testing.T) {
	const ds = "pii-ns.customers"
	rec := &countingRecorder{}

	gov, err := buildGovernor(
		config.Config{GovernanceFile: piiConfig(t, ds)},
		discardLogger(),
		rec,
	)
	if err != nil {
		t.Fatalf("buildGovernor: %v", err)
	}

	// Deny-by-default: no policy bundle, so this is a DENY — and a refusal
	// nobody can reconstruct is worse than an allow nobody can, which is why
	// AUTH-01d records both.
	decision, sens := gov.CheckAccess(context.Background(),
		&auth.Principal{Subject: "user:someone", Tenant: "acme"},
		graph.DatasetID{Namespace: "pii-ns", Name: "customers"},
		"")

	// NON-VACUITY: if the dataset classified as public, CheckAccess short-circuits
	// before the authorizer and this test would prove nothing about the recorder.
	if sens != governance.SensitivityPII {
		t.Fatalf("dataset classified as %v, want PII — CheckAccess returns before the authorizer for "+
			"a public dataset, so the assertion below would pass against a governor wired to nothing",
			sens)
	}
	if decision.Allow {
		t.Fatalf("access allowed with no policy bundle: %+v — deny-by-default is not holding", decision)
	}

	if len(rec.entries) == 0 {
		t.Fatal("the injected recorder saw no decision.\n\n" +
			"buildGovernor is constructing its own recorder again, so whichever one run() chose — " +
			"including the bus-backed one — is wired to nothing and every authorization decision " +
			"still reaches the log only.")
	}
}
