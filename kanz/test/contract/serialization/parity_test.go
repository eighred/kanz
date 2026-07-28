// Cross-language serialization parity (EVT-21b), Go side.
//
// The Go test asserts that:
//   - Every fixture round-trips Marshal → Unframe with byte-identical
//     payload and field-identical envelope.
//   - Every fixture passes bus.Validate after the round-trip — the wire
//     bytes the Python and TS tests load satisfy the envelope contract
//     established by EVT-21a.
//   - The committed fixtures/*.bin + manifest.json on disk match what
//     BuildAll() produces today. Drift fails the test loudly with the
//     regeneration command, rather than silently passing while Python /
//     TS load stale fixtures.
//
// The on-disk check is optional: if the fixtures directory is absent
// the test runs the in-memory assertions only and reports a clear
// "run genfixtures" skip note. CI invokes genfixtures before tests so
// the on-disk path always runs there.
package serialization_test

import (
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/test/contract/serialization"
	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
)

const fixturesDir = "fixtures"

// TestRoundTripParity is the in-memory parity assertion: marshal every
// fixture, unframe it, validate the envelope, and check field-by-field
// equality. This is the contract every cross-language test (Python, TS)
// asserts against the same wire bytes.
func TestRoundTripParity(t *testing.T) {
	m, err := serialization.BuildAll()
	if err != nil {
		t.Fatalf("BuildAll: %v", err)
	}
	for _, f := range m.Fixtures {
		t.Run(f.Name, func(t *testing.T) {
			env, payload, err := bus.Unframe(f.WireBytes)
			if err != nil {
				t.Fatalf("Unframe: %v", err)
			}
			if err := bus.Validate(env); err != nil {
				t.Fatalf("Validate after round-trip: %v", err)
			}
			assertEnvelopeMatches(t, f.Envelope, env)
			wantPayload, err := hex.DecodeString(f.PayloadHex)
			if err != nil {
				t.Fatalf("payload hex decode: %v", err)
			}
			if !bytes.Equal(payload, wantPayload) {
				t.Errorf("payload mismatch: got %x want %s", payload, f.PayloadHex)
			}
		})
	}
}

// TestCommittedFixturesMatchGenerator is the drift guard: if a fixture
// .bin or the manifest on disk diverges from what BuildAll() produces,
// the test fails with the regen command. Skips when the directory is
// absent (dev hasn't run the generator yet).
func TestCommittedFixturesMatchGenerator(t *testing.T) {
	// The .bin/manifest.json are generator output (CI runs genfixtures before
	// the test; only the README is committed). Skip when they're absent —
	// the directory itself exists for the README, so probe the manifest, not
	// the directory. Mirrors the Python/TS suites' skip-if-absent behavior.
	if _, err := os.Stat(filepath.Join(fixturesDir, "manifest.json")); os.IsNotExist(err) {
		t.Skipf("%s/ not generated — run: go run ./cmd/genfixtures -out %s", fixturesDir, fixturesDir)
	}
	m, err := serialization.BuildAll()
	if err != nil {
		t.Fatalf("BuildAll: %v", err)
	}
	for _, f := range m.Fixtures {
		t.Run(f.Name, func(t *testing.T) {
			got, err := os.ReadFile(filepath.Join(fixturesDir, f.File))
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			if !bytes.Equal(got, f.WireBytes) {
				t.Errorf("committed %s diverged from generator output — run: go run ./cmd/genfixtures -out %s", f.File, fixturesDir)
			}
		})
	}
}

// assertEnvelopeMatches compares the round-tripped proto envelope against the
// manifest's language-agnostic EnvelopeFields — the same field description
// every cross-language parity test (Python, TS) asserts against, so the
// contract is wire-form, not Go-specific (enums as string names, timestamps
// split into seconds/nanos).
func assertEnvelopeMatches(t *testing.T, want serialization.EnvelopeFields, got *envelopepb.Envelope) {
	t.Helper()
	if got.EventId != want.EventID {
		t.Errorf("EventId=%q want %q", got.EventId, want.EventID)
	}
	if got.EventType != want.EventType {
		t.Errorf("EventType=%q want %q", got.EventType, want.EventType)
	}
	if got.SchemaVersion != want.SchemaVersion {
		t.Errorf("SchemaVersion=%d want %d", got.SchemaVersion, want.SchemaVersion)
	}
	if got.EnvelopeVersion != want.EnvelopeVersion {
		t.Errorf("EnvelopeVersion=%d want %d", got.EnvelopeVersion, want.EnvelopeVersion)
	}
	if got.EventClass.String() != want.EventClass {
		t.Errorf("EventClass=%v want %v", got.EventClass.String(), want.EventClass)
	}
	if got.Domain != want.Domain {
		t.Errorf("Domain=%q want %q", got.Domain, want.Domain)
	}
	if !tsMatches(got.EventTime, want.EventTime) {
		t.Errorf("EventTime=%v want %v", got.EventTime, want.EventTime)
	}
	if !tsMatches(got.IngestionTime, want.IngestionTime) {
		t.Errorf("IngestionTime=%v want %v", got.IngestionTime, want.IngestionTime)
	}
	if !tsMatches(got.PublishTime, want.PublishTime) {
		t.Errorf("PublishTime=%v want %v", got.PublishTime, want.PublishTime)
	}
	if got.CorrelationId != want.CorrelationID {
		t.Errorf("CorrelationId=%q want %q", got.CorrelationId, want.CorrelationID)
	}
	if got.CausationId != want.CausationID {
		t.Errorf("CausationId=%q want %q", got.CausationId, want.CausationID)
	}
	if got.TraceContext != want.TraceContext {
		t.Errorf("TraceContext=%q want %q", got.TraceContext, want.TraceContext)
	}
	if got.Source != want.Source {
		t.Errorf("Source=%q want %q", got.Source, want.Source)
	}
	if got.ProducerVersion != want.ProducerVersion {
		t.Errorf("ProducerVersion=%q want %q", got.ProducerVersion, want.ProducerVersion)
	}
	if got.PartitionKey != want.PartitionKey {
		t.Errorf("PartitionKey=%q want %q", got.PartitionKey, want.PartitionKey)
	}
	if got.ProducerSequence != want.ProducerSequence {
		t.Errorf("ProducerSequence=%d want %d", got.ProducerSequence, want.ProducerSequence)
	}
	if got.IdempotencyKey != want.IdempotencyKey {
		t.Errorf("IdempotencyKey=%q want %q", got.IdempotencyKey, want.IdempotencyKey)
	}
	if got.PayloadSchemaRef != want.PayloadSchemaRef {
		t.Errorf("PayloadSchemaRef=%q want %q", got.PayloadSchemaRef, want.PayloadSchemaRef)
	}
	if len(got.QualityFlags) != len(want.QualityFlags) {
		t.Errorf("QualityFlags len=%d want %d", len(got.QualityFlags), len(want.QualityFlags))
		return
	}
	for i := range want.QualityFlags {
		if got.QualityFlags[i].String() != want.QualityFlags[i] {
			t.Errorf("QualityFlags[%d]=%v want %v", i, got.QualityFlags[i].String(), want.QualityFlags[i])
		}
	}
}

// tsMatches compares a proto Timestamp against the manifest's split
// seconds/nanos form. A nil proto timestamp reads as the zero {0,0}, matching
// serialization.tsFromPB's nil handling.
func tsMatches(got *timestamppb.Timestamp, want serialization.TimeStamp) bool {
	var s int64
	var n int32
	if got != nil {
		s, n = got.Seconds, got.Nanos
	}
	return s == want.Seconds && n == want.Nanos
}
