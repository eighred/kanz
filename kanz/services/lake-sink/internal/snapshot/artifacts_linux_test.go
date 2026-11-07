//go:build linux

package snapshot

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSnapshotImmutablePublicationAndExactRetry(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()
	const name = "00000000-0000-4000-8000-000000000001-1"
	manifest, parquet := []byte(`{"fixture":1}`), []byte("bounded-owned-fixture")
	if err := publishArtifactFiles(ctx, root, name, manifest, parquet); err != nil {
		t.Fatal("initial immutable publication failed")
	}
	if err := publishArtifactFiles(ctx, root, name, manifest, parquet); err != nil {
		t.Fatal("exact publication retry refused")
	}
	if err := publishArtifactFiles(ctx, root, name, manifest, []byte("different")); err == nil {
		t.Fatal("same generation overwritten")
	}
	got, err := readArtifactFile(ctx, root, name, "events.parquet", MaxParquetBytes)
	if err != nil || string(got) != string(parquet) {
		t.Fatal("accepted generation changed")
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != name {
		t.Fatal("retry leaked another generation or staging output")
	}
	if err := publishArtifactFiles(ctx, root, "../escape-generation", manifest, parquet); err == nil {
		t.Fatal("unowned path accepted")
	}
}

func TestSnapshotPublicationRefusesStagingCollisionAndLinkedMembers(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()
	const name = "00000000-0000-4000-8000-000000000002-1"
	if err := os.Mkdir(filepath.Join(root, name+"-pending"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, name+"-pending", "sentinel"), []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if publishArtifactFiles(ctx, root, name, []byte("{}"), []byte("data")) == nil {
		t.Fatal("unknown partial generation reused")
	}
	got, err := os.ReadFile(filepath.Join(root, name+"-pending", "sentinel"))
	if err != nil || string(got) != "preserve" {
		t.Fatal("unknown partial files changed")
	}
	const linked = "00000000-0000-4000-8000-000000000002-2"
	if err := os.Mkdir(filepath.Join(root, linked), 0700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "source")
	if err := os.WriteFile(source, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	member := filepath.Join(root, linked, "events.parquet")
	if err := os.Symlink(source, member); err != nil {
		t.Fatal(err)
	}
	if _, err := readArtifactFile(ctx, root, linked, "events.parquet", MaxParquetBytes); err == nil {
		t.Fatal("symlink followed")
	}
	if err := os.Remove(member); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(source, member); err != nil {
		t.Fatal(err)
	}
	if _, err := readArtifactFile(ctx, root, linked, "events.parquet", MaxParquetBytes); err == nil {
		t.Fatal("hardlink accepted")
	}
}
