// Package fixture checks that documents an earlier release stored still decode, for tests only.
package fixture

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"testing"
)

// Read returns a fixture file's bytes, failing the test when it cannot.
func Read(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// ExpectRoundTrip decodes data into value with no field left unknown, and encodes it back with every
// field data had, so a renamed, removed or retyped field fails the test while an added one does not.
func ExpectRoundTrip(t *testing.T, data []byte, value any) {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		t.Fatal("data after the document")
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	ExpectContains(t, data, encoded)
}

// ExpectContains fails the test unless the JSON got holds every member and element of want, with equal
// values.
func ExpectContains(t *testing.T, want, got []byte) {
	t.Helper()
	var wantValue, gotValue any
	if err := json.Unmarshal(want, &wantValue); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatal(err)
	}
	for _, difference := range contains("", wantValue, gotValue) {
		t.Error(difference)
	}
}

// contains lists where got lacks a member or element of want, or holds a different value.
func contains(path string, want, got any) []string {
	switch want := want.(type) {
	case map[string]any:
		object, isObject := got.(map[string]any)
		if !isObject {
			return []string{fmt.Sprintf("%s: got %v, want an object", path, got)}
		}
		var differences []string
		for key, value := range want {
			member, found := object[key]
			if !found {
				differences = append(differences, path+"."+key+" is missing")
				continue
			}
			differences = append(differences, contains(path+"."+key, value, member)...)
		}
		return differences
	case []any:
		array, isArray := got.([]any)
		if !isArray || len(array) != len(want) {
			return []string{fmt.Sprintf("%s: got %v, want %d elements", path, got, len(want))}
		}
		var differences []string
		for index := range want {
			differences = append(differences, contains(fmt.Sprintf("%s[%d]", path, index), want[index], array[index])...)
		}
		return differences
	default:
		if !reflect.DeepEqual(want, got) {
			return []string{fmt.Sprintf("%s: got %v, want %v", path, got, want)}
		}
		return nil
	}
}
