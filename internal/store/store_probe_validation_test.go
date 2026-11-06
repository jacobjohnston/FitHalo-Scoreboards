package store_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/store"
)

// segmentPattern is the documented shape of every collection segment and identifier.
var segmentPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// segment64 is a segment at the documented maximum length.
var segment64 = strings.Repeat("a", 64)

// segment65 is a segment one over the documented maximum length.
var segment65 = strings.Repeat("a", 65)

// validationCase is a name and whether the contract accepts it.
type validationCase struct {
	// name labels the case.
	name string

	// value is the collection, identifier or prefix under test.
	value string

	// valid is true when the contract accepts value.
	valid bool
}

// collectionCases lists collection paths the contract decides unambiguously.
var collectionCases = []validationCase{
	{"single segment", "weeks", true},
	{"two segments", "weeks/2026-10-19", true},
	{"five segments", "a/b/c/d/e", true},
	{"all allowed characters", "AZaz09_-", true},
	{"segment at 64", segment64, true},
	{"nested segment at 64", "a/" + segment64, true},
	{"underscore inside", "a_b/c_d", true},
	{"hyphen edges", "-a-/-b-", true},
	{"empty", "", false},
	{"slash only", "/", false},
	{"trailing slash", "a/", false},
	{"leading slash", "/a", false},
	{"double slash", "a//b", false},
	{"dot", ".", false},
	{"dot dot", "..", false},
	{"traversal inside", "a/../b", false},
	{"traversal at end", "a/..", false},
	{"dot segment", "a/./b", false},
	{"space", "a b", false},
	{"segment at 65", segment65, false},
	{"nested segment at 65", "a/" + segment65, false},
	{"reserved first segment", "_x", false},
	{"reserved collections", "_collections", false},
	{"reserved underscore only", "_", false},
	{"unicode letter", "café", false},
	{"fullwidth slash", "a／b", false},
	{"nul byte", "a\x00b", false},
	{"nul only", "\x00", false},
	{"backslash", `a\b`, false},
	{"period in segment", "a.b", false},
	{"percent encoded slash", "a%2Fb", false},
	{"tab", "a\tb", false},
	{"newline", "a\nb", false},
	{"colon", "a:b", false},
	{"tilde", "~", false},
	{"zero width space", "a​b", false},
	{"combining mark", "á", false},
}

// TestValidateCollection holds ValidateCollection to its documented rule.
func TestValidateCollection(t *testing.T) {
	for _, test := range collectionCases {
		t.Run(test.name, func(t *testing.T) {
			err := store.ValidateCollection(test.value)
			if test.valid && err != nil {
				t.Fatalf("ValidateCollection(%q) = %v, want nil", test.value, err)
			}
			if !test.valid && err == nil {
				t.Fatalf("ValidateCollection(%q) = nil, want error", test.value)
			}
		})
	}
}

// TestValidateCollectionNestedReservedSegment records whether "_" is reserved below the first segment.
func TestValidateCollectionNestedReservedSegment(t *testing.T) {
	// PROBE: the doc says a path that "begins with reservedPrefix" is refused, which reads as the first
	// segment only; the security review says the same. A nested "_b" is therefore accepted by the letter.
	for _, value := range []string{"a/_b", "a/_collections", "a/b/_c"} {
		err := store.ValidateCollection(value)
		t.Logf("ValidateCollection(%q) = %v", value, err)
		if err != nil {
			t.Errorf("ValidateCollection(%q) = %v; the contract reserves only a leading segment", value, err)
		}
	}
}

// idCases lists identifiers the contract decides unambiguously.
var idCases = []validationCase{
	{"letter", "a", true},
	{"digit", "0", true},
	{"hyphen", "-", true},
	{"underscore", "_", true},
	{"leading underscore", "_x", true},
	{"all allowed characters", "AZaz09_-", true},
	{"at 64", segment64, true},
	{"empty", "", false},
	{"dot", ".", false},
	{"dot dot", "..", false},
	{"slash inside", "a/b", false},
	{"leading slash", "/a", false},
	{"trailing slash", "a/", false},
	{"at 65", segment65, false},
	{"space", "a b", false},
	{"nul", "\x00", false},
	{"unicode", "é", false},
	{"period inside", "a.json", false},
	{"backslash", `a\b`, false},
	{"plus", "a+b", false},
	{"equals", "a=", false},
}

// TestValidateID holds ValidateID to its documented rule.
func TestValidateID(t *testing.T) {
	for _, test := range idCases {
		t.Run(test.name, func(t *testing.T) {
			err := store.ValidateID(test.value)
			if test.valid && err != nil {
				t.Fatalf("ValidateID(%q) = %v, want nil", test.value, err)
			}
			if !test.valid && err == nil {
				t.Fatalf("ValidateID(%q) = nil, want error", test.value)
			}
		})
	}
}

// prefixCases lists prefixes the contract decides unambiguously.
var prefixCases = []validationCase{
	{"single", "weeks/", true},
	{"nested", "routes/member/", true},
	{"at 64", segment64 + "/", true},
	{"no slash", "weeks", false},
	{"empty", "", false},
	{"slash only", "/", false},
	{"double trailing slash", "weeks//", false},
	{"leading slash", "/weeks/", false},
	{"reserved", "_x/", false},
	{"traversal", "../", false},
	{"traversal nested", "weeks/../", false},
	{"space", "we eks/", false},
	{"at 65", segment65 + "/", false},
	{"backslash", `weeks\`, false},
}

// TestValidatePrefix holds ValidatePrefix to its documented rule.
func TestValidatePrefix(t *testing.T) {
	for _, test := range prefixCases {
		t.Run(test.name, func(t *testing.T) {
			err := store.ValidatePrefix(test.value)
			if test.valid && err != nil {
				t.Fatalf("ValidatePrefix(%q) = %v, want nil", test.value, err)
			}
			if !test.valid && err == nil {
				t.Fatalf("ValidatePrefix(%q) = nil, want error", test.value)
			}
		})
	}
}

// TestValidatorsAgreeWithEachOther checks the three validators are consistent on shared inputs.
func TestValidatorsAgreeWithEachOther(t *testing.T) {
	for _, test := range append(append([]validationCase{}, collectionCases...), idCases...) {
		collectionErr := store.ValidateCollection(test.value)
		prefixErr := store.ValidatePrefix(test.value + "/")
		if (collectionErr == nil) != (prefixErr == nil) {
			t.Errorf("ValidateCollection(%q) = %v but ValidatePrefix(%q) = %v", test.value, collectionErr, test.value+"/", prefixErr)
		}
		if !strings.Contains(test.value, "/") && !strings.HasPrefix(test.value, "_") {
			idErr := store.ValidateID(test.value)
			if (collectionErr == nil) != (idErr == nil) {
				t.Errorf("ValidateCollection(%q) = %v but ValidateID = %v", test.value, collectionErr, idErr)
			}
		}
	}
}

// FuzzValidateCollection checks ValidateCollection against the documented pattern on arbitrary input.
func FuzzValidateCollection(f *testing.F) {
	for _, test := range collectionCases {
		f.Add(test.value)
	}
	f.Add("a/_b")
	f.Add("weeks/2026-10-19/" + segment64)
	f.Fuzz(func(t *testing.T, value string) {
		err := store.ValidateCollection(value)
		segments := strings.Split(value, "/")
		expected := value != ""
		for _, segment := range segments {
			if !segmentPattern.MatchString(segment) {
				expected = false
			}
		}
		if strings.HasPrefix(value, "_") {
			expected = false
		}
		nestedReserved := false
		for _, segment := range segments[min(1, len(segments)):] {
			if strings.HasPrefix(segment, "_") {
				nestedReserved = true
			}
		}
		if expected && !nestedReserved && err != nil {
			t.Fatalf("ValidateCollection(%q) = %v, but every segment matches %s", value, err, segmentPattern)
		}
		if !expected && err == nil {
			t.Fatalf("ValidateCollection(%q) = nil, but it breaks %s", value, segmentPattern)
		}
		if err == nil {
			if prefixErr := store.ValidatePrefix(value + "/"); prefixErr != nil {
				t.Fatalf("ValidateCollection(%q) = nil but ValidatePrefix = %v", value, prefixErr)
			}
			for _, segment := range segments {
				if idErr := store.ValidateID(segment); idErr != nil {
					t.Fatalf("ValidateCollection(%q) = nil but ValidateID(%q) = %v", value, segment, idErr)
				}
			}
		}
	})
}

// FuzzValidateID checks ValidateID against the documented pattern on arbitrary input.
func FuzzValidateID(f *testing.F) {
	for _, test := range idCases {
		f.Add(test.value)
	}
	f.Fuzz(func(t *testing.T, value string) {
		err := store.ValidateID(value)
		if expected := segmentPattern.MatchString(value); expected != (err == nil) {
			t.Fatalf("ValidateID(%q) = %v, pattern says valid=%v", value, err, expected)
		}
	})
}

// hostileNames lists collection and identifier pairs that must never reach the filesystem.
var hostileNames = []struct {
	// name labels the case.
	name string

	// collection is the collection argument.
	collection string

	// id is the identifier argument.
	id string
}{
	{"traversal collection", "../../etc", "passwd"},
	{"traversal id", "members", "../escape"},
	{"dot dot id", "members", ".."},
	{"absolute collection", "/etc", "passwd"},
	{"escape via sibling", "../escape", "x"},
	{"reserved collection", "_collections", "x"},
	{"nul in collection", "mem\x00bers", "x"},
	{"nul in id", "members", "x\x00"},
	{"slash in id", "members", "a/b"},
	{"empty collection", "", "x"},
	{"empty id", "members", ""},
	{"space", "mem bers", "x"},
	{"overlong segment", segment65, "x"},
	{"overlong id", "members", segment65},
	{"backslash id", "members", `..\..\x`},
	{"url encoded traversal", "%2e%2e", "x"},
}

// TestStoreMethodsRefuseHostileNames checks every method errors on an invalid name and touches no path outside root.
func TestStoreMethodsRefuseHostileNames(t *testing.T) {
	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			parent := t.TempDir()
			root := filepath.Join(parent, "root")
			directory, err := store.OpenDirectory(root)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { directory.Close() })
			var s store.Store = directory
			if b.name == "compressed" {
				s = store.Compress(directory)
			}
			ctx := context.Background()
			mustPut(t, s, "keep", "present", []byte("{}"), 0)
			for _, test := range hostileNames {
				t.Run(test.name, func(t *testing.T) {
					if _, err := s.Get(ctx, test.collection, test.id); err == nil {
						t.Errorf("Get(%q, %q) succeeded", test.collection, test.id)
					}
					if _, err := s.Put(ctx, test.collection, test.id, []byte("evil"), store.AnyVersion); err == nil {
						t.Errorf("Put(%q, %q) succeeded", test.collection, test.id)
					}
					if err := s.Delete(ctx, test.collection, test.id, store.AnyVersion); err == nil {
						t.Errorf("Delete(%q, %q) succeeded", test.collection, test.id)
					}
					if store.ValidateCollection(test.collection) == nil {
						return
					}
					if _, err := s.List(ctx, test.collection); err == nil {
						t.Errorf("List(%q) succeeded", test.collection)
					}
					if err := s.DeleteCollection(ctx, test.collection); err == nil {
						t.Errorf("DeleteCollection(%q) succeeded", test.collection)
					}
					if _, err := s.Collections(ctx, test.collection+"/"); err == nil {
						t.Errorf("Collections(%q/) succeeded", test.collection)
					}
				})
			}
			entries, err := os.ReadDir(parent)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].Name() != "root" {
				var names []string
				for _, entry := range entries {
					names = append(names, entry.Name())
				}
				t.Fatalf("files appeared beside root: %q", names)
			}
			if document := mustGet(t, s, "keep", "present"); string(document.Data) != "{}" {
				t.Fatalf("the present document changed: %q", document.Data)
			}
		})
	}
}

// TestHostileNamesAreNotReportedAsMissing checks a refused name is a validation error, not ErrNotFound.
func TestHostileNamesAreNotReportedAsMissing(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		// PROBE: the contract does not name the error for an invalid name; it should not masquerade as
		// ErrNotFound or ErrConflict, which callers map to 404 and 409.
		for _, test := range hostileNames {
			_, err := s.Get(context.Background(), test.collection, test.id)
			if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrConflict) {
				t.Errorf("Get(%q, %q) = %v, a document error for an invalid name", test.collection, test.id, err)
			}
		}
	})
}

// TestReservedIdentifiersAreOrdinaryDocuments checks ids beginning with "_" store and list like any other.
func TestReservedIdentifiersAreOrdinaryDocuments(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		for _, id := range []string{"_collections", "_", "_index", "__"} {
			if err := store.ValidateID(id); err != nil {
				t.Fatalf("ValidateID(%q) = %v, but the rule is only the character set", id, err)
			}
			mustPut(t, s, "members", id, []byte(id), 0)
		}
		mustPut(t, s, "members/_nested", "x", []byte("nested"), 0)
		items := mustList(t, s, "members")
		if len(items) != 4 {
			t.Fatalf("List = %+v, want four documents", items)
		}
		for _, item := range items {
			if string(item.Data) != item.ID {
				t.Errorf("item %q holds %q", item.ID, item.Data)
			}
		}
	})
}

// TestDeepCollectionPathsFailCleanlyOrWork checks a very deep path never corrupts the store.
func TestDeepCollectionPathsFailCleanlyOrWork(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		// PROBE: the contract bounds a segment at 64 characters but not the number of segments.
		deep := strings.TrimSuffix(strings.Repeat(segment64+"/", 20), "/")
		if err := store.ValidateCollection(deep); err != nil {
			t.Fatalf("ValidateCollection on 20 segments of 64: %v", err)
		}
		_, err := s.Put(context.Background(), deep, "d", []byte("deep"), 0)
		t.Logf("Put into a 1299-character collection path: %v", err)
		if err == nil {
			if document := mustGet(t, s, deep, "d"); string(document.Data) != "deep" {
				t.Fatalf("deep document read back %q", document.Data)
			}
		}
		mustPut(t, s, "shallow", "d", []byte("ok"), 0)
		if document := mustGet(t, s, "shallow", "d"); string(document.Data) != "ok" {
			t.Fatalf("shallow document read back %q", document.Data)
		}
	})
}
