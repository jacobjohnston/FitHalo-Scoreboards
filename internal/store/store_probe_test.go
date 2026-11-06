package store_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/store"
)

// backend names a store under probe and opens a fresh one.
type backend struct {
	// name labels the backend in subtests.
	name string

	// open returns a fresh, empty store closed at the end of the test.
	open func(t *testing.T) store.Store
}

// openDirectory opens a directory store in a fresh temporary directory.
func openDirectory(t *testing.T) store.Store {
	t.Helper()
	directory, err := store.OpenDirectory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { directory.Close() })
	return directory
}

// openCompressed wraps a fresh directory store in the compressing wrapper.
func openCompressed(t *testing.T) store.Store {
	t.Helper()
	return store.Compress(openDirectory(t))
}

// backends lists the two backends every probe runs against.
var backends = []backend{{"directory", openDirectory}, {"compressed", openCompressed}}

// forEachBackend runs probe as a subtest against every backend.
func forEachBackend(t *testing.T, probe func(t *testing.T, s store.Store)) {
	t.Helper()
	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) { probe(t, b.open(t)) })
	}
}

// mustPut writes a document and fails the test on error.
func mustPut(t *testing.T, s store.Store, collection, id string, data []byte, version int64) int64 {
	t.Helper()
	got, err := s.Put(context.Background(), collection, id, data, version)
	if err != nil {
		t.Fatalf("Put(%q, %q, v%d): %v", collection, id, version, err)
	}
	return got
}

// mustGet reads a document and fails the test on error.
func mustGet(t *testing.T, s store.Store, collection, id string) store.Document {
	t.Helper()
	document, err := s.Get(context.Background(), collection, id)
	if err != nil {
		t.Fatalf("Get(%q, %q): %v", collection, id, err)
	}
	return document
}

// mustList lists a collection and fails the test on error.
func mustList(t *testing.T, s store.Store, collection string) []store.Item {
	t.Helper()
	items, err := s.List(context.Background(), collection)
	if err != nil {
		t.Fatalf("List(%q): %v", collection, err)
	}
	return items
}

// mustCollections lists the collections beneath prefix and fails the test on error.
func mustCollections(t *testing.T, s store.Store, prefix string) []string {
	t.Helper()
	collections, err := s.Collections(context.Background(), prefix)
	if err != nil {
		t.Fatalf("Collections(%q): %v", prefix, err)
	}
	return collections
}

// randomBytes returns n bytes from crypto/rand.
func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	data := make([]byte, n)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	return data
}

// gzipped returns data as a valid gzip stream.
func gzipped(t *testing.T, data []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// TestVersionsStartAtOneAndIncrementByOne checks each Put advances the version by exactly one.
func TestVersionsStartAtOneAndIncrementByOne(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		var expected int64
		for i := 0; i < 12; i++ {
			got := mustPut(t, s, "members", "m", []byte(fmt.Sprintf(`{"n":%d}`, i)), expected)
			expected++
			if got != expected {
				t.Fatalf("Put %d returned version %d, want %d", i, got, expected)
			}
			if document := mustGet(t, s, "members", "m"); document.Version != expected {
				t.Fatalf("Get after Put %d read version %d, want %d", i, document.Version, expected)
			}
		}
	})
}

// TestPutZeroCreatesOnly checks a second create of the same document is a conflict that changes nothing.
func TestPutZeroCreatesOnly(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		mustPut(t, s, "members", "m", []byte("one"), 0)
		if _, err := s.Put(context.Background(), "members", "m", []byte("two"), 0); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("second create: %v, want ErrConflict", err)
		}
		if document := mustGet(t, s, "members", "m"); string(document.Data) != "one" || document.Version != 1 {
			t.Fatalf("after conflicting create: %q at v%d", document.Data, document.Version)
		}
	})
}

// TestPutWrongVersionLeavesDocumentUnchanged checks every mismatched version is refused without a write.
func TestPutWrongVersionLeavesDocumentUnchanged(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		mustPut(t, s, "members", "m", []byte("one"), 0)
		mustPut(t, s, "members", "m", []byte("two"), 1)
		for _, wrong := range []int64{0, 1, 3, 4, 1000, math.MaxInt64, -2, math.MinInt64} {
			_, err := s.Put(context.Background(), "members", "m", []byte("bad"), wrong)
			if !errors.Is(err, store.ErrConflict) {
				t.Errorf("Put with version %d: %v, want ErrConflict", wrong, err)
			}
			if document := mustGet(t, s, "members", "m"); string(document.Data) != "two" || document.Version != 2 {
				t.Fatalf("after Put with version %d: %q at v%d", wrong, document.Data, document.Version)
			}
		}
	})
}

// TestPutAnyVersionCreatesAndOverwrites checks AnyVersion works on missing and present documents alike.
func TestPutAnyVersionCreatesAndOverwrites(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		if got := mustPut(t, s, "members", "m", []byte("one"), store.AnyVersion); got != 1 {
			t.Errorf("AnyVersion create returned version %d, want 1", got)
		}
		if got := mustPut(t, s, "members", "m", []byte("two"), store.AnyVersion); got != 2 {
			t.Errorf("AnyVersion overwrite returned version %d, want 2", got)
		}
		if document := mustGet(t, s, "members", "m"); string(document.Data) != "two" || document.Version != 2 {
			t.Fatalf("after overwrite: %q at v%d", document.Data, document.Version)
		}
	})
}

// TestDeleteWrongVersionConflicts checks a stale delete is refused and keeps the document.
func TestDeleteWrongVersionConflicts(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		mustPut(t, s, "members", "m", []byte("one"), 0)
		for _, wrong := range []int64{0, 2, -2, math.MaxInt64} {
			if err := s.Delete(context.Background(), "members", "m", wrong); !errors.Is(err, store.ErrConflict) {
				t.Errorf("Delete with version %d: %v, want ErrConflict", wrong, err)
			}
			if document := mustGet(t, s, "members", "m"); string(document.Data) != "one" || document.Version != 1 {
				t.Fatalf("after stale delete %d: %q at v%d", wrong, document.Data, document.Version)
			}
		}
		if err := s.Delete(context.Background(), "members", "m", 1); err != nil {
			t.Fatalf("Delete with the right version: %v", err)
		}
		if _, err := s.Get(context.Background(), "members", "m"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("after delete: %v, want ErrNotFound", err)
		}
	})
}

// TestDeleteMissingDocument checks AnyVersion tolerates absence and a specific version reports it.
func TestDeleteMissingDocument(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		if err := s.Delete(context.Background(), "members", "ghost", store.AnyVersion); err != nil {
			t.Errorf("Delete missing with AnyVersion: %v, want nil", err)
		}
		// PROBE: the contract only promises AnyVersion is fine; a specific version should at least be an error.
		err := s.Delete(context.Background(), "members", "ghost", 1)
		if !errors.Is(err, store.ErrNotFound) && !errors.Is(err, store.ErrConflict) {
			t.Errorf("Delete missing with version 1: %v, want ErrNotFound or ErrConflict", err)
		}
		t.Logf("Delete missing with version 1 reports %v", err)
		err = s.Delete(context.Background(), "members", "ghost", 0)
		if !errors.Is(err, store.ErrNotFound) && !errors.Is(err, store.ErrConflict) {
			t.Errorf("Delete missing with version 0: %v, want ErrNotFound or ErrConflict", err)
		}
	})
}

// TestGetMissingDocument checks a missing document is ErrNotFound with a zero Document.
func TestGetMissingDocument(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		document, err := s.Get(context.Background(), "members", "ghost")
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("Get missing: %v, want ErrNotFound", err)
		}
		if document.Data != nil || document.Version != 0 {
			t.Errorf("Get missing returned %q at v%d, want nil at 0", document.Data, document.Version)
		}
		mustPut(t, s, "members", "other", []byte("x"), 0)
		if _, err := s.Get(context.Background(), "members", "ghost"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("Get missing beside a present document: %v", err)
		}
	})
}

// TestVersionRestartsAfterDelete checks a document can be created again after deletion.
func TestVersionRestartsAfterDelete(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		mustPut(t, s, "members", "m", []byte("one"), 0)
		mustPut(t, s, "members", "m", []byte("two"), 1)
		if err := s.Delete(context.Background(), "members", "m", 2); err != nil {
			t.Fatal(err)
		}
		got := mustPut(t, s, "members", "m", []byte("three"), 0)
		// PROBE: the contract says versions start at 1; it does not say whether they restart after a delete.
		if got != 1 {
			t.Logf("version after delete and re-create is %d, not 1", got)
		}
		if document := mustGet(t, s, "members", "m"); string(document.Data) != "three" || document.Version != got {
			t.Fatalf("after re-create: %q at v%d, Put said v%d", document.Data, document.Version, got)
		}
	})
}

// TestEmptyDocumentRoundTrips checks a zero-length document is stored and read back as zero-length.
func TestEmptyDocumentRoundTrips(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		for _, test := range []struct {
			name string
			data []byte
		}{
			{"empty", []byte{}},
			{"nil", nil},
		} {
			t.Run(test.name, func(t *testing.T) {
				if got := mustPut(t, s, "members", test.name, test.data, 0); got != 1 {
					t.Fatalf("version %d", got)
				}
				document := mustGet(t, s, "members", test.name)
				if len(document.Data) != 0 || document.Version != 1 {
					t.Fatalf("read back %q at v%d", document.Data, document.Version)
				}
				// PROBE: Document.Data is documented nil "when the document does not exist"; an existing
				// empty document arguably should read back non-nil.
				if document.Data == nil {
					t.Logf("empty document %s read back with nil Data", test.name)
				}
				items := mustList(t, s, "members")
				index := slices.IndexFunc(items, func(item store.Item) bool { return item.ID == test.name })
				if index < 0 || len(items[index].Data) != 0 || items[index].Version != 1 {
					t.Fatalf("List shows %+v", items)
				}
			})
		}
	})
}

// roundTripCase is a document that must be read back exactly as written.
type roundTripCase struct {
	// name labels the case.
	name string

	// data is the document written.
	data []byte
}

// roundTripCases builds the adversarial payloads for the round-trip probes.
func roundTripCases(t *testing.T) []roundTripCase {
	t.Helper()
	compressible := bytes.Repeat([]byte("abcabcabc"), 100000)
	different := []byte(`{"other":"document","that":"must not be returned"}`)
	return []roundTripCase{
		{"one byte", []byte{0x00}},
		{"one printable byte", []byte("a")},
		{"two bytes", []byte{0x1f, 0x8b}},
		{"five megabytes random", randomBytes(t, 5<<20)},
		{"random 4k", randomBytes(t, 4096)},
		{"highly compressible", compressible},
		{"gzip magic then random", append([]byte{0x1f, 0x8b, 0x08}, randomBytes(t, 300)...)},
		{"zlib header then random", append([]byte{0x78, 0x9c}, randomBytes(t, 300)...)},
		{"zstd magic then random", append([]byte{0x28, 0xb5, 0x2f, 0xfd}, randomBytes(t, 300)...)},
		{"raw deflate block then text", append([]byte{0x01, 0x05, 0x00, 0xfa, 0xff}, []byte("hello")...)},
		{"valid gzip of a different document", gzipped(t, different)},
		{"valid gzip of compressible text", gzipped(t, compressible[:4096])},
		{"invalid utf-8", []byte{0xff, 0xfe, 0xc0, 0xaf, 0xed, 0xa0, 0x80}},
		{"nul bytes", bytes.Repeat([]byte{0}, 1000)},
		{"json with control characters", []byte("{\"a\":\"\x00\x01\x1f\"}")},
		{"single 0xff", []byte{0xff}},
	}
}

// TestDataRoundTripsExactly checks every payload is returned byte for byte by Get and List.
func TestDataRoundTripsExactly(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		for _, test := range roundTripCases(t) {
			t.Run(test.name, func(t *testing.T) {
				mustPut(t, s, "blobs", "b", test.data, store.AnyVersion)
				document := mustGet(t, s, "blobs", "b")
				if !bytes.Equal(document.Data, test.data) {
					t.Fatalf("Get read back %d bytes, want %d; first bytes %x vs %x",
						len(document.Data), len(test.data), head(document.Data), head(test.data))
				}
				items := mustList(t, s, "blobs")
				if len(items) != 1 || !bytes.Equal(items[0].Data, test.data) {
					t.Fatalf("List read back %d items, first of %d bytes", len(items), len(items[0].Data))
				}
			})
		}
	})
}

// head returns at most the first eight bytes of data.
func head(data []byte) []byte {
	return data[:min(8, len(data))]
}

// TestEveryLeadingByteRoundTrips checks no first byte is mistaken for a compression marker.
func TestEveryLeadingByteRoundTrips(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		tail := randomBytes(t, 64)
		for first := 0; first < 256; first++ {
			for _, suffix := range [][]byte{nil, tail, bytes.Repeat([]byte("x"), 500)} {
				data := append([]byte{byte(first)}, suffix...)
				mustPut(t, s, "blobs", "b", data, store.AnyVersion)
				if document := mustGet(t, s, "blobs", "b"); !bytes.Equal(document.Data, data) {
					t.Fatalf("leading byte %#02x with %d-byte suffix read back %x", first, len(suffix), head(document.Data))
				}
			}
		}
	})
}

// TestPutDoesNotAliasCallerSlice checks mutating the slice after Put leaves the document unchanged.
func TestPutDoesNotAliasCallerSlice(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		data := []byte("original document")
		mustPut(t, s, "members", "m", data, 0)
		copy(data, "MUTATED!")
		if document := mustGet(t, s, "members", "m"); string(document.Data) != "original document" {
			t.Fatalf("Put aliased the caller's slice: %q", document.Data)
		}
	})
}

// TestGetDoesNotAliasStoredSlice checks mutating the slice from Get or List leaves the document unchanged.
func TestGetDoesNotAliasStoredSlice(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		mustPut(t, s, "members", "m", []byte("original document"), 0)
		first := mustGet(t, s, "members", "m")
		copy(first.Data, "MUTATED!")
		if second := mustGet(t, s, "members", "m"); string(second.Data) != "original document" {
			t.Fatalf("Get aliased the stored document: %q", second.Data)
		}
		items := mustList(t, s, "members")
		copy(items[0].Data, "MUTATED!")
		if third := mustGet(t, s, "members", "m"); string(third.Data) != "original document" {
			t.Fatalf("List aliased the stored document: %q", third.Data)
		}
	})
}

// TestListReturnsByteOrder checks identifier order is plain byte order, not case-folded or locale order.
func TestListReturnsByteOrder(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		// Pairs differing only in case are left out; TestCaseDifferentIDsAreDistinctDocuments probes those.
		ids := []string{"a", "B", "_", "-", "0", "Z", "aa", "a-", "a_", "9", "ab", "a0", "b-", "b_", "b0"}
		for _, id := range ids {
			mustPut(t, s, "ids", id, []byte(id), 0)
		}
		items := mustList(t, s, "ids")
		var got []string
		for _, item := range items {
			got = append(got, item.ID)
			if string(item.Data) != item.ID {
				t.Errorf("item %q carries data %q", item.ID, item.Data)
			}
		}
		want := slices.Clone(ids)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Fatalf("List order %q, want byte order %q", got, want)
		}
	})
}

// skipOnCaseInsensitiveFilesystem skips a test on a filesystem that folds case, such as macOS's APFS default,
// where ids differing only in case share a file. Accepted: identifiers are SHA-256 derived, so a case-only
// collision is neither likely (about one in 10^34) nor choosable, and the hosted platforms are case-sensitive.
func skipOnCaseInsensitiveFilesystem(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "A"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "a")); err == nil {
		t.Skip("the filesystem folds case; accepted for the directory store")
	}
}

// TestCaseDifferentIDsAreDistinctDocuments checks ids that differ only in case are different documents.
func TestCaseDifferentIDsAreDistinctDocuments(t *testing.T) {
	skipOnCaseInsensitiveFilesystem(t)
	forEachBackend(t, func(t *testing.T, s store.Store) {
		mustPut(t, s, "ids", "a", []byte("lower"), 0)
		if _, err := s.Put(context.Background(), "ids", "A", []byte("upper"), 0); err != nil {
			t.Fatalf("creating %q beside %q: %v", "A", "a", err)
		}
		if document := mustGet(t, s, "ids", "a"); string(document.Data) != "lower" {
			t.Fatalf("Get(a) = %q after writing A", document.Data)
		}
		if document := mustGet(t, s, "ids", "A"); string(document.Data) != "upper" {
			t.Fatalf("Get(A) = %q", document.Data)
		}
		if items := mustList(t, s, "ids"); len(items) != 2 {
			t.Fatalf("List = %+v, want two documents", items)
		}
	})
}

// TestCaseDifferentCollectionsAreDistinct checks collections that differ only in case are different collections.
func TestCaseDifferentCollectionsAreDistinct(t *testing.T) {
	skipOnCaseInsensitiveFilesystem(t)
	forEachBackend(t, func(t *testing.T, s store.Store) {
		mustPut(t, s, "top/weeks", "d", []byte("lower"), 0)
		if _, err := s.Put(context.Background(), "top/Weeks", "d", []byte("upper"), 0); err != nil {
			t.Fatalf("creating top/Weeks beside top/weeks: %v", err)
		}
		if got := mustCollections(t, s, "top/"); !slices.Equal(got, []string{"top/Weeks", "top/weeks"}) {
			t.Fatalf("Collections(top/) = %q", got)
		}
		if document := mustGet(t, s, "top/weeks", "d"); string(document.Data) != "lower" {
			t.Fatalf("Get(top/weeks) = %q", document.Data)
		}
	})
}

// TestListMissingCollection checks listing a collection that was never written is empty, not an error.
func TestListMissingCollection(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		// PROBE: the contract does not say whether a missing collection lists as empty or errors.
		items, err := s.List(context.Background(), "never/written")
		if err != nil {
			t.Fatalf("List missing collection: %v", err)
		}
		if len(items) != 0 {
			t.Fatalf("List missing collection returned %d items", len(items))
		}
	})
}

// TestListCarriesCurrentVersions checks each listed item reports the version Get would.
func TestListCarriesCurrentVersions(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		for i := 0; i < 5; i++ {
			id := fmt.Sprintf("m%d", i)
			var version int64
			for j := 0; j <= i; j++ {
				version = mustPut(t, s, "members", id, []byte(fmt.Sprintf("%d/%d", i, j)), version)
			}
		}
		for _, item := range mustList(t, s, "members") {
			document := mustGet(t, s, "members", item.ID)
			if item.Version != document.Version || !bytes.Equal(item.Data, document.Data) {
				t.Errorf("List %q = %q v%d, Get = %q v%d", item.ID, item.Data, item.Version, document.Data, document.Version)
			}
			var index int
			fmt.Sscanf(item.ID, "m%d", &index)
			if item.Version != int64(index+1) {
				t.Errorf("List %q v%d, want v%d", item.ID, item.Version, index+1)
			}
		}
	})
}

// TestListExcludesSubcollections checks a collection lists only its own documents.
func TestListExcludesSubcollections(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		mustPut(t, s, "weeks", "own", []byte("own"), 0)
		mustPut(t, s, "weeks/2026-10-19", "nested", []byte("nested"), 0)
		mustPut(t, s, "weeks/2026-10-19/deeper", "deepest", []byte("deepest"), 0)
		items := mustList(t, s, "weeks")
		if len(items) != 1 || items[0].ID != "own" {
			t.Fatalf("List weeks = %+v, want only own", items)
		}
		items = mustList(t, s, "weeks/2026-10-19")
		if len(items) != 1 || items[0].ID != "nested" {
			t.Fatalf("List weeks/2026-10-19 = %+v, want only nested", items)
		}
	})
}

// TestCollectionsListsDirectChildrenSorted checks Collections returns only direct children in order.
func TestCollectionsListsDirectChildrenSorted(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		for _, collection := range []string{"routes/zed", "routes/alpha/deep", "routes/Beta", "routes/alpha", "routes/_x_is_fine_nested", "routesx/other"} {
			mustPut(t, s, collection, "doc", []byte("{}"), 0)
		}
		got := mustCollections(t, s, "routes/")
		want := []string{"routes/Beta", "routes/_x_is_fine_nested", "routes/alpha", "routes/zed"}
		// PROBE: "routes/_x_is_fine_nested" is only present if a nested segment may begin with "_".
		if !slices.Equal(got, want) {
			t.Fatalf("Collections(routes/) = %q, want %q", got, want)
		}
		if got := mustCollections(t, s, "routes/alpha/"); !slices.Equal(got, []string{"routes/alpha/deep"}) {
			t.Fatalf("Collections(routes/alpha/) = %q", got)
		}
		if got := mustCollections(t, s, "routes/zed/"); len(got) != 0 {
			t.Fatalf("Collections(routes/zed/) = %q, want none", got)
		}
	})
}

// TestCollectionsIncludesIntermediateCollections checks a parent with no documents of its own still lists.
func TestCollectionsIncludesIntermediateCollections(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		mustPut(t, s, "routes/member/2026-10-19", "r", []byte("{}"), 0)
		if got := mustCollections(t, s, "routes/"); !slices.Equal(got, []string{"routes/member"}) {
			t.Fatalf("Collections(routes/) = %q", got)
		}
		if got := mustCollections(t, s, "routes/member/"); !slices.Equal(got, []string{"routes/member/2026-10-19"}) {
			t.Fatalf("Collections(routes/member/) = %q", got)
		}
	})
}

// TestCollectionsRejectsBadPrefix checks Collections refuses what ValidatePrefix refuses.
func TestCollectionsRejectsBadPrefix(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		mustPut(t, s, "weeks/2026", "d", []byte("{}"), 0)
		for _, prefix := range []string{"", "/", "weeks", "weeks//", "/weeks/", "../", "weeks/../", "_x/", "we eks/"} {
			validateErr := store.ValidatePrefix(prefix)
			_, err := s.Collections(context.Background(), prefix)
			if validateErr != nil && err == nil {
				t.Errorf("Collections(%q) succeeded although ValidatePrefix refuses it", prefix)
			}
			if err == nil {
				t.Errorf("Collections(%q) succeeded, want error", prefix)
			}
		}
	})
}

// TestCollectionsAfterDeleteCollection checks a deleted collection no longer lists.
func TestCollectionsAfterDeleteCollection(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		mustPut(t, s, "weeks/a", "d", []byte("{}"), 0)
		mustPut(t, s, "weeks/b", "d", []byte("{}"), 0)
		if err := s.DeleteCollection(context.Background(), "weeks/a"); err != nil {
			t.Fatal(err)
		}
		if got := mustCollections(t, s, "weeks/"); !slices.Equal(got, []string{"weeks/b"}) {
			t.Fatalf("Collections after delete = %q", got)
		}
		if got := mustCollections(t, s, "weeks/a/"); len(got) != 0 {
			t.Fatalf("Collections beneath the deleted collection = %q", got)
		}
	})
}

// TestCollectionsAfterLastDocumentDeleted records whether an emptied collection still lists.
func TestCollectionsAfterLastDocumentDeleted(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		mustPut(t, s, "weeks/a", "d", []byte("{}"), 0)
		mustPut(t, s, "weeks/b", "d", []byte("{}"), 0)
		if err := s.Delete(context.Background(), "weeks/a", "d", store.AnyVersion); err != nil {
			t.Fatal(err)
		}
		// PROBE: the contract does not say whether an emptied collection keeps listing.
		got := mustCollections(t, s, "weeks/")
		t.Logf("Collections after the last document was deleted: %q", got)
		if !slices.Contains(got, "weeks/b") {
			t.Fatalf("Collections lost the sibling: %q", got)
		}
		if items := mustList(t, s, "weeks/a"); len(items) != 0 {
			t.Fatalf("emptied collection still lists %+v", items)
		}
	})
}

// TestDeleteCollectionRemovesEverythingBeneath checks nested collections and documents go with it.
func TestDeleteCollectionRemovesEverythingBeneath(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		for _, collection := range []string{"p", "p/a", "p/a/b", "p/c"} {
			for _, id := range []string{"x", "y"} {
				mustPut(t, s, collection, id, []byte(collection+id), 0)
			}
		}
		if err := s.DeleteCollection(context.Background(), "p"); err != nil {
			t.Fatal(err)
		}
		for _, collection := range []string{"p", "p/a", "p/a/b", "p/c"} {
			if items := mustList(t, s, collection); len(items) != 0 {
				t.Errorf("List(%q) after DeleteCollection = %+v", collection, items)
			}
			if _, err := s.Get(context.Background(), collection, "x"); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("Get(%q, x) after DeleteCollection: %v", collection, err)
			}
			if got := mustCollections(t, s, collection+"/"); len(got) != 0 {
				t.Errorf("Collections(%q/) after DeleteCollection = %q", collection, got)
			}
		}
		if _, err := s.Put(context.Background(), "p/a", "x", []byte("again"), 0); err != nil {
			t.Fatalf("re-creating after DeleteCollection: %v", err)
		}
	})
}

// TestDeleteCollectionMissingIsNil checks deleting a collection that never existed is not an error.
func TestDeleteCollectionMissingIsNil(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		if err := s.DeleteCollection(context.Background(), "never/here"); err != nil {
			t.Fatalf("DeleteCollection missing: %v", err)
		}
		mustPut(t, s, "p", "x", []byte("{}"), 0)
		if err := s.DeleteCollection(context.Background(), "p"); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteCollection(context.Background(), "p"); err != nil {
			t.Fatalf("DeleteCollection twice: %v", err)
		}
	})
}

// TestDeleteCollectionSparesSiblingWithSamePrefix checks "weeks" does not take "weeks2" with it.
func TestDeleteCollectionSparesSiblingWithSamePrefix(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		mustPut(t, s, "top/weeks", "d", []byte("weeks"), 0)
		mustPut(t, s, "top/weeks/2026", "d", []byte("nested"), 0)
		mustPut(t, s, "top/weeks2", "d", []byte("weeks2"), 0)
		mustPut(t, s, "top/weeks2/2026", "d", []byte("nested2"), 0)
		mustPut(t, s, "top/week", "d", []byte("week"), 0)
		if err := s.DeleteCollection(context.Background(), "top/weeks"); err != nil {
			t.Fatal(err)
		}
		for _, survivor := range []string{"top/weeks2", "top/weeks2/2026", "top/week"} {
			if _, err := s.Get(context.Background(), survivor, "d"); err != nil {
				t.Errorf("Get(%q) after deleting top/weeks: %v", survivor, err)
			}
		}
		if got := mustCollections(t, s, "top/"); !slices.Equal(got, []string{"top/week", "top/weeks2"}) {
			t.Fatalf("Collections(top/) = %q", got)
		}
		if got := mustCollections(t, s, "top/weeks2/"); !slices.Equal(got, []string{"top/weeks2/2026"}) {
			t.Fatalf("Collections(top/weeks2/) = %q", got)
		}
	})
}

// TestDeleteDocumentSparesSubcollectionOfSameName checks Delete("a","b") keeps collection "a/b".
func TestDeleteDocumentSparesSubcollectionOfSameName(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		mustPut(t, s, "a/b", "c", []byte("nested"), 0)
		if _, err := s.Put(context.Background(), "a", "b", []byte("doc"), 0); err != nil {
			t.Skipf("document id colliding with a subcollection name is refused: %v", err)
		}
		if err := s.Delete(context.Background(), "a", "b", store.AnyVersion); err != nil {
			t.Fatal(err)
		}
		if document, err := s.Get(context.Background(), "a/b", "c"); err != nil || string(document.Data) != "nested" {
			t.Fatalf("nested document after deleting same-named document: %q, %v", document.Data, err)
		}
	})
}

// TestDeleteCollectionSparesDocumentOfSameName checks DeleteCollection("a/b") keeps document "a"/"b".
func TestDeleteCollectionSparesDocumentOfSameName(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		mustPut(t, s, "a", "b", []byte("doc"), 0)
		if _, err := s.Put(context.Background(), "a/b", "c", []byte("nested"), 0); err != nil {
			t.Skipf("subcollection named like an existing document is refused: %v", err)
		}
		if err := s.DeleteCollection(context.Background(), "a/b"); err != nil {
			t.Fatal(err)
		}
		if document, err := s.Get(context.Background(), "a", "b"); err != nil || string(document.Data) != "doc" {
			t.Fatalf("document after deleting same-named collection: %q, %v", document.Data, err)
		}
	})
}

// TestIDAndSubcollectionShareAName checks a document and a collection of the same name never corrupt each other.
func TestIDAndSubcollectionShareAName(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		for _, order := range []string{"document first", "collection first"} {
			t.Run(order, func(t *testing.T) {
				collection := strings.ReplaceAll(order, " ", "-")
				var documentErr, nestedErr error
				if order == "document first" {
					_, documentErr = s.Put(context.Background(), collection, "b", []byte("doc"), 0)
					_, nestedErr = s.Put(context.Background(), collection+"/b", "c", []byte("nested"), 0)
				} else {
					_, nestedErr = s.Put(context.Background(), collection+"/b", "c", []byte("nested"), 0)
					_, documentErr = s.Put(context.Background(), collection, "b", []byte("doc"), 0)
				}
				t.Logf("document: %v; nested: %v", documentErr, nestedErr)
				if documentErr == nil {
					if document := mustGet(t, s, collection, "b"); string(document.Data) != "doc" {
						t.Fatalf("document read back %q", document.Data)
					}
					if items := mustList(t, s, collection); len(items) != 1 || items[0].ID != "b" {
						t.Fatalf("List = %+v", items)
					}
				}
				if nestedErr == nil {
					if document := mustGet(t, s, collection+"/b", "c"); string(document.Data) != "nested" {
						t.Fatalf("nested read back %q", document.Data)
					}
					if got := mustCollections(t, s, collection+"/"); !slices.Equal(got, []string{collection + "/b"}) {
						t.Fatalf("Collections = %q", got)
					}
				}
				if documentErr != nil && nestedErr != nil {
					t.Fatalf("both writes failed: %v; %v", documentErr, nestedErr)
				}
			})
		}
	})
}

// TestConcurrentMixedOperations hammers the same and different ids from many goroutines.
func TestConcurrentMixedOperations(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		var group sync.WaitGroup
		errs := make(chan error, 20*40)
		for g := 0; g < 20; g++ {
			group.Add(1)
			go func(g int) {
				defer group.Done()
				for i := 0; i < 40; i++ {
					id := fmt.Sprintf("id%d", (g+i)%5)
					data := []byte(fmt.Sprintf("g%d-i%d", g, i))
					var err error
					switch i % 4 {
					case 0:
						_, err = s.Put(ctx, "hammer", id, data, store.AnyVersion)
					case 1:
						_, err = s.Get(ctx, "hammer", id)
					case 2:
						err = s.Delete(ctx, "hammer", id, store.AnyVersion)
					case 3:
						_, err = s.List(ctx, "hammer")
					}
					if err != nil && !errors.Is(err, store.ErrConflict) && !errors.Is(err, store.ErrNotFound) {
						errs <- fmt.Errorf("goroutine %d op %d: %w", g, i, err)
					}
				}
			}(g)
		}
		group.Wait()
		close(errs)
		for err := range errs {
			t.Error(err)
		}
		for _, item := range mustList(t, s, "hammer") {
			document := mustGet(t, s, "hammer", item.ID)
			if !bytes.Equal(document.Data, item.Data) || document.Version != item.Version {
				t.Errorf("List and Get disagree on %q", item.ID)
			}
		}
	})
}

// TestConcurrentCreatesExactlyOneWinner checks only one of many Put-0 racers creates the document.
func TestConcurrentCreatesExactlyOneWinner(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		var group sync.WaitGroup
		wins := make(chan int, 32)
		for g := 0; g < 32; g++ {
			group.Add(1)
			go func(g int) {
				defer group.Done()
				version, err := s.Put(context.Background(), "race", "one", []byte(fmt.Sprint(g)), 0)
				if err == nil {
					if version != 1 {
						t.Errorf("winner %d got version %d", g, version)
					}
					wins <- g
				} else if !errors.Is(err, store.ErrConflict) {
					t.Errorf("racer %d: %v", g, err)
				}
			}(g)
		}
		group.Wait()
		close(wins)
		var winners []int
		for g := range wins {
			winners = append(winners, g)
		}
		if len(winners) != 1 {
			t.Fatalf("%d racers created the document: %v", len(winners), winners)
		}
		if document := mustGet(t, s, "race", "one"); string(document.Data) != fmt.Sprint(winners[0]) {
			t.Fatalf("document holds %q, winner was %d", document.Data, winners[0])
		}
	})
}

// TestOperationsAfterClose records how a closed store behaves.
func TestOperationsAfterClose(t *testing.T) {
	root := t.TempDir()
	directory, err := store.OpenDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	mustPut(t, directory, "members", "m", []byte("x"), 0)
	if err := directory.Close(); err != nil {
		t.Fatal(err)
	}
	// PROBE: the contract does not say whether a closed store refuses or serves; it must not panic.
	_, err = directory.Get(context.Background(), "members", "m")
	t.Logf("Get after Close: %v", err)
	if err := directory.Close(); err != nil {
		t.Logf("second Close: %v", err)
	}
}
