package snapshot

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
)

// Manifest is private registered provenance. No user path or URI is accepted;
// readers obtain this exact generation only from the authorized registry row.
type Manifest struct {
	SchemaVersion uint32   `json:"schema_version"`
	Tenant        string   `json:"tenant"`
	SnapshotID    string   `json:"snapshot_id"`
	SourceScope   uint32   `json:"source_scope"`
	RequestSHA256 [32]byte `json:"request_sha256"`
	CutSHA256     [32]byte `json:"cut_sha256"`
	ParquetSHA256 [32]byte `json:"parquet_sha256"`
	Rows          uint64   `json:"rows"`
	ParquetBytes  uint64   `json:"parquet_bytes"`
}

func (l *Lease) PreparePublication(parquet []byte, rows uint64) (Manifest, string, error) {
	if l.cut == nil || len(parquet) == 0 || len(parquet) > MaxParquetBytes || rows > MaxRows || l.row.generation < 1 {
		return Manifest{}, "", ErrInvalid
	}
	var requestHash [32]byte
	copy(requestHash[:], l.row.requestHash)
	manifest := Manifest{SchemaVersion: SchemaVersion, Tenant: l.store.tenant, SnapshotID: l.row.public.SnapshotId, SourceScope: 1, RequestSHA256: requestHash, CutSHA256: sha256.Sum256(l.cut), ParquetSHA256: sha256.Sum256(parquet), Rows: rows, ParquetBytes: uint64(len(parquet))}
	name := fmt.Sprintf("%s-%d", manifest.SnapshotID, l.row.generation)
	if !idPattern.MatchString(name) {
		return Manifest{}, "", ErrInvalid
	}
	return manifest, name, nil
}

// PublishArtifact fsyncs both members, the generation directory and its parent
// before the SQL READY transaction. No-replace rename never overwrites a prior
// generation. Any failed/uncertain publication is an unregistered orphan; this
// path does not silently delete it or make it queryable.
func PublishArtifact(ctx context.Context, root, name string, manifest Manifest, parquet []byte) (Publication, error) {
	if !idPattern.MatchString(name) || !boundedText(manifest.Tenant, 256) || manifest.SchemaVersion != SchemaVersion || manifest.SourceScope != 1 || manifest.Rows > MaxRows || len(parquet) == 0 || len(parquet) > MaxParquetBytes || uint64(len(parquet)) != manifest.ParquetBytes || sha256.Sum256(parquet) != manifest.ParquetSHA256 {
		return Publication{}, ErrInvalid
	}
	wire, err := json.Marshal(manifest)
	if err != nil || len(wire) > MaxControlBytes {
		return Publication{}, ErrCapacity
	}
	if err := publishArtifactFiles(ctx, root, name, wire, parquet); err != nil {
		return Publication{}, err
	}
	return Publication{Manifest: wire, ParquetSHA256: manifest.ParquetSHA256, OutputName: name, Rows: manifest.Rows, Bytes: manifest.ParquetBytes}, nil
}
