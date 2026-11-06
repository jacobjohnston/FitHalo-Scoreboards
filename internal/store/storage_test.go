package store

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

// storageFixtures holds a directory store exactly as the 1.0 server leaves it, with each document's bytes
// as written beside it in expected; see docs/specifications.md.
const storageFixtures = "testdata/storage-1"

// TestStorageFixtures reads the 1.0 directory store, with documents in both formats, back to the bytes
// written and the versions they had. Packing may change; reading what 1.0 packed may not.
func TestStorageFixtures(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.CopyFS(root, os.DirFS(filepath.Join(storageFixtures, "directory"))); err != nil {
		t.Fatal(err)
	}
	directory, err := OpenDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { directory.Close() })
	backend := Compress(directory)

	formats := map[byte]bool{}
	documents := []struct {
		// collection and id are where the document is kept.
		collection, id string

		// version is the document's stored version.
		version int64
	}{
		{"board", "board", 1},
		{"members", "WJlWhMsgEjcbPz3g3zLheA", 2},
		{"avatars", "WJlWhMsgEjcbPz3g3zLheA", 1},
	}
	for _, document := range documents {
		packed, err := directory.Get(ctx, document.collection, document.id)
		if err != nil {
			t.Fatal(err)
		}
		formats[packed.Data[0]] = true
		got, err := backend.Get(ctx, document.collection, document.id)
		if err != nil {
			t.Fatalf("%s/%s: %v", document.collection, document.id, err)
		}
		want, err := os.ReadFile(filepath.Join(storageFixtures, "expected", document.collection, document.id))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got.Data, want) || got.Version != document.version {
			t.Errorf("%s/%s: got version %d, %q; want version %d, %q", document.collection, document.id, got.Version, got.Data, document.version, want)
		}
	}
	if !formats[formatPlain] || !formats[formatDeflate1] {
		t.Errorf("the fixtures cover formats %v, want both %d and %d", formats, formatPlain, formatDeflate1)
	}
	if items, err := backend.List(ctx, "members"); err != nil || len(items) != 1 {
		t.Errorf("listing members: %d items, %v", len(items), err)
	}
}
