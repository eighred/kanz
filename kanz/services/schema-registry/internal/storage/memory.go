package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Memory is an in-memory Storage for tests. Loses everything on restart.
type Memory struct {
	mu      sync.RWMutex
	schemas map[Ref]Schema
}

func NewMemory() *Memory { return &Memory{schemas: make(map[Ref]Schema)} }

func (m *Memory) Put(_ context.Context, s Schema) error {
	if err := validatePut(&s); err != nil {
		return err
	}
	if s.CreatedAt.IsZero() {
		s.CreatedAt = time.Now().UTC()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.schemas[s.Ref]; ok {
		if existing.Fingerprint != s.Fingerprint {
			return fmt.Errorf("%w: %s (have %s, got %s)", ErrConflict, s.Ref, existing.Fingerprint, s.Fingerprint)
		}
		return nil
	}
	m.schemas[s.Ref] = s
	return nil
}

func (m *Memory) Get(_ context.Context, r Ref) (Schema, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.schemas[r]
	if !ok {
		return Schema{}, ErrNotFound
	}
	return s, nil
}

func (m *Memory) Register(_ context.Context, schemaID string, descriptor []byte, sourceTag string) (Ref, bool, error) {
	if schemaID == "" {
		return Ref{}, false, errors.New("schema id is empty")
	}
	if len(descriptor) == 0 {
		return Ref{}, false, errors.New("schema descriptor is empty")
	}
	sum := sha256.Sum256(descriptor)
	fingerprint := hex.EncodeToString(sum[:])

	m.mu.Lock()
	defer m.mu.Unlock()

	var latestVer uint64
	var latestFingerprint string
	for ref, s := range m.schemas {
		if ref.SchemaID != schemaID {
			continue
		}
		if ref.Version > latestVer {
			latestVer = ref.Version
			latestFingerprint = s.Fingerprint
		}
	}
	if latestVer > 0 && latestFingerprint == fingerprint {
		return Ref{SchemaID: schemaID, Version: latestVer}, false, nil
	}
	newRef := Ref{SchemaID: schemaID, Version: latestVer + 1}
	m.schemas[newRef] = Schema{
		Ref:         newRef,
		Descriptor:  append([]byte(nil), descriptor...),
		Fingerprint: fingerprint,
		SourceTag:   sourceTag,
		CreatedAt:   time.Now().UTC(),
	}
	return newRef, true, nil
}

func (m *Memory) Ping(_ context.Context) error { return nil }
