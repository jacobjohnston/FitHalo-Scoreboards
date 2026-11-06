package store_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/store"
)

// jsonShapedCases lists byte strings that happen to parse as JSON and must still round-trip verbatim.
var jsonShapedCases = []roundTripCase{
	{"fuzz crasher space zero", []byte(" 0")},
	{"leading newline", []byte("\n0")},
	{"trailing newline", []byte(`{"a":1}` + "\n")},
	{"trailing space", []byte("0 ")},
	{"object with spaces", []byte(`{"a": 1}`)},
	{"array with spaces", []byte(`[1, 2]`)},
	{"tab indented", []byte("{\n\t\"a\": 1\n}")},
	{"key order", []byte(`{"b":1,"a":2}`)},
	{"duplicate keys", []byte(`{"a":1,"a":2}`)},
	{"float one point zero", []byte(`1.0`)},
	{"exponent", []byte(`1e2`)},
	{"capital exponent plus", []byte(`1E+2`)},
	{"negative zero", []byte(`-0`)},
	{"trailing fraction zero", []byte(`0.10`)},
	{"big integer", []byte(`12345678901234567890`)},
	{"precise float", []byte(`0.1000000000000000055511151231257827`)},
	{"escaped unicode", []byte(`"é"`)},
	{"raw unicode", []byte(`"é"`)},
	{"html characters", []byte(`"<>&"`)},
	{"escaped slash", []byte(`"\/"`)},
	{"surrogate escapes", []byte(`"😀"`)},
	{"lone surrogate escape", []byte(`"\ud83d"`)},
	{"null", []byte(`null`)},
	{"true", []byte(`true`)},
	{"empty string", []byte(`""`)},
	{"empty object", []byte(`{}`)},
	{"nested spaces", []byte(`{"a":{"b":[1, {"c": null}]}}`)},
	{"deep arrays", []byte(strings.Repeat("[", 500) + strings.Repeat("]", 500))},
	{"string with escaped nul", []byte(`"\u0000"`)},
	{"number then space then number", []byte(`1 2`)},
	{"bom then object", []byte("\xef\xbb\xbf{}")},
}

// TestJSONShapedDocumentsRoundTripVerbatim checks a document that parses as JSON is still stored byte for byte.
func TestJSONShapedDocumentsRoundTripVerbatim(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		for _, test := range jsonShapedCases {
			t.Run(test.name, func(t *testing.T) {
				mustPut(t, s, "json", "j", test.data, store.AnyVersion)
				document := mustGet(t, s, "json", "j")
				if !bytes.Equal(document.Data, test.data) {
					t.Fatalf("wrote %q, read back %q", test.data, document.Data)
				}
				items := mustList(t, s, "json")
				if len(items) != 1 || !bytes.Equal(items[0].Data, test.data) {
					t.Fatalf("wrote %q, List read back %q", test.data, items[0].Data)
				}
			})
		}
	})
}

// TestJSONShapedDocumentVersionStillAdvances checks an altered-on-disk document still versions correctly.
func TestJSONShapedDocumentVersionStillAdvances(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		first := mustPut(t, s, "json", "j", []byte(`{"a": 1}`), 0)
		second := mustPut(t, s, "json", "j", []byte(`{"a": 2}`), first)
		if first != 1 || second != 2 {
			t.Fatalf("versions %d, %d", first, second)
		}
	})
}
