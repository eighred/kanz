package openlineage_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/eighred/kanz/services/lineage/internal/graph"
	"github.com/eighred/kanz/services/lineage/internal/openlineage"
)

func ownedEvent(t *testing.T, owner graph.Owner) openlineage.OwnedRunEvent {
	t.Helper()
	g, err := graph.NewMemory(graph.Limits{EventIndexMax: 4, DatasetMax: 4, EdgeMax: 4, StringBytesMax: 4096})
	if err != nil {
		t.Fatal(err)
	}
	s, err := g.Observe(graph.Observation{Owner: owner, EventID: "fixture", Dataset: graph.DatasetID{Namespace: "kanz.risk", Name: "ExposureSet"},
		Domain: "risk", SchemaRef: "risk.v1.ExposureSet:1", At: time.Unix(1, 0), Source: "risk-engine", CorrelationID: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	event, err := openlineage.FromSnapshot(s)
	if err != nil {
		t.Fatal(err)
	}
	return event
}

type countTransport struct{ calls int }

func (t *countTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.calls++
	return nil, errors.New("unexpected network")
}
func TestUnscopedHTTPRefusesConstructionAndDirectEmissionWithoutNetwork(t *testing.T) {
	transport := &countTransport{}
	if emitter, err := openlineage.NewHTTPEmitter("https://catalog.invalid", &http.Client{Transport: transport}); emitter != nil || !errors.Is(err, openlineage.ErrUnscopedSink) {
		t.Fatalf("constructor accepted unscoped sink: %v", err)
	}
	var direct openlineage.HTTPEmitter
	if err := direct.Emit(context.Background(), ownedEvent(t, "acme")); !errors.Is(err, openlineage.ErrUnscopedSink) {
		t.Fatalf("direct emitter accepted unscoped sink: %v", err)
	}
	if transport.calls != 0 {
		t.Fatal("refused sink performed network I/O")
	}
}
func TestOwnedLogEmissionAndUnknownOwnerRefusal(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))
	emitter := openlineage.NewLogEmitter(logger)
	if err := emitter.Emit(context.Background(), openlineage.OwnedRunEvent{}); err == nil {
		t.Fatal("unknown emission owner accepted")
	}
	if output.Len() != 0 {
		t.Fatal("unknown owner produced healthy emission")
	}
	if err := emitter.Emit(context.Background(), ownedEvent(t, "acme")); err != nil {
		t.Fatal(err)
	}
	var row map[string]any
	if err := json.Unmarshal(output.Bytes(), &row); err != nil {
		t.Fatal(err)
	}
	if row["owner"] != "acme" || row["output"] != "kanz.risk.ExposureSet" || row["job"] != "risk-engine" {
		t.Fatal("stored owner/canonical identity lost from log")
	}
}
func TestCanonicalWireIdentityIsUnchangedAndCannotSupplySinkIsolation(t *testing.T) {
	// Deliberate storage-layer collision control, not two ordinary globally
	// unique FACTs. Owner remains separate even when the canonical wire is equal.
	a, b := ownedEvent(t, "acme"), ownedEvent(t, "other")
	aw, err := json.Marshal(a.Event())
	if err != nil {
		t.Fatal(err)
	}
	bw, err := json.Marshal(b.Event())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(aw, bw) {
		t.Fatal("ownership silently changed canonical namespace/name/run identity")
	}
	if a.Owner() == b.Owner() {
		t.Fatal("typed emission ownership collapsed")
	}
	if _, err := openlineage.FromSnapshot(graph.ObservationSnapshot{}); err == nil {
		t.Fatal("zero snapshot appeared owned")
	}
}
