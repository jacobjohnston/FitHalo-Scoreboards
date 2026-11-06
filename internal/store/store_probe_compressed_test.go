package store_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"slices"
	"testing"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/store"
)

// openCompressedPair opens a directory store and the wrapper over it, returning both.
func openCompressedPair(t *testing.T) (*store.Directory, *store.Compressed) {
	t.Helper()
	directory, err := store.OpenDirectory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { directory.Close() })
	return directory, store.Compress(directory)
}

// TestCompressedNeverStoresLargerThanWritten checks the wrapper's stored form is at most the raw size plus a marker.
func TestCompressedNeverStoresLargerThanWritten(t *testing.T) {
	directory, wrapper := openCompressedPair(t)
	ctx := context.Background()
	for n := 0; n <= 64; n++ {
		random := randomBytes(t, n)
		mustPut(t, wrapper, "blobs", "r", random, store.AnyVersion)
		stored, err := directory.Get(ctx, "blobs", "r")
		if err != nil {
			t.Fatal(err)
		}
		// README: "compresses every document where compression makes the document smaller".
		if len(stored.Data) > n+1 {
			t.Errorf("%d random bytes stored as %d bytes", n, len(stored.Data))
		}
	}
}

// TestCompressedStoresCompressibleSmaller checks a repetitive document really is kept smaller.
func TestCompressedStoresCompressibleSmaller(t *testing.T) {
	directory, wrapper := openCompressedPair(t)
	repetitive := bytes.Repeat([]byte(`{"progress":0.84,"score":420,"goal":500}`), 200)
	mustPut(t, wrapper, "weeks", "w", repetitive, 0)
	stored, err := directory.Get(context.Background(), "weeks", "w")
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Data) >= len(repetitive) {
		t.Fatalf("%d repetitive bytes stored as %d bytes, want smaller", len(repetitive), len(stored.Data))
	}
	if document := mustGet(t, wrapper, "weeks", "w"); !bytes.Equal(document.Data, repetitive) {
		t.Fatal("compressed document did not round-trip")
	}
}

// TestCompressedEveryLengthAroundThreshold round-trips compressible and random documents of every small length.
func TestCompressedEveryLengthAroundThreshold(t *testing.T) {
	_, wrapper := openCompressedPair(t)
	for n := 0; n <= 300; n++ {
		for _, test := range []struct {
			name string
			data []byte
		}{
			{"zeros", make([]byte, n)},
			{"repeat", bytes.Repeat([]byte("a"), n)},
			{"random", randomBytes(t, n)},
			{"json", []byte(fmt.Sprintf(`{"n":%d,"pad":"%s"}`, n, bytes.Repeat([]byte("x"), max(0, n-20))))},
		} {
			mustPut(t, wrapper, "blobs", test.name, test.data, store.AnyVersion)
			if document := mustGet(t, wrapper, "blobs", test.name); !bytes.Equal(document.Data, test.data) {
				t.Fatalf("%s of %d bytes read back %d bytes", test.name, n, len(document.Data))
			}
		}
	}
}

// TestCompressedVersionsMatchBackend checks the wrapper reports the versions the backend holds.
func TestCompressedVersionsMatchBackend(t *testing.T) {
	directory, wrapper := openCompressedPair(t)
	var version int64
	for i := 0; i < 5; i++ {
		version = mustPut(t, wrapper, "weeks", "w", bytes.Repeat([]byte("w"), 1000*i), version)
		stored, err := directory.Get(context.Background(), "weeks", "w")
		if err != nil || stored.Version != version {
			t.Fatalf("backend v%d, wrapper said v%d, %v", stored.Version, version, err)
		}
	}
	items := mustList(t, wrapper, "weeks")
	if len(items) != 1 || items[0].Version != 5 || len(items[0].Data) != 4000 {
		t.Fatalf("List = %+v", items)
	}
}

// TestCompressedReadsPlainBackendDocument records what the wrapper returns for a document written beneath it.
func TestCompressedReadsPlainBackendDocument(t *testing.T) {
	directory, wrapper := openCompressedPair(t)
	// PROBE: a directory written by an unwrapped server and then opened through Compress is a migration
	// path the contract does not cover; the wrapper should not return silently different bytes.
	raw := []byte(`{"progress":0.5,"score":100}`)
	mustPut(t, directory, "weeks", "w", raw, 0)
	document, err := wrapper.Get(context.Background(), "weeks", "w")
	t.Logf("Compressed.Get of a plain backend document: %q, %v", document.Data, err)
	if err == nil && !bytes.Equal(document.Data, raw) {
		t.Errorf("wrapper returned %q for a plain backend document %q without an error", document.Data, raw)
	}
	items, err := wrapper.List(context.Background(), "weeks")
	t.Logf("Compressed.List over a plain backend document: %d items, %v", len(items), err)
}

// TestCompressedPassesThroughCollections checks Collections, List and DeleteCollection agree with the backend.
func TestCompressedPassesThroughCollections(t *testing.T) {
	directory, wrapper := openCompressedPair(t)
	ctx := context.Background()
	for _, collection := range []string{"weeks/a", "weeks/b", "weeks/b/c"} {
		mustPut(t, wrapper, collection, "d", bytes.Repeat([]byte("z"), 500), 0)
	}
	fromWrapper := mustCollections(t, wrapper, "weeks/")
	fromBackend := mustCollections(t, directory, "weeks/")
	if !slices.Equal(fromWrapper, fromBackend) || !slices.Equal(fromWrapper, []string{"weeks/a", "weeks/b"}) {
		t.Fatalf("Collections wrapper %q backend %q", fromWrapper, fromBackend)
	}
	if err := wrapper.DeleteCollection(ctx, "weeks/b"); err != nil {
		t.Fatal(err)
	}
	if got := mustCollections(t, directory, "weeks/"); !slices.Equal(got, []string{"weeks/a"}) {
		t.Fatalf("backend Collections after wrapper DeleteCollection = %q", got)
	}
	if items := mustList(t, directory, "weeks/b/c"); len(items) != 0 {
		t.Fatalf("nested documents survived DeleteCollection through the wrapper: %+v", items)
	}
}

// TestCompressedStoreFieldIsBackend checks the exported Store field is the wrapped backend.
func TestCompressedStoreFieldIsBackend(t *testing.T) {
	directory, wrapper := openCompressedPair(t)
	if wrapper.Store != store.Store(directory) {
		t.Fatal("Compressed.Store is not the backend handed to Compress")
	}
}

// TestCompressedBigRandomDocument round-trips five megabytes of random data through the wrapper.
func TestCompressedBigRandomDocument(t *testing.T) {
	directory, wrapper := openCompressedPair(t)
	random := randomBytes(t, 5<<20)
	mustPut(t, wrapper, "blobs", "big", random, 0)
	if document := mustGet(t, wrapper, "blobs", "big"); !bytes.Equal(document.Data, random) {
		t.Fatal("five megabytes of random data did not round-trip")
	}
	stored, err := directory.Get(context.Background(), "blobs", "big")
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Data) > len(random)+1 {
		t.Fatalf("random data grew from %d to %d bytes on disk", len(random), len(stored.Data))
	}
}

// TestCompressedRandomLengthsProperty round-trips many random lengths with random prefixes.
func TestCompressedRandomLengthsProperty(t *testing.T) {
	_, wrapper := openCompressedPair(t)
	for i := 0; i < 200; i++ {
		length, err := rand.Int(rand.Reader, big.NewInt(3000))
		if err != nil {
			t.Fatal(err)
		}
		prefix := randomBytes(t, 4)
		data := append(prefix, bytes.Repeat([]byte("ab"), int(length.Int64()))...)
		mustPut(t, wrapper, "blobs", "p", data, store.AnyVersion)
		if document := mustGet(t, wrapper, "blobs", "p"); !bytes.Equal(document.Data, data) {
			t.Fatalf("prefix %x with %d repeats read back %d bytes", prefix, length, len(document.Data))
		}
	}
}

// FuzzCompressedRoundTrip checks Put then Get through the wrapper returns identical bytes for any input.
func FuzzCompressedRoundTrip(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0x00})
	f.Add([]byte{0x01})
	f.Add([]byte{0x1f, 0x8b, 0x08})
	f.Add([]byte{0x78, 0x9c})
	f.Add([]byte(`{"progress":0.84,"score":420,"goal":500,"tier":"monster"}`))
	f.Add(bytes.Repeat([]byte("a"), 1000))
	f.Add([]byte{0xff, 0xfe, 0xc0, 0xaf})
	f.Fuzz(func(t *testing.T, data []byte) {
		directory, err := store.OpenDirectory(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer directory.Close()
		wrapper := store.Compress(directory)
		ctx := context.Background()
		if _, err := wrapper.Put(ctx, "fuzz", "d", data, 0); err != nil {
			t.Fatalf("Put: %v", err)
		}
		document, err := wrapper.Get(ctx, "fuzz", "d")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if !bytes.Equal(document.Data, data) {
			t.Fatalf("round trip of %d bytes (%x...) returned %d bytes (%x...)", len(data), head(data), len(document.Data), head(document.Data))
		}
		items, err := wrapper.List(ctx, "fuzz")
		if err != nil || len(items) != 1 || !bytes.Equal(items[0].Data, data) {
			t.Fatalf("List: %d items, %v", len(items), err)
		}
	})
}

// FuzzDirectoryRoundTrip checks the plain directory backend returns identical bytes for any input.
func FuzzDirectoryRoundTrip(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0x00})
	f.Add([]byte(`{"a":1}`))
	f.Add([]byte{0xff, 0xfe})
	// Crashers the fuzzer found: documents that parse as JSON are not stored verbatim.
	f.Add([]byte(" 0"))
	f.Add([]byte(`{"a": 1}`))
	f.Add([]byte(`"<"`))
	f.Fuzz(func(t *testing.T, data []byte) {
		directory, err := store.OpenDirectory(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer directory.Close()
		ctx := context.Background()
		if _, err := directory.Put(ctx, "fuzz", "d", data, 0); err != nil {
			t.Fatalf("Put: %v", err)
		}
		document, err := directory.Get(ctx, "fuzz", "d")
		if err != nil || !bytes.Equal(document.Data, data) {
			t.Fatalf("Get of %d bytes returned %d bytes, %v", len(data), len(document.Data), err)
		}
	})
}
