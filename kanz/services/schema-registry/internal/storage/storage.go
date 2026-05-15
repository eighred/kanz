// Package storage is the schema-registry persistence boundary.
//
// One row per (schema_id, version); the pair is immutable once published —
// see kanz-schemas/docs/schema-evolution.md §6. Backends are pluggable so
// tests can drive the same suite against Postgres and an in-memory fake.
package storage

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schema is the persisted form of a payload schema version.
type Schema struct {
	Ref         Ref
	Descriptor  []byte    // opaque schema bytes (typically a FileDescriptorSet subset)
	Fingerprint string    // sha256 hex of Descriptor; derived in Put when empty
	SourceTag   string    // kanz-schemas release tag this version was ingested from
	CreatedAt   time.Time
}

// Ref is the parsed form of an envelope payload_schema_ref. Wire form is
// "<schema-id>:<version>", e.g. "market.v1.MarketDataEvent:7". Version is
// monotonic per schema-id and bumps on every change, breaking or not
// (kanz-schemas/docs/schema-evolution.md §6).
type Ref struct {
	SchemaID string
	Version  uint64
}

func ParseRef(s string) (Ref, error) {
	id, ver, ok := strings.Cut(s, ":")
	if !ok || id == "" || ver == "" {
		return Ref{}, fmt.Errorf("invalid schema ref %q: expected <schema-id>:<version>", s)
	}
	v, err := strconv.ParseUint(ver, 10, 64)
	if err != nil {
		return Ref{}, fmt.Errorf("invalid schema ref %q: version is not a positive integer", s)
	}
	if v == 0 {
		return Ref{}, fmt.Errorf("invalid schema ref %q: version must be >= 1", s)
	}
	return Ref{SchemaID: id, Version: v}, nil
}

func (r Ref) String() string { return fmt.Sprintf("%s:%d", r.SchemaID, r.Version) }

// Storage is the persistence interface every backend implements.
//
// Put is idempotent: re-putting the same (Ref, Descriptor) succeeds; putting
// a different Descriptor under an existing Ref returns ErrConflict. The
// registry version line in payload_schema_ref is immutable once published.
//
// Register is the ingest entry point — the caller supplies the schema-id and
// descriptor bytes; the registry assigns the version. If the latest stored
// version for the schema-id already has an identical fingerprint, Register
// returns that ref with created=false; otherwise it inserts a new row at
// latest+1 and returns created=true. This is the contract relied on by
// kanz-schemas's release CI (EVT-16b): re-running a release is a no-op.
type Storage interface {
	Put(ctx context.Context, s Schema) error
	Register(ctx context.Context, schemaID string, descriptor []byte, sourceTag string) (Ref, bool, error)
	Get(ctx context.Context, r Ref) (Schema, error)
	Ping(ctx context.Context) error
}

var (
	ErrNotFound = errors.New("schema not found")
	ErrConflict = errors.New("schema version exists with different content")
)
