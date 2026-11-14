package server_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/lineage/internal/governance"
	"github.com/eighred/kanz/services/lineage/internal/graph"
	"github.com/eighred/kanz/services/lineage/internal/harvest"
	"github.com/eighred/kanz/services/lineage/internal/openlineage"
	"github.com/eighred/kanz/services/lineage/internal/query"
	"github.com/eighred/kanz/services/lineage/internal/server"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type catalogCapture struct{ events []openlineage.RunEvent }

func (c *catalogCapture) Emit(_ context.Context, event openlineage.RunEvent) error {
	c.events = append(c.events, event)
	return nil
}

func catalogFixture(t *testing.T) (*server.Server, []openlineage.RunEvent) {
	t.Helper()
	g := graph.NewMemory()
	emitter := &catalogCapture{}
	h := harvest.New(g, emitter)
	now := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	for _, env := range []*envelopepb.Envelope{
		{EventId: "profile-event", Domain: "customer", PayloadSchemaRef: "customer.v1.PersonProfile:1", CorrelationId: "00000000-0000-4000-8000-000000000001", EventTime: timestamppb.New(now)},
		{EventId: "exposure-event", Domain: "risk", PayloadSchemaRef: "risk.v1.CustomerExposure:1", CorrelationId: "00000000-0000-4000-8000-000000000002", CausationId: "profile-event", EventTime: timestamppb.New(now)},
	} {
		if err := h.Handle(context.Background(), env, nil); err != nil {
			t.Fatal(err)
		}
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	gov := governance.NewGovernor(governance.NewClassifier(&governance.Config{Datasets: []string{"PersonProfile"}}),
		auth.NewPolicyAuthorizer(&auth.Policy{Roles: map[string][]auth.Action{"steward": {governance.ActionPIIRead}}}))
	ready := &server.Readiness{}
	ready.Set(true)
	return server.New(ready, logger, server.WithLineage(g, query.NewService(g, gov))), emitter.events
}

func catalogScratch(t *testing.T) string {
	t.Helper()
	path := os.Getenv("TEST_CATALOG_SCRATCH")
	if path == "" {
		t.Skip("actual DataHub fixture requires catalog-verification workflow")
	}
	return path
}

func TestCatalogFixtureExport(t *testing.T) {
	path := catalogScratch(t)
	_, events := catalogFixture(t)
	body, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "events.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Actual catalog ingestion/readback and alteration happen before this phase in
// the dedicated workflow. Metadata is evidence, never an input to the Governor.
func TestCatalogMetadataCannotGrantLineageAccess(t *testing.T) {
	path := catalogScratch(t)
	body, err := os.ReadFile(filepath.Join(path, "readback.json"))
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Datasets map[string]json.RawMessage `json:"datasets"`
		Cases    []string                   `json:"cases"`
	}
	if err := json.Unmarshal(body, &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Datasets) != 2 || len(report.Cases) != 6 {
		t.Fatal("complete actual catalog proof and alteration are required")
	}
	for _, identity := range []string{
		"urn:li:dataset:(urn:li:dataPlatform:kanz,kanz.customer/PersonProfile,DEV)",
		"urn:li:dataset:(urn:li:dataPlatform:kanz,kanz.risk/CustomerExposure,DEV)",
	} {
		if _, ok := report.Datasets[identity]; !ok {
			t.Fatal("actual catalog dataset identity differs")
		}
	}
	s, _ := catalogFixture(t)
	actual := httptest.NewServer(s)
	defer actual.Close()
	for _, tc := range []struct {
		name, path, role string
		status           int
	}{
		{"anonymous-catalog", "/v1/catalog/datasets", "", http.StatusUnauthorized},
		{"anonymous-lineage", "/v1/lineage/dataset/kanz.customer/PersonProfile", "", http.StatusUnauthorized},
		{"catalog-owner-is-not-a-grant", "/v1/lineage/dataset/kanz.customer/PersonProfile", "viewer", http.StatusForbidden},
		{"steward-still-authorized", "/v1/lineage/dataset/kanz.customer/PersonProfile", "steward", http.StatusOK},
		{"upstream-pii-still-redacted", "/v1/lineage/dataset/kanz.risk/CustomerExposure", "viewer", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, actual.URL+tc.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			if tc.role != "" {
				auth.SetPrincipalHeaders(req.Header, "fixture-reader", "fixture-tenant", []string{tc.role})
			}
			client := &http.Client{Timeout: 5 * time.Second}
			response, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = response.Body.Close() }()
			if response.StatusCode != tc.status {
				t.Fatalf("status=%d, want %d", response.StatusCode, tc.status)
			}
			if tc.name == "upstream-pii-still-redacted" {
				var provenance query.Provenance
				if err := json.NewDecoder(io.LimitReader(response.Body, 64*1024)).Decode(&provenance); err != nil {
					t.Fatal(err)
				}
				if len(provenance.Upstream) != 1 || !provenance.Upstream[0].Redacted || provenance.Upstream[0].SchemaRef != "" {
					t.Fatal("catalog PUBLIC tag must not expose governed upstream schema")
				}
			}
		})
	}
}
