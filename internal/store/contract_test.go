package store

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// runContract checks a backend behaves as the Store interface promises, so every backend is held to
// the same tests.
func runContract(t *testing.T, open func(t *testing.T) Store) {
	t.Helper()
	t.Run("versions guard writes", func(t *testing.T) {
		ctx, backend := context.Background(), open(t)
		version, err := backend.Put(ctx, "members", "first", []byte(`{"a":1}`), 0)
		if err != nil || version != 1 {
			t.Fatalf("create: version %d, %v", version, err)
		}
		if _, err := backend.Put(ctx, "members", "first", []byte(`{"a":2}`), 0); !errors.Is(err, ErrConflict) {
			t.Fatalf("creating twice: %v", err)
		}
		if _, err := backend.Put(ctx, "members", "first", []byte(`{"a":2}`), 7); !errors.Is(err, ErrConflict) {
			t.Fatalf("stale version: %v", err)
		}
		if version, err := backend.Put(ctx, "members", "first", []byte(`{"a":2}`), 1); err != nil || version != 2 {
			t.Fatalf("update: version %d, %v", version, err)
		}
		if version, err := backend.Put(ctx, "members", "first", []byte(`{"a":3}`), AnyVersion); err != nil || version != 3 {
			t.Fatalf("overwrite: version %d, %v", version, err)
		}
		document, err := backend.Get(ctx, "members", "first")
		if err != nil || string(document.Data) != `{"a":3}` || document.Version != 3 {
			t.Fatalf("read back %q at %d, %v", document.Data, document.Version, err)
		}
		if err := backend.Delete(ctx, "members", "first", 1); !errors.Is(err, ErrConflict) {
			t.Fatalf("deleting a stale version: %v", err)
		}
		if err := backend.Delete(ctx, "members", "first", 3); err != nil {
			t.Fatal(err)
		}
		if _, err := backend.Get(ctx, "members", "first"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("after delete: %v", err)
		}
		if err := backend.Delete(ctx, "members", "first", AnyVersion); err != nil {
			t.Fatalf("deleting a missing document with AnyVersion: %v", err)
		}
	})

	t.Run("binary round trips", func(t *testing.T) {
		ctx, backend := context.Background(), open(t)
		image := []byte{0xFF, 0xD8, 0xFF, 0x00, 0x01, 0x02}
		if _, err := backend.Put(ctx, "avatars", "first", image, AnyVersion); err != nil {
			t.Fatal(err)
		}
		document, err := backend.Get(ctx, "avatars", "first")
		if err != nil || !slices.Equal(document.Data, image) {
			t.Fatalf("read back %v, %v", document.Data, err)
		}
	})

	t.Run("collections", func(t *testing.T) {
		ctx, backend := context.Background(), open(t)
		for _, collection := range []string{"weeks/2026-10-12", "weeks/2026-10-19", "routes/member/2026-10-19", "routes/member/2026-10-12"} {
			for _, id := range []string{"a", "b", "c"} {
				if _, err := backend.Put(ctx, collection, id, []byte(`{}`), 0); err != nil {
					t.Fatal(err)
				}
			}
		}
		challenges, err := backend.Collections(ctx, "weeks/")
		if err != nil || !slices.Equal(challenges, []string{"weeks/2026-10-12", "weeks/2026-10-19"}) {
			t.Fatalf("collections %v, %v", challenges, err)
		}
		members, err := backend.Collections(ctx, "routes/")
		if err != nil || !slices.Equal(members, []string{"routes/member"}) {
			t.Fatalf("collections beneath routes/ %v, %v", members, err)
		}
		items, err := backend.List(ctx, "weeks/2026-10-19")
		if err != nil || len(items) != 3 || items[0].ID != "a" || items[2].ID != "c" {
			t.Fatalf("list %v, %v", items, err)
		}
		if err := backend.DeleteCollection(ctx, "routes/member"); err != nil {
			t.Fatal(err)
		}
		if left, _ := backend.Collections(ctx, "routes/member/"); len(left) != 0 {
			t.Fatalf("nested collections survived: %v", left)
		}
		if left, _ := backend.List(ctx, "routes/member/2026-10-19"); len(left) != 0 {
			t.Fatalf("documents in a deleted collection survived: %v", left)
		}
	})

	t.Run("update retries", func(t *testing.T) {
		ctx, backend := context.Background(), open(t)
		for range 3 {
			err := Update(ctx, backend, "counters", "count", func(data []byte) ([]byte, error) {
				if data == nil {
					return []byte(`1`), nil
				}
				return []byte{data[0] + 1}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		document, _ := backend.Get(ctx, "counters", "count")
		if string(document.Data) != "3" || document.Version != 3 {
			t.Fatalf("count %q at version %d", document.Data, document.Version)
		}
	})

	t.Run("reserved collections are refused", func(t *testing.T) {
		ctx, backend := context.Background(), open(t)
		for _, collection := range []string{"_collections", "_collections/x", "_other"} {
			if _, err := backend.Put(ctx, collection, "id", []byte(`{}`), AnyVersion); err == nil {
				t.Errorf("accepted reserved collection %q", collection)
			}
			if _, err := backend.List(ctx, collection); err == nil {
				t.Errorf("listed reserved collection %q", collection)
			}
		}
		if _, err := backend.Collections(ctx, "_collections/"); err == nil {
			t.Error("accepted reserved prefix \"_collections/\"")
		}
	})

	t.Run("keys cannot escape", func(t *testing.T) {
		ctx, backend := context.Background(), open(t)
		for _, collection := range []string{"../outside", "weeks/../..", "", "weeks//x", "a b"} {
			if _, err := backend.Put(ctx, collection, "id", []byte(`{}`), AnyVersion); err == nil {
				t.Errorf("accepted collection %q", collection)
			}
		}
		for _, id := range []string{"..", "a/b", "", ".lock"} {
			if _, err := backend.Put(ctx, "members", id, []byte(`{}`), AnyVersion); err == nil {
				t.Errorf("accepted identifier %q", id)
			}
		}
	})
}
